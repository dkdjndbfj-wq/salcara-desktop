package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

// codexAgent drives one long-lived `codex app-server` process for the whole bridge.
type codexAgent struct {
	exeOverride   string
	stateFile     string // remembers which threads the bridge started (for SessionInfo.Client)
	sink          func(protocol.Event)
	settings      func() Settings
	thr           *throttle
	aps           *approvals
	sideQuestions sideQuestionRegistry
	sideTools     codexSideToolsRegistry
	privateHome   string // private side-tool worker only; never inherited from phone input

	startMu               sync.Mutex // serializes process start
	mu                    sync.Mutex
	conn                  *rpcConn
	starting              *codexStartupLease // owns initialization before conn is published
	unconfirmedProcess    *rpcConn           // retired transport is not proof the owned OS worker stopped
	connSig               string
	startedAt             time.Time
	nextStart             time.Time
	backoff               time.Duration
	sessionActionsVersion uint64 // fences directory replies across native management writes
	threads               map[string]*codexThread
	owned                 map[string]bool
	turnReverts           map[string][]*codexTurnRevertReceipt
	revertStoreError      error
	closed                bool
	closeCh               chan struct{}
	wg                    sync.WaitGroup
}

type codexThread struct {
	controlMu              sync.Mutex // serializes turn admission/terminal CAS, never taken by the reader
	context                codexContextState
	sessionActions         codexSessionActionState
	workflows              codexWorkflowState
	continuedTerminal      string
	steerTurn              string // controlMu owns the bounded same-turn receipt cache
	steerReceipts          map[string]codexSteerReceipt
	id                     string
	info                   protocol.SessionInfo
	path                   string
	loaded                 bool              // started/resumed on our app-server
	idleRestartRollout     *codexIdleRollout // process-local proof for an unchanged, cleanly retired idle thread
	historyMode            string
	ephemeral              bool
	revertPending          bool
	policy                 string
	permissionUnconfirmed  bool // partial/ambiguous runtime settings acknowledgement
	inheritedFullAccess    bool // observed dangerous sandbox, even without a recognized reviewer tuple
	legacyPolicyConfigured bool // local auto-answer policy alone is not proof of a chosen host sandbox
	turnID                 string
	turnVersion            uint64 // prevents a late turn/start reply from reviving a completed/newer turn
	statusUnconfirmed      bool   // an explicit future/malformed ThreadStatus is not permission to resume
	baseline               *int64 // total tokens (in,out) at turn start
	baseOut                int64
	usage                  protocol.Usage
	items                  map[string]*cxLive
	opened                 bool                // phone opened it: stream new history items while it runs elsewhere
	seen                   map[string]bool     // history item keys already sent (external threads)
	tailHistory            bool                // opened through bounded official turn history
	tailBaselineTurn       string              // newest turn known when opened/refreshed
	historyItemDigests     map[string][32]byte // bounded external-history item snapshots, including updates
	historyTurnDigests     map[string][32]byte // explicitly observed turn states, not inferred completions
	historyReadVersion     uint64              // prevents a late history read from reverting a newer snapshot
	childStates            map[string]string   // latest native collab state; a completed parent turn can leave a child running
	childOverflow          bool                // fail closed if native child activity exceeds the bounded tracker
	pendingAttachments     [][]protocol.AttachmentRef
	messageAttachments     map[string][]protocol.AttachmentRef
}

// cxLive accumulates streaming state for one item.
type cxLive struct {
	messagePhase string
	text         string
	summary      []string
	content      []string
	output       string
	changes      []cxChange
	lastEvent    protocol.Event
}

func newCodexAgent(sink func(protocol.Event), settings func() Settings, exe, stateFile string) *codexAgent {
	a := &codexAgent{exeOverride: exe, stateFile: stateFile, sink: sink, settings: settings,
		aps: newApprovals(), threads: map[string]*codexThread{}, owned: map[string]bool{}, closeCh: make(chan struct{})}
	a.thr = newThrottle(sink)
	a.loadState()
	a.loadTurnReverts()
	return a
}

func (a *codexAgent) ID() string   { return "codex" }
func (a *codexAgent) Name() string { return "Codex" }

func (a *codexAgent) Descriptor() protocol.AgentDescriptor {
	a.mu.Lock()
	revert := a.turnRevertPath() != "" && a.conn != nil && a.conn.supportsAnyTurnRevert() && a.revertStoreError == nil
	a.mu.Unlock()
	return protocol.AgentDescriptor{
		ID: "codex", Name: "Codex", Tool: "codex", APIFamily: "codex", ControlSurface: "cli",
		Capabilities: protocol.AgentCapabilities{
			LatestTurnRevert: revert,
			SideQuestion:     true,
			SessionActions:   true, ArchivedSessions: true,
			SessionWorkflows:       true,
			ContextReferences:      true,
			ContextAnnotations:     true,
			ContextDiffAnnotations: true,
			Context:                true, Compact: true,
			Features: true,
			List:     true, Read: true, Send: true, Interrupt: true, PreciseInterrupt: true, Continue: true, Steer: true, Approval: true,
			Questions: true, Subagents: true, Attachments: true, ImageInput: true,
			ImageOutput: true, ModelOverride: true, PermissionModes: []string{"ask", "auto_review", "full_access"},
		},
	}
}

func (a *codexAgent) exe() (string, bool) {
	override := a.exeOverride
	if override == "" {
		override = a.settings().CodexPath
	}
	p, ok := findExecutable("codex", override)
	if ok {
		if native := nativeCodexExe(p); native != "" {
			p = native
		}
	}
	return p, ok
}

func (a *codexAgent) Detect(ctx context.Context) protocol.Tool {
	t := protocol.Tool{ID: "codex", Name: "Codex"}
	p, ok := a.exe()
	if !ok {
		return t
	}
	v, ok := toolVersion(ctx, p)
	t.Available, t.Version = ok, v
	return t
}

// ---- persistence of bridge-started threads ----

func (a *codexAgent) loadState() {
	if a.stateFile == "" {
		return
	}
	b, err := os.ReadFile(a.stateFile)
	if err != nil {
		return
	}
	var state struct {
		Owned       []string                                       `json:"owned"`
		Attachments map[string]map[string][]protocol.AttachmentRef `json:"attachments"`
	}
	if json.Unmarshal(b, &state) == nil && state.Owned != nil {
		for _, id := range state.Owned {
			a.owned[id] = true
		}
		for thread, items := range state.Attachments {
			t := a.threadLocked(thread)
			t.messageAttachments = items
		}
		return
	}
	// Version 1 stored only a JSON array of owned thread ids.
	var ids []string
	if json.Unmarshal(b, &ids) == nil {
		for _, id := range ids {
			a.owned[id] = true
		}
	}
}

func (a *codexAgent) saveStateLocked() {
	if a.stateFile == "" {
		return
	}
	ids := make([]string, 0, len(a.owned))
	for id := range a.owned {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) > 2000 {
		ids = ids[len(ids)-2000:]
	}
	attachments := map[string]map[string][]protocol.AttachmentRef{}
	for id, thread := range a.threads {
		if len(thread.messageAttachments) == 0 {
			continue
		}
		attachments[id] = thread.messageAttachments
	}
	b, _ := json.Marshal(struct {
		Owned       []string                                       `json:"owned"`
		Attachments map[string]map[string][]protocol.AttachmentRef `json:"attachments,omitempty"`
	}{Owned: ids, Attachments: attachments})
	_ = os.MkdirAll(filepath.Dir(a.stateFile), 0o700)
	tmp := a.stateFile + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, a.stateFile)
	}
}

// ---- process management ----

func tomlString(s string, quoted bool) string {
	if !quoted {
		return s // codex falls back to a literal string when the value isn't valid TOML
	}
	b, _ := json.Marshal(s) // JSON string escapes are valid TOML basic-string escapes
	return string(b)
}

// codexArgs builds `codex app-server` with config overrides that force the relay provider.
func codexArgs(exe string, s Settings) []string {
	args := []string{"app-server"}
	if s.UseOriginalCodex || s.codexRoot() == "" {
		return args
	}
	// Through a .cmd shim cmd.exe re-parses the command line; avoid quotes and spaces there.
	quoted := !strings.EqualFold(filepath.Ext(exe), ".cmd") && !strings.EqualFold(filepath.Ext(exe), ".bat")
	base := strings.TrimRight(s.codexRoot(), "/") + "/v1"
	if s.CodexProvider != "" {
		provider := s.codexProvider()
		args = append(args, "-c", "model_provider="+tomlString(provider, quoted))
		if provider == "openai" {
			// app-server does not load environment API keys as authentication.
			// The known Key is installed over its local RPC after verifying this
			// process-only store; never overwrite the original desktop login.
			return append(args, "-c", "openai_base_url="+tomlString(base, quoted),
				"-c", "cli_auth_credentials_store="+tomlString("ephemeral", quoted),
				"-c", "forced_login_method="+tomlString("api", quoted))
		}
		// Preserve even dotted provider IDs using a quoted inline-table key.
		// Native CLI overrides can merge older authentication fields, so ensure
		// verifies the effective provider before opening or resuming any thread.
		entry := `{name="Salcara",base_url=` + tomlString(base, true) + `,env_key="SUB2API_API_KEY",wire_api="responses",requires_openai_auth=false,supports_websockets=false}`
		return append(args, "-c", "model_providers={"+tomlString(provider, true)+"="+entry+"}")
	}
	kv := [][2]string{
		{"model_provider", tomlString("salcara", quoted)},
		{"model_providers.salcara.name", tomlString("Salcara", quoted)},
		{"model_providers.salcara.base_url", tomlString(base, quoted)},
		{"model_providers.salcara.env_key", tomlString("SUB2API_API_KEY", quoted)},
		{"model_providers.salcara.wire_api", tomlString("responses", quoted)},
		{"model_providers.salcara.requires_openai_auth", "false"},
		{"model_providers.salcara.supports_websockets", "false"},
	}
	for _, p := range kv {
		args = append(args, "-c", p[0]+"="+p[1])
	}
	return args
}

func codexSig(s Settings) string {
	return s.codexRoot() + "\x00" + s.CodexKey + "\x00" + s.codexProvider() + "\x00" + strconv.FormatBool(s.UseOriginalCodex) + "\x00" + s.CodexPath + "\x00" + s.CodexModel
}

func validateCodexWorkerOverrides(exe string, s Settings) error {
	shim := strings.EqualFold(filepath.Ext(exe), ".cmd") || strings.EqualFold(filepath.Ext(exe), ".bat")
	if shim && !s.UseOriginalCodex && s.CodexProvider != "" && s.codexProvider() != "openai" {
		return errors.New("请在电脑工具卡片选择原生 codex.exe；当前脚本启动器不能安全保留供应商配置")
	}
	return nil
}

func (a *codexAgent) hasActiveTurnsLocked() bool {
	for _, t := range a.threads {
		if t.codexSessionActionBusyLocked() {
			return true
		}
		if t.codexWorkflowBlocksMutationLocked() {
			return true
		}
		if t.loaded {
			if t.codexCompactionBusyLocked() {
				return true
			}
			if t.statusUnconfirmed {
				return true
			}
			if t.childOverflow {
				return true
			}
			if t.turnID != "" {
				return true
			}
			for _, status := range t.childStates {
				switch status {
				case "completed", "interrupted", "errored", "shutdown", "notFound":
				default:
					return true
				}
			}
		}
	}
	return false
}

// ensure returns a running, initialized app-server connection, starting (or restarting after a
// settings change) as needed. Restarts after crashes are rate-limited with exponential backoff.
func (a *codexAgent) ensure(ctx context.Context) (*rpcConn, error) {
	return a.ensureConnection(ctx, false)
}

// Background history readers have no execution-source snapshot. Reading the
// shared Codex history must not switch an idle phone worker back to the local
// default API while another request is confirming that worker's metadata.
// Explicitly scoped reads retain the same source checks as execution.
func (a *codexAgent) ensureRead(ctx context.Context) (*rpcConn, error) {
	return a.ensureConnection(ctx, true)
}

func (a *codexAgent) ensureConnection(ctx context.Context, readOnly bool) (*rpcConn, error) {
	a.startMu.Lock()
	defer a.startMu.Unlock()
	s := settingsForContext(ctx, a.settings)
	sig := codexSig(s)
	// A delayed approval/MCP request can outlive turn bookkeeping. Inspect its
	// own registry without nesting its mutex under a.mu; a catalog/read must not
	// restart the process just because a shared API card was edited while that
	// process is still waiting for a phone response.
	pendingApproval := pendingRemoteApprovals(a.aps)

	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil, errors.New("桥接程序正在退出")
	}
	if a.unconfirmedProcess != nil {
		if !a.unconfirmedProcess.processExitConfirmed() {
			a.mu.Unlock()
			return nil, errRPCProcessExitUnconfirmed
		}
		a.unconfirmedProcess = nil
	}
	c := a.conn
	if c != nil && !c.alive() {
		// The exit watcher may not have run yet. Process the confirmed dead
		// connection before a new epoch can replace it and make that watcher stale.
		a.mu.Unlock()
		<-c.done
		a.onExit(c)
		a.mu.Lock()
		if a.unconfirmedProcess != nil && !a.unconfirmedProcess.processExitConfirmed() {
			a.mu.Unlock()
			return nil, errRPCProcessExitUnconfirmed
		}
		if a.closed {
			a.mu.Unlock()
			return nil, errors.New("桥接程序正在退出")
		}
		c = a.conn
	}
	if c != nil && c.alive() {
		if _, frozen := ctx.Value(configSnapshotKey{}).(config.Config); readOnly && !frozen {
			a.mu.Unlock()
			return c, nil
		}
		if a.connSig != sig && pendingApproval {
			a.mu.Unlock()
			return nil, errors.New("Codex 还有等待处理的问题或审批，请处理后再切换 API 配置")
		}
		if _, frozen := ctx.Value(configSnapshotKey{}).(config.Config); frozen && a.connSig != sig && a.hasActiveTurnsLocked() {
			a.mu.Unlock()
			return nil, errors.New("Codex 正在使用另一份 API 配置，请结束当前任务后重试")
		}
		if a.connSig == sig || a.hasActiveTurnsLocked() {
			a.mu.Unlock()
			return c, nil
		}
		// Relay settings changed and nothing is running: restart with the new ones.
		idleRollouts := a.captureIdleCodexRolloutsLocked()
		a.conn = nil
		a.markUnloadedLocked()
		a.mu.Unlock()
		c.Kill()
		<-c.done
		if err := a.fenceUnconfirmedProcess(c); err != nil {
			return nil, err
		}
		a.mu.Lock()
		// Drop any final notifications emitted between unloading and process exit.
		a.markUnloadedLocked()
		a.rememberIdleCodexRolloutsLocked(c, idleRollouts)
	}
	wait := time.Until(a.nextStart)
	if a.closed {
		a.mu.Unlock()
		return nil, errors.New("桥接程序正在退出")
	}
	startup := a.beginStartupLocked(ctx)
	a.mu.Unlock()
	defer a.finishStartup(startup)
	ctx = startup.ctx
	if wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-a.closeCh:
			return nil, errors.New("桥接程序正在退出")
		}
	}

	exe, ok := a.exe()
	if !ok {
		return nil, ErrCodexNotInstalled
	}
	if err := validateCodexWorkerOverrides(exe, s); err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, codexArgs(exe, s)...)
	prepareCmd(cmd)
	env := map[string]string{"SUB2API_API_KEY": s.CodexKey}
	var drop []string
	if !s.UseOriginalCodex && s.codexRoot() == "" {
		env = map[string]string{} // Codex's own login and config
	} else if s.UseOriginalCodex {
		env = map[string]string{"OPENAI_API_KEY": s.CodexKey}
		drop = []string{"OPENAI_BASE_URL", "SUB2API_API_KEY"}
	} else if s.codexProvider() == "openai" {
		env = map[string]string{} // initialized using process-only RPC authentication
		drop = []string{"OPENAI_API_KEY", "OPENAI_BASE_URL", "SUB2API_API_KEY"}
	}
	if needsCodexProviderGuard(s) {
		// Workload identity and access-token overrides can take precedence over
		// the selected API. Isolate only Bridge-managed workers, not own-login.
		drop = append(drop, "CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "OPENAI_FEDERATION_RULE_ID", "OPENAI_IDENTITY_TOKEN_FILE", "OPENAI_WORKLOAD_IDENTITY_CONTEXT")
	}
	if a.privateHome != "" {
		env["CODEX_HOME"] = a.privateHome
		// Never expose the hosting app's tool transport to an independent side worker.
		drop = append(drop, "OPENAI_API_KEY", "OPENAI_BASE_URL", "CODEX_TOOL_PIPE", "CODEX_APP_TOOLS_PIPE_PATH", "CODEX_TOOL_CALL_ID", "CODEX_ELECTRON_RESOURCES_PATH", "CODEX_THREAD_ID", "CODEX_TURN_ID")
	}
	cmd.Env = childEnv(exe, env, drop)
	if home, err := os.UserHomeDir(); err == nil {
		cmd.Dir = home
	}
	c, err := startRPC(cmd, nil, nil)
	if err != nil {
		a.failedStart()
		return nil, errors.New("无法启动 Codex：" + err.Error())
	}
	a.mu.Lock()
	startup.conn = c
	closing := a.closed || ctx.Err() != nil
	a.mu.Unlock()
	if closing {
		c.Kill()
		<-c.done
		_ = a.fenceUnconfirmedProcess(c)
		return nil, errors.New("桥接程序正在退出")
	}
	a.installCodexNotificationHandler(c)
	a.installCodexRequestHandler(c)
	ictx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	init := map[string]any{
		"clientInfo": map[string]any{"name": "salcara_bridge", "title": "Salcara Bridge", "version": "1.0.0"},
		// Needed only for the official read-only turn pager. This does not
		// change approval/sandbox policy or authorize any new mutation.
		"capabilities": map[string]any{"experimentalApi": true, "requestAttestation": false},
	}
	var initialized codexSessionInitialize
	if err := c.Call(ictx, "initialize", init, &initialized); err != nil {
		c.Kill()
		<-c.done
		if exitErr := a.fenceUnconfirmedProcess(c); exitErr != nil {
			return nil, exitErr
		}
		a.failedStart()
		msg := err.Error()
		if tail := c.stderrText(); tail != "" && !strings.Contains(msg, tail) {
			msg += "\n" + truncTail(tail, 600)
		}
		return nil, errors.New("Codex 启动失败：" + msg)
	}
	c.recordSessionInitialization(initialized)
	_ = c.Notify("initialized", nil)
	c.probeTurnRevert(ictx)
	if err := validateCodexEffectiveProvider(ictx, c, s); err != nil {
		c.Kill()
		<-c.done
		if exitErr := a.fenceUnconfirmedProcess(c); exitErr != nil {
			return nil, exitErr
		}
		a.failedStart()
		return nil, err
	}
	if err := initializeCodexBuiltinAuthentication(ictx, c, s); err != nil {
		c.Kill()
		<-c.done
		if exitErr := a.fenceUnconfirmedProcess(c); exitErr != nil {
			return nil, exitErr
		}
		a.failedStart()
		return nil, err
	}

	a.mu.Lock()
	if a.closed || ctx.Err() != nil {
		a.mu.Unlock()
		c.Kill()
		<-c.done
		_ = a.fenceUnconfirmedProcess(c)
		return nil, errors.New("桥接程序正在退出")
	}
	a.conn, a.connSig, a.startedAt = c, sig, time.Now()
	a.wg.Add(1)
	a.mu.Unlock()
	go func() {
		defer a.wg.Done()
		<-c.done
		a.onExit(c)
	}()
	return c, nil
}

func (a *codexAgent) fenceUnconfirmedProcess(c *rpcConn) error {
	// done publishes the transport error, while processWaitDone separately
	// publishes OS exit. Preserve the fence across retries, not only this call.
	if !errors.Is(c.err, errRPCProcessExitUnconfirmed) || c.processExitConfirmed() {
		return nil
	}
	a.mu.Lock()
	a.unconfirmedProcess = c
	a.mu.Unlock()
	return errRPCProcessExitUnconfirmed
}

func (a *codexAgent) failedStart() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.bumpBackoffLocked()
}

func (a *codexAgent) bumpBackoffLocked() {
	if a.backoff == 0 {
		a.backoff = time.Second
	} else {
		a.backoff *= 2
		if a.backoff > time.Minute {
			a.backoff = time.Minute
		}
	}
	a.nextStart = time.Now().Add(a.backoff)
}

func (a *codexAgent) markUnloadedLocked() {
	for _, t := range a.threads {
		a.retireCodexContextLocked(t)
		a.retireCodexSessionActionsLocked(t)
		a.retireCodexWorkflowsLocked(t)
		t.loaded = false
		t.idleRestartRollout = nil
		t.turnVersion++
		t.historyReadVersion++
		t.turnID = ""
		t.items = nil
		// This helper is used only at whole app-server process boundaries
		// (confirmed exit or idle settings restart), never on notLoaded notices.
		// The next process must rebuild runtime ownership from real new events;
		// retaining a dead epoch's child tracker would lock API changes forever.
		t.childStates = nil
		t.childOverflow = false
	}
}

// onExit handles a dead app-server: running turns fail, approvals are withdrawn, the next call restarts.
func (a *codexAgent) onExit(c *rpcConn) {
	a.mu.Lock()
	if a.conn != c {
		a.mu.Unlock()
		return
	}
	a.conn = nil
	unconfirmed := errors.Is(c.err, errRPCProcessExitUnconfirmed) && !c.processExitConfirmed()
	if unconfirmed {
		a.unconfirmedProcess = c
	}
	if time.Since(a.startedAt) > time.Minute {
		a.backoff = 0
	}
	a.bumpBackoffLocked()
	closed := a.closed
	var failed []*codexThread
	var invalidatedContext []*codexThread
	for _, t := range a.threads {
		if t.context.conn == c && (t.context.snapshot != nil || t.context.compaction != nil) {
			invalidatedContext = append(invalidatedContext, t)
		}
		if t.loaded && t.turnID != "" {
			failed = append(failed, t)
		}
	}
	a.markUnloadedLocked()
	var contexts []protocol.SessionContext
	for _, t := range invalidatedContext {
		contexts = append(contexts, a.codexContextLocked(t))
	}
	var infos []protocol.SessionInfo
	for _, t := range failed {
		t.info.Status = "idle"
		if unconfirmed {
			t.info.Status = "failed"
			t.info.Controllable = false
			t.statusUnconfirmed = true
		}
		infos = append(infos, t.info)
	}
	a.mu.Unlock()
	if closed {
		return
	}
	for _, state := range contexts {
		state := state
		a.emit(protocol.Event{SessionKey: state.SessionKey, Type: "context.updated", Context: &state})
	}
	errText := "Codex 进程意外退出"
	if c.err != nil {
		errText = c.err.Error()
	}
	for _, info := range infos {
		a.aps.cancelSession(info.SessionKey)
		a.emit(protocol.Event{SessionKey: info.SessionKey, Type: "turn", Status: "failed", Error: errText})
		a.emit(protocol.Event{SessionKey: info.SessionKey, Type: "session.updated", Session: ptrInfo(info)})
	}
}

func (a *codexAgent) partial(e protocol.Event) {
	e.Tool = "codex"
	e.TS = nowMs()
	a.thr.Update(e)
}

func (a *codexAgent) emitFinal(e protocol.Event) {
	e = a.projectInputReceipt(e)
	e.Tool = "codex"
	e.TS = nowMs()
	a.thr.Final(e)
}

func ptrInfo(i protocol.SessionInfo) *protocol.SessionInfo { return &i }

func (a *codexAgent) emit(e protocol.Event) {
	e = a.projectInputReceipt(e)
	e.Tool = "codex"
	if e.TS == 0 {
		e.TS = nowMs()
	}
	a.sink(e)
}

// ---- thread state ----

func (a *codexAgent) threadLocked(id string) *codexThread {
	t := a.threads[id]
	if t == nil {
		t = &codexThread{id: id, info: protocol.SessionInfo{SessionKey: "codex:" + id, Tool: "codex", Client: "Codex", Status: "idle", Controllable: true}, messageAttachments: map[string][]protocol.AttachmentRef{}}
		a.threads[id] = t
	}
	return t
}

// historyEvents reattaches phone-uploaded media to Codex history. Codex may
// snapshot localImage inputs into inline data before returning a thread item,
// so the original path is not a reliable history key; the bridge persists the
// item-id → opaque-ref mapping alongside its owned-thread state.
func (a *codexAgent) historyEvents(sk string, th cxThread) []protocol.Event {
	events := codexHistory(sk, th)
	id := strings.TrimPrefix(sk, "codex:")
	a.mu.Lock()
	t := a.threads[id]
	if t != nil {
		// Notifications can add or replace attachment references concurrently
		// with history reads. Copy only the requested refs while holding the
		// same lock, so returned events never retain the mutable map or slices.
		for i := range events {
			if events[i].Type == "message" && events[i].Role == "user" && len(t.messageAttachments[events[i].ID]) > 0 {
				events[i].Attachments = append([]protocol.AttachmentRef(nil), t.messageAttachments[events[i].ID]...)
			}
		}
	}
	a.mu.Unlock()
	for i := range events {
		events[i] = a.projectInputReceipt(events[i])
	}
	return events
}

func fileRecentlyModified(path string) (time.Time, bool) {
	if path == "" {
		return time.Time{}, false
	}
	st, err := os.Stat(path)
	if err != nil {
		return time.Time{}, false
	}
	return st.ModTime(), time.Since(st.ModTime()) < externalActive
}

// These are additional refusal signals, not proof of exclusive ownership.
// Runtime status is local to the app-server being queried; notLoaded/idle
// cannot establish that a different Codex process has stopped using a thread.
func codexThreadInProgress(th cxThread) bool {
	if th.Status.Type == "active" {
		return true
	}
	for _, turn := range th.Turns {
		if turn.Status == "inProgress" {
			return true
		}
	}
	return false
}

// applyThreadLocked refreshes cached SessionInfo from a Thread object.
func (a *codexAgent) applyThreadLocked(th cxThread) *codexThread {
	t := a.threadLocked(th.ID)
	t.historyMode, t.ephemeral = th.HistoryMode, th.Ephemeral
	t.info.LatestTurnRevert = a.turnRevertPath() != "" && a.revertStoreError == nil && a.owned[th.ID] && t.loaded && !th.Ephemeral && th.ParentThreadID == nil && a.conn != nil && a.conn.turnRevertMethod(th.HistoryMode) != ""
	originator := ""
	if th.Originator != nil {
		originator = *th.Originator
	}
	info := &t.info
	if th.ParentThreadID != nil && validChildID(*th.ParentThreadID) {
		info.ParentSessionKey = "codex:" + *th.ParentThreadID
	}
	if th.ForkedFromID != nil {
		info.ForkedFromSessionKey = ""
		if validChildID(*th.ForkedFromID) && *th.ForkedFromID != th.ID {
			info.ForkedFromSessionKey = "codex:" + *th.ForkedFromID
		}
	}
	if th.AgentNickname != nil {
		info.AgentNickname = truncHead(strings.TrimSpace(*th.AgentNickname), 160)
	}
	if th.AgentRole != nil {
		info.AgentRole = truncHead(strings.TrimSpace(*th.AgentRole), 160)
	}
	info.Client = codexClient(th.Source, originator, a.owned[th.ID])
	title := ""
	if th.Name != nil {
		title = *th.Name
	}
	if title == "" {
		title = th.Preview
	}
	if title != "" {
		info.Title = titleText(title)
	}
	if th.Cwd != "" {
		info.Cwd = th.Cwd
	}
	if th.Model != nil && *th.Model != "" {
		info.Model = *th.Model
	}
	if th.UpdatedAt > 0 {
		info.UpdatedAt = th.UpdatedAt * 1000
	}
	if th.Path != nil {
		t.path = *th.Path
	}
	mt, recent := fileRecentlyModified(t.path)
	if ms := mt.UnixMilli(); !mt.IsZero() && ms > info.UpdatedAt {
		info.UpdatedAt = ms
	}
	switch th.Status.Type {
	case "active":
		t.statusUnconfirmed = false
		info.Status = "running"
		for _, f := range th.Status.ActiveFlags {
			if f == "waitingOnApproval" {
				info.Status = "waiting_approval"
			}
		}
	case "systemError":
		t.statusUnconfirmed = false
		info.Status = "failed"
	case "idle", "notLoaded", "":
		if th.Status.Type != "" {
			t.statusUnconfirmed = false
		}
		info.Status = "idle"
		if t.statusUnconfirmed {
			info.Status = "failed"
		}
	default:
		t.statusUnconfirmed = true
		if info.Status != "running" && info.Status != "waiting_approval" {
			// Reuse the existing non-ready state; no guessed wire/UI state.
			info.Status = "failed"
		}
	}
	info.Controllable = !t.statusUnconfirmed
	if !t.loaded && (codexThreadInProgress(th) || recent && !t.idleRestartRollout.matches(t.path)) {
		// External active turns remain read-only even during long, quiet tools.
		// Recent rollout writes remain an independent conservative safeguard.
		if info.Status != "waiting_approval" {
			info.Status = "running"
		}
		info.Controllable = false
	}
	if t.loaded && t.turnID != "" {
		info.ActiveTurnID = t.turnID
		info.Status = "running"
		if a.aps.countFor(info.SessionKey) > 0 {
			info.Status = "waiting_approval"
		}
	}
	if !t.loaded && len(th.Turns) > 0 {
		last := th.Turns[len(th.Turns)-1]
		if ValidTurnID(last.ID) && last.Status == "inProgress" {
			info.ActiveTurnID = last.ID
		} else if ValidTurnID(last.ID) && (last.Status == "completed" || last.Status == "failed" || last.Status == "interrupted") {
			info.ActiveTurnID, info.LastTerminalTurnID = "", last.ID
		}
	}
	return t
}

// ---- Agent API ----

var codexSourceKinds = []string{"cli", "vscode", "exec", "appServer", "subAgent", "subAgentReview", "subAgentCompact", "subAgentThreadSpawn", "subAgentOther"}

func (a *codexAgent) listThreads(ctx context.Context) ([]cxThread, error) {
	c, err := a.ensureRead(ctx)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data []cxThread `json:"data"`
	}
	params := map[string]any{"limit": maxSessions, "sortKey": "updated_at", "modelProviders": []string{}, "sourceKinds": codexSourceKinds}
	if err := c.Call(ctx, "thread/list", params, &resp); err != nil {
		var re *rpcError
		if !errors.As(err, &re) {
			return nil, err
		}
		// Older app-servers: fall back to the minimal request.
		if err := c.Call(ctx, "thread/list", map[string]any{"limit": maxSessions, "modelProviders": []string{}}, &resp); err != nil {
			return nil, err
		}
	}
	return resp.Data, nil
}

// WatchdogSessions returns only active turns already owned by the live
// app-server. Unlike Sessions, it never starts the app-server or loads history.
func (a *codexAgent) WatchdogSessions(ctx context.Context) ([]protocol.SessionInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a.mu.Lock()
	c := a.conn
	if a.closed || c == nil {
		a.mu.Unlock()
		return nil, nil
	}
	out := make([]protocol.SessionInfo, 0)
	for _, t := range a.threads {
		if !t.loaded || t.statusUnconfirmed || !ValidTurnID(t.turnID) || t.info.Status != "running" && t.info.Status != "waiting_approval" {
			continue
		}
		info := t.info
		info.ActiveTurnID = t.turnID
		out = append(out, info)
	}
	a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !c.alive() {
		return nil, nil
	}
	a.mu.Lock()
	current := !a.closed && a.conn == c
	a.mu.Unlock()
	if !current {
		return nil, nil
	}
	return out, nil
}

func (a *codexAgent) Sessions(ctx context.Context) ([]protocol.SessionInfo, error) {
	a.mu.Lock()
	managementVersion := a.sessionActionsVersion
	a.mu.Unlock()
	list, err := a.listThreads(ctx)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	if a.sessionActionsVersion != managementVersion {
		a.mu.Unlock()
		return nil, errCodexSessionActionStale
	}
	out := make([]protocol.SessionInfo, 0, len(list))
	seen := map[string]bool{}
	for _, th := range list {
		if th.Ephemeral {
			continue
		}
		t := a.applyThreadLocked(th)
		t.info.Archived = false // thread/list defaults to the non-archived directory.
		if t.sessionActions.conn == a.conn {
			t.sessionActions.archived, t.sessionActions.memberCursor = false, ""
		}
		seen[t.id] = true
		out = append(out, t.info)
	}
	// Threads running on our app-server that thread/list doesn't return (yet): codex may write the
	// rollout file lazily or filter the list by source, and a phone-started task must not vanish.
	for id, t := range a.threads {
		if !seen[id] && t.loaded && !t.info.Archived && t.info.SessionKey != "" {
			out = append(out, t.info)
		}
	}
	a.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	if len(out) > maxSessions {
		out = out[:maxSessions]
	}
	return out, nil
}

func (a *codexAgent) readThread(ctx context.Context, id string) (cxThread, error) {
	c, err := a.ensureRead(ctx)
	if err != nil {
		return cxThread{}, err
	}
	var resp struct {
		Thread cxThread `json:"thread"`
	}
	if err := c.Call(ctx, "thread/read", map[string]any{"threadId": id, "includeTurns": true}, &resp); err != nil {
		return cxThread{}, err
	}
	if resp.Thread.ID != id {
		return cxThread{}, errors.New("会话身份不一致，已取消读取")
	}
	return resp.Thread, nil
}

func historyKeys(th cxThread) []string {
	var keys []string
	for _, t := range th.Turns {
		for i := range t.Items {
			keys = append(keys, t.ID+":"+strconv.Itoa(i))
		}
	}
	return keys
}

func (a *codexAgent) Open(ctx context.Context, id string) (protocol.SessionInfo, []protocol.Event, error) {
	a.reconcileTurnReverts(ctx, id)
	th, err := a.readThread(ctx, id)
	if err != nil {
		return protocol.SessionInfo{}, nil, err
	}
	a.mu.Lock()
	t := a.applyThreadLocked(th)
	t.opened = true
	t.tailHistory = false
	t.historyReadVersion++
	t.historyItemDigests, t.historyTurnDigests, _ = codexHistoryDigests(codexHistoryWindow(th.Turns))
	t.seen = map[string]bool{}
	for _, k := range historyKeys(th) {
		t.seen[k] = true
	}
	info := t.info
	a.mu.Unlock()
	evs := dedupeEvents(a.historyEvents(info.SessionKey, th), maxHistoryEvents)
	pending := a.aps.snapshot(info.SessionKey)
	if len(pending) > 0 {
		info.Status = "waiting_approval"
	}
	return info, append(historyWithApprovals(evs, pending), a.revertedHistoryEvents(id)...), nil
}

func codexPolicy(approval string) (string, string) {
	if normalizePolicy(approval) == "auto_all" {
		return "on-request", "workspace-write"
	}
	// Current Codex no longer accepts the retired "untrusted" policy.
	return "on-request", "workspace-write"
}

// requireRelay returns the worker settings. A Bridge-managed API is optional:
// without one the worker uses Codex's own login and config.toml.
func (a *codexAgent) requireRelay(ctx context.Context) (Settings, error) {
	s := settingsForContext(ctx, a.settings)
	if s.codexRoot() != "" && s.CodexKey == "" && !s.UseOriginalCodex {
		return s, errors.New("电脑端选择的 API 缺少 Key，请在 Salcara Bridge 中检查")
	}
	return s, nil
}

// withProvider adds model_provider only when Bridge manages the API.
func withProvider(params map[string]any, s Settings) map[string]any {
	if p := s.codexProvider(); p != "" {
		params["modelProvider"] = p
	}
	return params
}

func (a *codexAgent) Start(ctx context.Context, cwd, prompt, model, approval string) (string, error) {
	return a.StartWithOptions(ctx, cwd, prompt, approval, TurnOptions{Model: model})
}

// StartWithOptions creates a thread and sends the first message with the
// phone's model / effort / images.
func (a *codexAgent) StartWithOptions(ctx context.Context, cwd, prompt, approval string, opts TurnOptions) (string, error) {
	if err := validateReferencedInput("codex", prompt, opts); err != nil {
		return "", err
	}
	if err := ValidateTurnFeatures(opts); err != nil {
		return "", err
	}
	if opts.ValidatePermission != nil {
		if err := opts.ValidatePermission(); err != nil {
			return "", err
		}
	}
	if opts.PermissionMode != "" && !validCodexPermissionMode(opts.PermissionMode) {
		return "", errCodexPermissionMode
	}
	model := opts.Model
	s, err := a.requireRelay(ctx)
	if err != nil {
		return "", err
	}
	c, err := a.ensure(ctx)
	if err != nil {
		return "", err
	}
	if err := validateCodexEffectiveProviderAt(ctx, c, s, cwd); err != nil {
		return "", err
	}
	if approval == "" {
		approval = s.Approval
	}
	approval = normalizePolicy(approval)
	if model == "" {
		model = s.CodexModel
	}
	if _, _, err := prepareCodexTurnFeatures(ctx, c, cwd, model, opts); err != nil {
		return "", err
	}
	ap, sb := codexPolicy(approval)
	params := withProvider(map[string]any{"cwd": cwd, "approvalPolicy": ap, "sandbox": sb}, s)
	if opts.PermissionMode != "" {
		codexPermissionParams(params, opts.PermissionMode)
		approval = "ask" // native reviewer decides; never blanket-approve locally
	}
	if model != "" {
		params["model"] = model
	}
	var resp struct {
		Thread cxThread `json:"thread"`
		Model  string   `json:"model"`
		codexPermissionReply
	}
	if err := c.Call(ctx, "thread/start", params, &resp); err != nil {
		return "", fmt.Errorf("Codex 创建会话失败：%w", err)
	}
	id := resp.Thread.ID
	if opts.PermissionMode != "" && !resp.codexPermissionReply.matches(opts.PermissionMode) {
		if resp.Thread.Status.Type != "idle" || codexThreadInProgress(resp.Thread) {
			return id, errCodexPermissionUnconfirmed
		}
		updated, err := updateNewCodexPermission(ctx, c, id, opts.PermissionMode)
		if err != nil {
			return id, err
		}
		resp.codexPermissionReply = updated
	}
	a.mu.Lock()
	current := a.threadLocked(id)
	if a.conn != c || current.turnID != "" || current.info.ActiveTurnID != "" || current.statusUnconfirmed {
		a.mu.Unlock()
		return id, errCodexPermissionUnconfirmed
	}
	a.owned[id] = true
	a.saveStateLocked()
	t := a.applyThreadLocked(resp.Thread)
	t.info.WorkspaceMode = opts.WorkspaceMode
	t.loaded, t.policy = true, approval
	t.legacyPolicyConfigured = opts.PermissionMode == ""
	t.info.PermissionMode = opts.PermissionMode
	t.inheritedFullAccess = approval == "auto_all" || resp.Sandbox.Type == "dangerFullAccess"
	if t.info.Title == "" {
		t.info.Title = titleText(prompt)
	}
	if t.info.Cwd == "" {
		t.info.Cwd = cwd
	}
	if resp.Model != "" {
		t.info.Model = resp.Model
	}
	// A created thread is not running until a turn was actually accepted.
	// In particular, a synchronous capacity rejection must not leave a phantom
	// running task that permanently blocks the phone's queued messages.
	t.info.UpdatedAt = nowMs()
	info := t.info
	a.mu.Unlock()
	a.emit(protocol.Event{SessionKey: info.SessionKey, Type: "session.updated", Session: ptrInfo(info)})
	opts.Model = ""          // already the thread's model
	opts.PermissionMode = "" // checked against thread/start before sending any input
	if err := a.startTurn(ctx, c, id, prompt, opts); err != nil {
		return id, err
	}
	return id, nil
}

// TurnOptions are optional per-message overrides chosen on the phone composer.
type TurnOptions struct {
	WorkspaceMode          protocol.WorkspaceMode  // Code/Work directory label; does not grant a cwd
	CrossTaskOrigin        *protocol.MessageOrigin // internal Bridge-created receipt; never decoded from user input
	ContextReferences      []protocol.ContextReference
	CollaborationMode      string                    // explicit native default/plan mode; empty follows host
	Skills                 []protocol.SkillReference // exact entries from this cwd's native catalog
	PermissionMode         string                    // explicit per-session override, never a global config change
	ClaudeApproval         string                    // explicit Claude native mode; never interpreted by Codex
	ValidatePermission     func() error              // live desktop consent recheck, internal only
	ExpectedTerminalTurnID string                    // internal terminal CAS, never inferred from text
	ControlOperationID     string                    // frozen receipt identity for same-session continuation
	Model                  string
	ClientUserMessageID    string                   // optional exact echo correlation, never a sender task identity
	Effort                 string                   // explicit supported runtime value; empty follows its default
	Images                 []string                 // local image files assembled from phone uploads
	Attachments            []protocol.AttachmentRef // refs to echo back to the paired phone
	// Desktop marks a new Claude Code session as Claude Desktop's (Code tab).
	Desktop bool
}

func (a *codexAgent) startTurn(ctx context.Context, c *rpcConn, id, text string, opts ...TurnOptions) error {
	a.mu.Lock()
	thread := a.threadLocked(id)
	a.mu.Unlock()
	thread.controlMu.Lock()
	defer thread.controlMu.Unlock()
	return a.startTurnLocked(ctx, c, id, text, opts...)
}

// Caller serializes the whole resume / permissions / input admission sequence.
func (a *codexAgent) startTurnLocked(ctx context.Context, c *rpcConn, id, text string, opts ...TurnOptions) error {
	var options TurnOptions
	if len(opts) > 0 {
		options = opts[0]
	}
	referenceText, referenceErr := contextReferenceInput("codex", options.ContextReferences)
	if referenceErr != nil {
		return referenceErr
	}
	input := []map[string]any{}
	if strings.TrimSpace(text) != "" {
		input = append(input, map[string]any{"type": "text", "text": text, "text_elements": []any{}})
	}
	if referenceText != "" {
		input = append(input, map[string]any{"type": "text", "text": referenceText, "text_elements": []any{}})
	}
	if len(opts) > 0 {
		for _, path := range opts[0].Images {
			input = append(input, map[string]any{"type": "localImage", "path": path})
		}
	}
	if len(input) == 0 {
		return errors.New("请输入内容或添加图片")
	}
	a.mu.Lock()
	thread := a.threadLocked(id)
	if thread.codexSessionActionBusyLocked() || thread.info.Archived {
		a.mu.Unlock()
		return errCodexSessionActionBusy
	}
	if thread.codexWorkflowBlocksMutationLocked() {
		a.mu.Unlock()
		return errCodexWorkflowBusy
	}
	if thread.codexCompactionBusyLocked() {
		a.mu.Unlock()
		return errCodexCompactionBusy
	}
	if thread.statusUnconfirmed {
		a.mu.Unlock()
		return errors.New("无法确认原 Codex 会话状态，暂不能发送")
	}
	version := thread.turnVersion
	cwd := thread.info.Cwd
	currentModel := thread.info.Model
	permissionUnconfirmed := thread.permissionUnconfirmed
	a.mu.Unlock()
	if permissionUnconfirmed && (len(opts) == 0 || opts[0].PermissionMode == "") {
		return errCodexPermissionUnconfirmed
	}
	if err := validateCodexEffectiveProviderAt(ctx, c, settingsForContext(ctx, a.settings), cwd); err != nil {
		return err
	}
	modeInput, skills, err := prepareCodexTurnFeatures(ctx, c, cwd, currentModel, func() TurnOptions {
		if len(opts) > 0 {
			return opts[0]
		}
		return TurnOptions{}
	}())
	if err != nil {
		return err
	}
	if len(skills) > 0 {
		// UserInput.skill is the explicit native invocation. Adding a synthetic
		// $name to UserInput.text duplicates it in live and historical messages.
		for _, skill := range skills {
			input = append(input, map[string]any{"type": "skill", "name": skill.Name, "path": skill.Path})
		}
	}
	if len(opts) > 0 && opts[0].PermissionMode != "" {
		if err := a.applyCodexPermission(ctx, c, thread, opts[0]); err != nil {
			return err
		}
	}
	if len(opts) > 0 && opts[0].ValidatePermission != nil {
		if err := opts[0].ValidatePermission(); err != nil {
			return err
		}
	}
	var receiptClientID string
	if len(options.ContextReferences) > 0 || len(skills) > 0 && a.stateFile != "" || options.CrossTaskOrigin != nil {
		receiptClientID = options.ClientUserMessageID
		if !skillReceiptUUID(receiptClientID) {
			receiptClientID = newUUID()
		}
		receipt := codexInputReceipt{Origin: protocol.CloneMessageOrigin(options.CrossTaskOrigin), Version: 1, SessionID: id, ClientID: receiptClientID, Text: text, Skills: append([]protocol.SkillReference(nil), skills...), ContextReferences: protocol.CloneContextReferences(options.ContextReferences)}
		if err := a.saveInputReceipt(receipt, "client", receiptClientID); err != nil {
			return err
		}
	}
	if len(opts) > 0 && len(opts[0].Attachments) > 0 {
		a.mu.Lock()
		t := a.threadLocked(id)
		if len(t.pendingAttachments) < 32 {
			t.pendingAttachments = append(t.pendingAttachments, append([]protocol.AttachmentRef(nil), opts[0].Attachments...))
		}
		a.mu.Unlock()
	}
	params := map[string]any{"threadId": id, "input": input}
	if receiptClientID != "" {
		params["clientUserMessageId"] = receiptClientID
	}
	if modeInput != nil {
		params["collaborationMode"] = modeInput
	}
	if len(opts) > 0 {
		if opts[0].ClientUserMessageID != "" && params["clientUserMessageId"] == nil {
			params["clientUserMessageId"] = opts[0].ClientUserMessageID
		}
		if opts[0].Model != "" {
			params["model"] = opts[0].Model
		}
		if opts[0].Effort != "" {
			params["effort"] = opts[0].Effort
		}
	}
	var resp struct {
		Turn cxTurn `json:"turn"`
	}
	if len(opts) > 0 && opts[0].ExpectedTerminalTurnID != "" {
		if err := a.reserveCodexContinuation(c, id, opts[0].ExpectedTerminalTurnID); err != nil {
			return err
		}
	}
	if options.CrossTaskOrigin != nil {
		a.mu.Lock()
		current := a.threads[id]
		ready := a.conn == c && current == thread && current.turnVersion == version && current.turnID == "" && current.info.ActiveTurnID == "" && !current.info.Archived && (current.info.Status == "idle" || current.info.Status == "failed")
		a.mu.Unlock()
		if !ready {
			return errors.New("cooperation_stale: 目标任务状态已改变，消息未发送")
		}
	}
	if err := c.Call(ctx, "turn/start", params, &resp); err != nil {
		if len(opts) > 0 && opts[0].ExpectedTerminalTurnID != "" && protocol.ExecutionErrorCode(err) != "command_delivery_uncertain" {
			a.mu.Lock()
			if a.conn == c && thread.turnVersion == version && thread.turnID == "" {
				thread.continuedTerminal = ""
			}
			a.mu.Unlock()
		}
		if len(opts) > 0 && len(opts[0].Attachments) > 0 {
			a.mu.Lock()
			t := a.threadLocked(id)
			if n := len(t.pendingAttachments); n > 0 {
				t.pendingAttachments = t.pendingAttachments[:n-1]
			}
			a.mu.Unlock()
		}
		if errors.Is(err, ErrRPCDeliveryTooLarge) {
			return err
		}
		return fmt.Errorf("Codex 发送失败：%w", err)
	}
	a.mu.Lock()
	t := a.threads[id]
	changed := false
	var info protocol.SessionInfo
	if t != nil && a.conn == c && t.turnVersion == version && resp.Turn.ID != "" && resp.Turn.Status == "inProgress" {
		t.turnID = resp.Turn.ID
		t.info.ActiveTurnID = resp.Turn.ID
		if t.info.Status != "running" && t.info.Status != "waiting_approval" {
			t.info.Status, t.info.UpdatedAt, changed = "running", nowMs(), true
		}
	}
	modelAckCurrent := t != nil && (t.turnVersion == version || resp.Turn.ID != "" && (t.turnID == resp.Turn.ID || t.turnID == "" && t.info.LastTerminalTurnID == resp.Turn.ID))
	if modelAckCurrent && a.conn == c && len(opts) > 0 && opts[0].Model != "" && t.info.Model != opts[0].Model {
		// The override sticks for later turns of this thread; show it on the phone.
		// A delayed reply to an older turn cannot overwrite a successor's model.
		t.info.Model, changed = opts[0].Model, true
	}
	if modelAckCurrent && a.conn == c && modeInput != nil && resp.Turn.ID != "" {
		t.info.CollaborationMode, changed = opts[0].CollaborationMode, true
	}
	if t != nil && changed {
		info = t.info
	}
	a.mu.Unlock()
	if changed {
		a.emit(protocol.Event{SessionKey: info.SessionKey, Type: "session.updated", Session: ptrInfo(info)})
	}
	return nil
}

func (a *codexAgent) Send(ctx context.Context, id, text string) error {
	return a.SendWithOptions(ctx, id, text, TurnOptions{})
}

// KnownSessionModel is a read-only hint for pre-turn input validation. It never
// starts/resumes a thread, guesses a model, or reads its conversation again.
func (a *codexAgent) KnownSessionModel(id string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if thread := a.threads[id]; thread != nil {
		return thread.info.Model
	}
	return ""
}

// SendWithOptions continues a thread with optional model / reasoning effort.
func (a *codexAgent) SendWithOptions(ctx context.Context, id, text string, opts TurnOptions) error {
	if err := validateReferencedInput("codex", text, opts); err != nil {
		return err
	}
	if err := ValidateTurnFeatures(opts); err != nil {
		return err
	}
	if opts.PermissionMode != "" && !validCodexPermissionMode(opts.PermissionMode) {
		return errCodexPermissionMode
	}
	if opts.ValidatePermission != nil {
		if err := opts.ValidatePermission(); err != nil {
			return err
		}
	}
	s, err := a.requireRelay(ctx)
	if err != nil {
		return err
	}
	c, err := a.ensure(ctx)
	if err != nil {
		return err
	}
	if opts.HasFeatures() {
		info, err := a.SessionMetadata(ctx, id)
		if err != nil {
			return err
		}
		if err := validateCodexEffectiveProviderAt(ctx, c, s, info.Cwd); err != nil {
			return err
		}
		if _, _, err := prepareCodexTurnFeatures(ctx, c, info.Cwd, a.KnownSessionModel(id), opts); err != nil {
			return err
		}
	}
	a.mu.Lock()
	t := a.threadLocked(id)
	a.mu.Unlock()
	t.controlMu.Lock()
	defer t.controlMu.Unlock()
	a.mu.Lock()
	if t.codexSessionActionBusyLocked() || t.info.Archived {
		a.mu.Unlock()
		return errCodexSessionActionBusy
	}
	if t.codexWorkflowBlocksMutationLocked() {
		a.mu.Unlock()
		return errCodexWorkflowBusy
	}
	if t.codexCompactionBusyLocked() {
		a.mu.Unlock()
		return errCodexCompactionBusy
	}
	loaded := t.loaded
	statusUnconfirmed := t.statusUnconfirmed
	policy := t.policy
	legacyPolicyConfigured := t.legacyPolicyConfigured
	previousPermission := t.info.PermissionMode
	resumeVersion := t.turnVersion
	resumeThread := t
	resumeStatus := t.info.Status
	resumeUnconfirmed := t.statusUnconfirmed
	a.mu.Unlock()
	if loaded && statusUnconfirmed {
		return errors.New("无法确认原 Codex 会话状态，暂不能发送")
	}
	if !loaded {
		if opts.PermissionMode == "" {
			opts.PermissionMode = previousPermission
		}
		// Always read afresh, including when the rollout path was cached by
		// Open/Sessions. A stale path or a failed read must not bypass refusal.
		th, readErr := a.readThread(ctx, id)
		a.mu.Lock()
		resumeCurrent := a.conn == c && a.threads[id] == resumeThread && resumeThread.turnVersion == resumeVersion && resumeThread.info.Status == resumeStatus && resumeThread.statusUnconfirmed == resumeUnconfirmed
		a.mu.Unlock()
		if !resumeCurrent {
			return errors.New("Codex 会话或运行进程已改变，请重新读取后发送")
		}
		if readErr == nil && th.ID != id {
			readErr = errors.New("返回的会话 ID 不匹配")
		}
		if readErr != nil {
			a.mu.Lock()
			if a.conn != c || a.threads[id] != resumeThread || resumeThread.turnVersion != resumeVersion || resumeThread.info.Status != resumeStatus || resumeThread.statusUnconfirmed != resumeUnconfirmed {
				a.mu.Unlock()
				return errors.New("Codex 会话状态已改变，请重新读取后发送")
			}
			t = a.threadLocked(id)
			t.info.Controllable = false
			info := t.info
			a.mu.Unlock()
			a.emit(protocol.Event{SessionKey: info.SessionKey, Type: "session.updated", Session: ptrInfo(info)})
			return errors.New("无法确认原 Codex 会话状态，暂不能恢复：" + readErr.Error())
		}
		a.mu.Lock()
		if a.conn != c || a.threads[id] != resumeThread || resumeThread.turnVersion != resumeVersion || resumeThread.info.Status != resumeStatus || resumeThread.statusUnconfirmed != resumeUnconfirmed {
			a.mu.Unlock()
			return errors.New("Codex 会话状态已改变，请重新读取后发送")
		}
		t = a.applyThreadLocked(th)
		path := t.path
		idleRollout := t.idleRestartRollout
		info := t.info
		statusUnconfirmed = t.statusUnconfirmed
		a.mu.Unlock()
		a.emit(protocol.Event{SessionKey: info.SessionKey, Type: "session.updated", Session: ptrInfo(info)})
		if statusUnconfirmed {
			return errors.New("无法确认原 Codex 会话状态，暂不能恢复")
		}
		if codexThreadInProgress(th) {
			return errors.New(errExternalRunning)
		}
		_, recent := fileRecentlyModified(path)
		if recent && !idleRollout.matches(path) {
			return errors.New(errExternalRunning)
		}
		if err := validateCodexEffectiveProviderAt(ctx, c, s, info.Cwd); err != nil {
			return err
		}
		params := withProvider(map[string]any{"threadId": id}, s)
		if legacyPolicyConfigured && policy != "" && previousPermission == "" && opts.PermissionMode == "" {
			ap, sb := codexPolicy(policy)
			params["approvalPolicy"], params["sandbox"] = ap, sb
		}
		// Unknown original threads keep their native policy/sandbox. Legacy
		// defaults are for newly created threads, never authority to rewrite an
		// existing thread or blanket-approve requests received while resuming.
		if policy == "" || previousPermission != "" || opts.PermissionMode != "" {
			policy = "ask"
		}
		a.mu.Lock()
		if a.conn != c || a.threads[id] != resumeThread || resumeThread.turnVersion != resumeVersion || t.turnID != "" || t.info.ActiveTurnID != "" || t.info.Status == "running" || t.info.Status == "waiting_approval" || t.statusUnconfirmed {
			a.mu.Unlock()
			return errors.New("Codex 会话状态已改变，暂不能恢复")
		}
		t.policy = policy
		a.mu.Unlock()
		resumeModel := opts.Model
		if resumeModel == "" {
			resumeModel = s.CodexModel
		}
		if resumeModel != "" {
			// A refreshed key can retire the bound default without changing its
			// address/key. Resume with the phone's verified model, not that old
			// default; the original thread and its history remain unchanged.
			params["model"] = resumeModel
		}
		var resp struct {
			Thread cxThread `json:"thread"`
			codexPermissionReply
		}
		if err := c.callGuarded(ctx, "thread/resume", params, &resp, func() error {
			// Config validation can take time. If another writer touched the
			// rollout after the fresh read, the clean-restart exception is gone.
			if recent && !idleRollout.matches(path) {
				return errors.New(errExternalRunning)
			}
			return nil
		}); err != nil {
			return codexResumeFailure(err)
		}
		if resp.Thread.ID != id || codexThreadInProgress(resp.Thread) || resp.Thread.Status.Type != "" && resp.Thread.Status.Type != "idle" && resp.Thread.Status.Type != "notLoaded" {
			return errors.New("Codex 恢复会话返回的身份或状态未确认，消息未发送")
		}
		a.mu.Lock()
		if a.conn != c || a.threads[id] != resumeThread || resumeThread.turnVersion != resumeVersion || resumeThread.turnID != "" || resumeThread.info.ActiveTurnID != "" || resumeThread.info.Status == "running" || resumeThread.info.Status == "waiting_approval" || resumeThread.statusUnconfirmed {
			a.mu.Unlock()
			return errors.New("Codex 会话状态已改变，消息未发送")
		}
		resp.Thread.Turns = nil
		// The exact idle resume has now been acknowledged by this process.
		// Native resume may itself append to the rollout; those writes are ours.
		t.loaded = true
		t = a.applyThreadLocked(resp.Thread)
		t.policy = policy
		t.idleRestartRollout = nil
		// Only an exact effective tuple is projected as confirmed. opts retains
		// any requested mode for checked reapplication if that proof is absent.
		t.info.PermissionMode = ""
		t.info.CollaborationMode = "" // a resumed runtime has not confirmed its collaboration mode
		for _, mode := range []string{"ask", "auto_review", "full_access"} {
			if resp.codexPermissionReply.matches(mode) {
				t.info.PermissionMode = mode
				t.inheritedFullAccess = mode == "full_access"
				break
			}
		}
		if resp.Sandbox.Type == "dangerFullAccess" {
			t.inheritedFullAccess = true
		}
		a.mu.Unlock()
	}
	return a.startTurnLocked(ctx, c, id, text, opts)
}

func (a *codexAgent) Interrupt(ctx context.Context, id string) error {
	return ErrExpectedTurnRequired
}

func (a *codexAgent) Respond(approvalID, decision, message string) bool {
	return a.aps.answer(approvalID, approvalAnswer{decision: normalizeDecision(decision), message: message, by: "phone"})
}

func (a *codexAgent) Models(ctx context.Context) []string {
	catalog, _ := a.ModelCatalog(ctx)
	return catalog.Models
}

// model/list reports the current runtime's own per-model options; no inference
// from names, foreign provider templates or paid completion probes is used.
func (a *codexAgent) ModelCatalog(ctx context.Context) (protocol.ModelCatalog, error) {
	out := protocol.ModelCatalog{Models: []string{}, ModelCapabilities: map[string]protocol.ModelCapability{}}
	c, err := a.ensure(ctx)
	if err != nil {
		return out, err
	}
	cursor := ""
	seen := map[string]bool{}
	for page := 0; page < 20; page++ {
		params := map[string]any{"limit": 100, "includeHidden": false}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var resp codexModelPage
		if err := c.Call(ctx, "model/list", params, &resp); err != nil {
			return protocol.ModelCatalog{}, err
		}
		appendCodexModelPage(&out, resp)
		if len(out.Models) > 1000 {
			return protocol.ModelCatalog{}, errors.New("模型目录过大")
		}
		if resp.NextCursor == "" {
			return out, nil
		}
		if len(resp.NextCursor) > 4096 || seen[resp.NextCursor] {
			return protocol.ModelCatalog{}, errors.New("模型目录读取位置无效")
		}
		seen[resp.NextCursor] = true
		cursor = resp.NextCursor
	}
	return protocol.ModelCatalog{}, errors.New("模型目录分页过多")
}

func (a *codexAgent) Close() {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.closed = true
	close(a.closeCh)
	c := a.conn
	a.conn = nil
	startup := a.starting
	var initializing *rpcConn
	if startup != nil {
		initializing = startup.conn
		startup.cancel()
	}
	a.mu.Unlock()
	a.sideTools.closeAll()
	a.aps.cancelAll()
	a.thr.Close()
	if c != nil {
		c.Kill()
	}
	if initializing != nil && initializing != c {
		initializing.Kill()
	}
	// Transport drainage and the startup/request wait group often depend on
	// the very same event callback. Give all of them one deadline, rather than
	// spending a fresh five seconds at each step after the process was killed.
	drainDeadline := time.Now().Add(codexCloseDrainTimeout)
	if c != nil {
		waitCodexCloseTransport(c, drainDeadline)
	}
	if initializing != nil && initializing != c {
		waitCodexCloseTransport(initializing, drainDeadline)
	}
	// closed is the admission gate for all wg.Add calls, including unpublished
	// startup and native request handlers. Cancellation owns their process even
	// while initialize is blocked; Close never waits on startMu to reach it.
	finished := make(chan struct{})
	go func() { a.wg.Wait(); close(finished) }()
	waitCodexCloseDrain(finished, drainDeadline)
}

// ---- notifications ----

type cxNote struct {
	ThreadID  string          `json:"threadId"`
	TurnID    string          `json:"turnId"`
	ItemID    string          `json:"itemId"`
	Delta     string          `json:"delta"`
	Index     int             `json:"summaryIndex"`
	Item      json.RawMessage `json:"item"`
	Turn      *cxTurn         `json:"turn"`
	Thread    *cxThread       `json:"thread"`
	Status    *cxThreadStatus `json:"status"`
	Changes   []cxChange      `json:"changes"`
	WillRetry bool            `json:"willRetry"`
	Error     *cxTurnError    `json:"error"`
	RequestID json.RawMessage `json:"requestId"`
	Plan      []cxPlanStep    `json:"plan"`
	Usage     *struct {
		Total struct {
			InputTokens  int64 `json:"inputTokens"`
			OutputTokens int64 `json:"outputTokens"`
		} `json:"total"`
		Last struct {
			InputTokens  int64 `json:"inputTokens"`
			OutputTokens int64 `json:"outputTokens"`
		} `json:"last"`
	} `json:"tokenUsage"`
}

func (t *codexThread) live(itemID string) *cxLive {
	if t.items == nil {
		t.items = map[string]*cxLive{}
	}
	l := t.items[itemID]
	if l == nil {
		l = &cxLive{}
		t.items[itemID] = l
	}
	return l
}

func (a *codexAgent) onNotify(method string, params json.RawMessage) {
	if a.onSupplementalNotify(method, params) {
		return
	}
	var n cxNote
	_ = json.Unmarshal(params, &n)
	sk := "codex:" + n.ThreadID
	switch method {
	case "thread/started":
		if n.Thread == nil {
			return
		}
		a.mu.Lock()
		t := a.applyThreadLocked(*n.Thread)
		info := t.info
		a.mu.Unlock()
		a.emit(protocol.Event{SessionKey: info.SessionKey, Type: "session.updated", Session: ptrInfo(info)})

	case "thread/status/changed":
		if n.Status == nil || !validChildID(n.ThreadID) {
			return
		}
		a.mu.Lock()
		t := a.threadLocked(n.ThreadID)
		old := t.info.Status
		oldControllable := t.info.Controllable
		switch n.Status.Type {
		case "active":
			t.statusUnconfirmed = false
			t.info.Status = "running"
			for _, f := range n.Status.ActiveFlags {
				if f == "waitingOnApproval" {
					t.info.Status = "waiting_approval"
				}
			}
		case "systemError":
			t.statusUnconfirmed = false
			t.info.Status = "failed"
		case "idle", "notLoaded":
			t.statusUnconfirmed = false
			t.info.Status = "idle"
		default:
			t.statusUnconfirmed = true
			if t.info.Status != "running" && t.info.Status != "waiting_approval" {
				t.info.Status = "failed"
			}
		}
		if t.statusUnconfirmed {
			t.info.Controllable = false
		} else if t.loaded {
			t.info.Controllable = true
		}
		t.info.UpdatedAt = nowMs()
		info := t.info
		for _, parent := range a.threads {
			if _, tracked := parent.childStates[n.ThreadID]; tracked {
				childStatus := ""
				switch n.Status.Type {
				case "active":
					childStatus = "running"
				case "idle":
					childStatus = "completed"
				case "systemError":
					childStatus = "errored"
				default:
					continue
				}
				parent.childStates[n.ThreadID] = childStatus
			}
		}
		a.mu.Unlock()
		if old != info.Status || oldControllable != info.Controllable {
			a.emit(protocol.Event{SessionKey: sk, Type: "session.updated", Session: ptrInfo(info)})
		}

	case "turn/started":
		a.mu.Lock()
		t := a.threadLocked(n.ThreadID)
		t.turnVersion++
		if n.Turn != nil {
			t.turnID = n.Turn.ID
		}
		t.info.ActiveTurnID = t.turnID
		t.baseline, t.usage = nil, protocol.Usage{}
		t.info.Status = "running"
		t.info.UpdatedAt = nowMs()
		info := t.info
		turnID := t.turnID
		for _, parent := range a.threads {
			if _, tracked := parent.childStates[n.ThreadID]; tracked {
				parent.childStates[n.ThreadID] = "running"
			}
		}
		a.mu.Unlock()
		a.emit(protocol.Event{SessionKey: sk, Type: "turn", Status: "started", TurnID: turnID})
		a.emit(protocol.Event{SessionKey: sk, Type: "session.updated", Session: ptrInfo(info)})

	case "turn/completed":
		// A future/malformed terminal payload must not invent success, drain
		// queued work, or clear a real active turn. A fresh host status/read can
		// subsequently establish its actual state.
		if n.Turn == nil || n.Turn.Status != "completed" && n.Turn.Status != "failed" && n.Turn.Status != "interrupted" {
			if validChildID(n.ThreadID) {
				a.emit(protocol.Event{SessionKey: sk, Type: "notice", TurnID: n.TurnID, Level: "warn", ErrorCode: "unsupported_turn_state", Text: "电脑返回了无法确认的任务结束状态，请在电脑核对"})
			}
			return
		}
		a.mu.Lock()
		t := a.threadLocked(n.ThreadID)
		// A late terminal for an older turn cannot retire its successor or the
		// successor's pending questions. A missing ID is not an exact target.
		if !ValidTurnID(n.Turn.ID) || t.turnID != "" && t.turnID != n.Turn.ID {
			a.mu.Unlock()
			return
		}
		t.turnVersion++
		turnID := t.turnID
		if n.Turn != nil && n.Turn.ID != "" {
			turnID = n.Turn.ID
		}
		t.turnID = ""
		t.info.ActiveTurnID, t.info.LastTerminalTurnID = "", turnID
		t.items = nil
		usage := t.usage
		t.info.Status = "idle"
		t.info.UpdatedAt = nowMs()
		info := t.info
		for _, parent := range a.threads {
			if _, tracked := parent.childStates[n.ThreadID]; tracked {
				if n.Turn == nil {
					continue
				}
				status := ""
				switch n.Turn.Status {
				case "completed":
					status = "completed"
				case "failed":
					status = "errored"
				case "interrupted":
					status = "interrupted"
				default:
					continue
				}
				parent.childStates[n.ThreadID] = status
			}
		}
		a.mu.Unlock()
		a.aps.cancelSession(sk)
		e := protocol.Event{SessionKey: sk, Type: "turn", Status: "completed", TurnID: turnID}
		if n.Turn != nil {
			e.Status = codexTurnStatus(n.Turn.Status)
			if n.Turn.Error != nil {
				e.Error = n.Turn.Error.Message
				e.ErrorCode = n.Turn.Error.executionCode()
				e.RetryAt = n.Turn.Error.retryAt(nowMs())
			}
			if n.Turn.DurationMS != nil && *n.Turn.DurationMS >= 0 {
				e.DurationMS = *n.Turn.DurationMS
			}
		}
		if usage.InputTokens > 0 || usage.OutputTokens > 0 {
			e.Usage = &usage
		}
		a.emit(e)
		a.emit(protocol.Event{SessionKey: sk, Type: "session.updated", Session: ptrInfo(info)})

	case "thread/tokenUsage/updated":
		if n.Usage == nil {
			return
		}
		a.mu.Lock()
		t := a.threadLocked(n.ThreadID)
		if t.baseline == nil {
			in := n.Usage.Total.InputTokens - n.Usage.Last.InputTokens
			t.baseline = &in
			t.baseOut = n.Usage.Total.OutputTokens - n.Usage.Last.OutputTokens
		}
		t.usage = protocol.Usage{InputTokens: n.Usage.Total.InputTokens - *t.baseline, OutputTokens: n.Usage.Total.OutputTokens - t.baseOut}
		a.mu.Unlock()

	case "item/started", "item/completed":
		var it cxItem
		if json.Unmarshal(n.Item, &it) != nil {
			return
		}
		completed := method == "item/completed"
		a.mu.Lock()
		t := a.threadLocked(n.ThreadID)
		cwd := t.info.Cwd
		if it.Type == "agentMessage" && it.Phase != nil {
			t.live(it.ID).messagePhase = codexMessagePhase(it.Phase)
		}
		if it.Type == "collabAgentToolCall" {
			if t.childStates == nil {
				t.childStates = map[string]string{}
			}
			for _, child := range codexChildIDs(it) {
				if len(t.childStates) >= 256 {
					if _, exists := t.childStates[child]; !exists {
						for old, status := range t.childStates {
							switch status {
							case "completed", "interrupted", "errored", "shutdown", "notFound":
								delete(t.childStates, old)
							}
						}
						if len(t.childStates) >= 256 {
							t.childOverflow = true
							continue
						}
					}
				}
				if state, exists := it.AgentsStates[child]; exists {
					t.childStates[child] = state.Status
				} else if _, tracked := t.childStates[child]; !tracked {
					t.childStates[child] = ""
				}
			}
			if len(it.ReceiverThreadIDs) > 256 || len(it.AgentsStates) > 256 {
				t.childOverflow = true
			}
		}
		if it.Type == "fileChange" {
			l := t.live(it.ID)
			if len(it.Changes) > 0 {
				l.changes = it.Changes
			} else {
				it.Changes = l.changes
			}
		}
		if !completed && it.Type == "commandExecution" && it.AggregatedOutput == nil {
			if l := t.items[it.ID]; l != nil && l.output != "" {
				out := l.output
				it.AggregatedOutput = &out
			}
		}
		// A tool or message can finish after a successor turn has started (or
		// after the current turn was cleared). Its native turn identity belongs
		// to the item, never to whichever turn happens to be active now.
		turnID := n.TurnID
		if turnID == "" {
			turnID = t.turnID // Compatibility notifications may omit turnId.
		} else if !ValidTurnID(turnID) {
			turnID = "" // A malformed explicit identity must not borrow the active turn.
		}
		e, ok := codexItemEvent(sk, it, completed, cwd)
		e.TurnID = turnID
		if ok && it.Type == "contextCompaction" {
			live := t.live(it.ID)
			previous := live.lastEvent
			op := t.context.compaction
			staleProgress := !completed && op != nil && op.conn == a.conn && op.turnID == n.TurnID &&
				(op.itemID == "" || op.itemID == it.ID) && (op.timedOut || op.completed)
			if staleProgress {
				ok = false // A late/replayed start cannot revive progress after timeout or completion.
			} else if previous.Type == "notice" && previous.ID == e.ID && previous.Text == e.Text && previous.Progress == e.Progress {
				ok = false // Native replay of the same compaction lifecycle item.
			} else {
				live.lastEvent = e
			}
		}
		if ok && it.Type == "plan" {
			live := t.live(it.ID)
			if live.lastEvent.OutputFinal && !completed {
				a.mu.Unlock()
				return
			}
			live.text, live.lastEvent = e.Output, e
		}
		if ok && e.Type == "message" && e.Role == "user" {
			if t.messageAttachments == nil {
				t.messageAttachments = map[string][]protocol.AttachmentRef{}
			}
			if refs, exists := t.messageAttachments[it.ID]; exists {
				e.Attachments = append([]protocol.AttachmentRef(nil), refs...)
			} else if len(t.pendingAttachments) > 0 {
				refs := append([]protocol.AttachmentRef(nil), t.pendingAttachments[0]...)
				t.pendingAttachments = t.pendingAttachments[1:]
				if len(refs) > 0 {
					t.messageAttachments[it.ID] = refs
					e.Attachments = refs
					a.saveStateLocked()
				}
			} else if len(e.Attachments) > 0 {
				t.messageAttachments[it.ID] = append([]protocol.AttachmentRef(nil), e.Attachments...)
				a.saveStateLocked()
			}
		}
		if ok && !completed && e.Type == "tool" {
			t.live(it.ID).lastEvent = e
		}
		a.mu.Unlock()
		if !ok {
			return
		}
		a.emitFinal(e)
		for _, child := range codexSubagentEvents(sk, it) {
			child.TurnID = e.TurnID
			a.emitFinal(child)
		}

	case "turn/plan/updated":
		// Codex's to-do list: the phone renders it as a checklist, like the app.
		if len(n.Plan) == 0 {
			return
		}
		id := "plan:" + n.TurnID
		if n.TurnID == "" {
			id = "plan:" + n.ThreadID
		}
		e := codexPlanEvent(sk, id, n.Plan)
		e.TurnID = n.TurnID
		a.emitFinal(e)

	case "item/agentMessage/delta":
		a.mu.Lock()
		t := a.threadLocked(n.ThreadID)
		l := t.live(n.ItemID)
		l.text += n.Delta
		text := l.text
		phase := l.messagePhase
		a.mu.Unlock()
		a.partial(protocol.Event{SessionKey: sk, TurnID: n.TurnID, Type: "message", ID: n.ItemID, Role: "assistant", Text: text, MessagePhase: phase})

	case "item/reasoning/summaryTextDelta", "item/reasoning/textDelta", "item/reasoning/summaryPartAdded":
		var idx struct {
			SummaryIndex *int `json:"summaryIndex"`
			ContentIndex *int `json:"contentIndex"`
		}
		indexError := json.Unmarshal(params, &idx)
		i := 0
		if method == "item/reasoning/textDelta" {
			if idx.ContentIndex != nil {
				i = *idx.ContentIndex
			}
		} else if idx.SummaryIndex != nil {
			i = *idx.SummaryIndex
		}
		// Compatibility runtimes still supply indices over JSON. Do not let a
		// negative/future unbounded index panic or force enormous allocations.
		if indexError != nil || i < 0 || i >= 256 {
			if validChildID(n.ThreadID) && validChildID(n.ItemID) {
				a.emit(protocol.Event{SessionKey: sk, ID: "reasoning-index:" + n.ItemID, TurnID: n.TurnID, Type: "notice", Level: "warn", ErrorCode: "unsupported_item", Text: "电脑返回了当前版本无法读取的处理摘要"})
			}
			return
		}
		a.mu.Lock()
		t := a.threadLocked(n.ThreadID)
		l := t.live(n.ItemID)
		if method == "item/reasoning/textDelta" {
			for len(l.content) <= i {
				l.content = append(l.content, "")
			}
			l.content[i] += n.Delta
		} else {
			for len(l.summary) <= i {
				l.summary = append(l.summary, "")
			}
			l.summary[i] += n.Delta
		}
		cj, _ := json.Marshal(l.content)
		text := reasoningText(l.summary, cj)
		a.mu.Unlock()
		if text != "" {
			a.partial(protocol.Event{SessionKey: sk, TurnID: n.TurnID, Type: "reasoning", ID: n.ItemID, Text: text})
		}

	case "item/commandExecution/outputDelta":
		a.mu.Lock()
		t := a.threadLocked(n.ThreadID)
		l := t.live(n.ItemID)
		l.output += n.Delta
		if len(l.output) > 64*1024 {
			l.output = l.output[len(l.output)-32*1024:]
		}
		e := l.lastEvent
		if e.ID == "" {
			e = protocol.Event{SessionKey: sk, Type: "tool", ID: n.ItemID, Kind: "command", Title: "运行命令"}
		}
		e.TurnID = n.TurnID
		e.Status = "running"
		e.Output = truncTail(l.output, maxOutputChars)
		a.mu.Unlock()
		a.partial(e)

	case "item/fileChange/patchUpdated":
		a.mu.Lock()
		t := a.threadLocked(n.ThreadID)
		l := t.live(n.ItemID)
		l.changes = n.Changes
		cwd := t.info.Cwd
		a.mu.Unlock()
		a.partial(protocol.Event{SessionKey: sk, TurnID: n.TurnID, Type: "tool", ID: n.ItemID, Kind: "file_change", Status: "running",
			Title: codexChangesTitle(n.Changes, cwd), Diff: codexChangesDiff(n.Changes)})

	case "error":
		if n.Error == nil || !validChildID(n.ThreadID) {
			return
		}
		level := "error"
		text := n.Error.Message
		if n.WillRetry {
			level = "warn"
			text += "（正在重试）"
		}
		a.emit(protocol.Event{SessionKey: sk, TurnID: n.TurnID, Type: "notice", Level: level, Text: truncHead(text, 1000), ErrorCode: n.Error.executionCode(), RetryAt: n.Error.retryAt(nowMs()), Retrying: n.WillRetry})

	case "serverRequest/resolved":
		a.aps.resolveByRef(string(n.RequestID))
	}
}
