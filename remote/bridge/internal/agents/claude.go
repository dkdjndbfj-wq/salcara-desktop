package agents

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"salcara/bridge/internal/protocol"
)

// claudeAgent runs one `claude -p --input-format stream-json` process per session the bridge drives and
// reads ~/.claude/projects for everything else.
type claudeAgent struct {
	exeOverride string
	home        string // ~/.claude (or $CLAUDE_CONFIG_DIR)
	sink        func(protocol.Event)
	settings    func() Settings
	thr         *throttle
	aps         *approvals
	hist        *claudeHistory

	startMu           sync.Mutex // serializes owned startup with Close
	mu                sync.Mutex
	procs             map[string]*claudeProc
	workers           map[*claudeProc]struct{}        // includes retired workers without confirmed OS exit
	live              map[string]protocol.SessionInfo // sessions run by the bridge (this bridge run)
	policies          map[string]string
	planRequired      map[string]bool                    // independent of confirmed mode; survives owned worker replacement
	desktop           map[string]bool                    // sessions that belong to Claude Desktop's Code tab
	fromPhone         map[string]bool                    // phone-created sessions with an unclaimed Desktop import
	allowed           map[string]map[string]bool         // "allow_session" tool names per session
	recentOwned       map[string]time.Time               // when a bridge process for the session last exited
	continuedTerminal map[string]string                  // terminal CAS reserved by an in-flight Continue
	elicitationHooks  map[string]*claudeElicitationLease // ephemeral, worker-bound local HTTP hook
	contextState      claudeContextRegistry
	sessionActions    claudeSessionActionRegistry
	sideQuestions     sideQuestionRegistry
	actionSDKResolve  func(context.Context) (*claudeSessionSDK, error) // test seam; production resolves the installed official SDK
	actionSDKRun      claudeSessionSDKRunner                           // test seam; production invokes native SDK helpers
	closed            bool
	closeCh           chan struct{}
	wg                sync.WaitGroup
}

type claudeProc struct {
	id                  string
	cmd                 *exec.Cmd
	stdin               io.WriteCloser
	stdout              io.ReadCloser
	stderr              io.ReadCloser
	wmu                 sync.Mutex
	turnMu              sync.Mutex // serializes feature controls with new-turn admission
	controlMu           sync.Mutex
	controlPending      map[string]chan claudeControlReply
	initialized         bool // protected by turnMu
	featureProbe        bool // immutable; never publishes a session or admits a turn
	stopOnce            sync.Once
	exitOnce            sync.Once
	stopCh              chan struct{}
	processWaitDone     chan struct{}
	treeTermination     processTreeTermination
	exitCleanupReady    atomic.Bool // final session cleanup completed before owned-exit receipt
	cwd                 string
	policy              string
	model               string
	workerSettingsSig   string      // hashed process settings; never emitted to the phone
	activeSubtasks      atomic.Bool // read across the stream reader and phone command goroutines
	mcpFile             string
	runtimeSettingsFile string
	permissionLease     *claudePermissionLease
	elicitationHook     *claudeElicitationLease
	mapper              *claudeMapper
	done                chan struct{}

	mu                sync.Mutex
	turns             int      // user turns written but not yet answered by a result
	turnIDs           []string // stable IDs in the owned stream's admission order
	stopped           bool
	stderrBuf         []byte
	resultUUIDs       map[string]struct{} // exact, worker-local bounded replay window
	resultOrder       []string
	resultCursor      int
	collaborationMode string // confirmed by native metadata/control response, protected by mu
	requirePlan       bool   // remains set until explicit default or approved ExitPlanMode
	planExitApproved  bool
	contextCompacting bool // protected by mu; excludes new turns until the native compact result
}

func newClaudeAgent(sink func(protocol.Event), settings func() Settings, exe, home string) *claudeAgent {
	if home == "" {
		home = defaultClaudeHome()
	}
	a := &claudeAgent{exeOverride: exe, home: home, sink: sink, settings: settings, aps: newApprovals(),
		procs: map[string]*claudeProc{}, workers: map[*claudeProc]struct{}{}, live: map[string]protocol.SessionInfo{}, policies: map[string]string{}, planRequired: map[string]bool{}, desktop: map[string]bool{}, fromPhone: map[string]bool{},
		allowed: map[string]map[string]bool{}, recentOwned: map[string]time.Time{}, continuedTerminal: map[string]string{}, elicitationHooks: map[string]*claudeElicitationLease{}, closeCh: make(chan struct{})}
	a.thr = newThrottle(sink)
	a.hist = newClaudeHistory(filepath.Join(home, "projects"))
	return a
}

func defaultClaudeHome() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".claude")
}

func (a *claudeAgent) ID() string   { return "claude" }
func (a *claudeAgent) Name() string { return "Claude Code" }

func (a *claudeAgent) Descriptor() protocol.AgentDescriptor {
	return protocol.AgentDescriptor{
		ID: "claude", Name: "Claude Code", Tool: "claude", APIFamily: "claude", ControlSurface: "cli",
		Capabilities: protocol.AgentCapabilities{
			SideQuestion:           true,
			ContextReferences:      true,
			ContextAnnotations:     true,
			ContextDiffAnnotations: true,
			List:                   true, Read: true, Send: true, Interrupt: true, PreciseInterrupt: true, Continue: true, Approval: true,
			Questions: true, Subagents: true, Attachments: true, ImageInput: true, Features: true,
			ImageOutput: true, ModelOverride: true, Context: true, Compact: true, SessionActions: true, ApprovalModes: []string{"ask", "auto_edits", "auto_all"},
		},
	}
}

func (a *claudeAgent) exe() (string, bool) {
	override := a.exeOverride
	if override == "" {
		override = a.settings().ClaudePath
	}
	return LocalExecutable("claude", override)
}

func (a *claudeAgent) Detect(ctx context.Context) protocol.Tool {
	t := protocol.Tool{ID: "claude", Name: "Claude Code"}
	p, ok := a.exe()
	if !ok {
		return t
	}
	v, ok := toolVersion(ctx, p)
	t.Available, t.Version = ok, v
	return t
}

func (a *claudeAgent) emit(e protocol.Event) {
	e.Tool = "claude"
	if e.TS == 0 {
		e.TS = nowMs()
	}
	a.sink(e)
}

func (a *claudeAgent) Models(ctx context.Context) []string {
	return []string{"sonnet", "opus", "haiku"}
}

func (a *claudeAgent) Respond(approvalID, decision, message string) bool {
	return a.aps.answer(approvalID, approvalAnswer{decision: normalizeDecision(decision), message: message, by: "phone"})
}

// ownedLocked: the bridge drives this session now or did a moment ago (its transcript writes are ours).
func (a *claudeAgent) ownedLocked(id string) bool {
	if a.procs[id] != nil {
		return true
	}
	if t, ok := a.recentOwned[id]; ok && time.Since(t) < 10*time.Second {
		return true
	}
	return false
}

// ---- process lifecycle ----

func (a *claudeAgent) mcpConfig(s Settings, id, permissionToken string) (string, error) {
	exe := s.BridgeExe
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return "", err
		}
	}
	cfg := map[string]any{"mcpServers": map[string]any{"salcara": map[string]any{
		"type":    "stdio",
		"command": exe,
		"args":    []string{"mcp-approval", "--port", strconv.Itoa(s.LocalPort), "--token", permissionToken, "--session", id},
	}}}
	b, _ := json.Marshal(cfg)
	dir := filepath.Join(os.TempDir(), "salcara-bridge")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(dir, "mcp-*.json")
	if err != nil {
		return "", err
	}
	path := file.Name()
	_, writeErr := file.Write(b)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(path)
		return "", errors.Join(writeErr, closeErr)
	}
	return path, nil
}

func claudeArgs(id, mcpFile, model, policy string, resume bool) []string {
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--include-partial-messages", "--permission-prompt-tool", "mcp__salcara__approve", "--mcp-config", mcpFile}
	if resume {
		args = append(args, "--resume", id)
	} else {
		args = append(args, "--session-id", id)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	switch policy {
	case "auto_all":
		// This is a native per-worker mode. The remote dispatcher requires an
		// explicit phone choice plus current desktop consent before launch.
		// Prompts still emitted by Claude are never auto-approved by our helper.
		args = append(args, "--permission-mode", "bypassPermissions")
	case "auto_edits":
		args = append(args, "--permission-mode", "acceptEdits")
	default:
		// Never inherit local default/auto/bypass while the phone says ask.
		args = append(args, "--permission-mode", "default")
	}
	return args
}

func (a *claudeAgent) spawn(ctx context.Context, id, cwd, model, policy string, resume bool, guards ...func() error) (*claudeProc, error) {
	return a.spawnConfigured(ctx, id, cwd, model, policy, resume, claudeSpawnOptions{}, guards...)
}

func (a *claudeAgent) spawnConfigured(ctx context.Context, id, cwd, model, policy string, resume bool, config claudeSpawnOptions, guards ...func() error) (*claudeProc, error) {
	a.startMu.Lock()
	defer a.startMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(guards) > 0 && guards[0] != nil {
		if err := guards[0](); err != nil {
			return nil, err
		}
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil, errors.New("桥接程序正在退出")
	}
	if a.hasUnconfirmedWorkerLocked() {
		a.mu.Unlock()
		return nil, errClaudeProcessExitUnconfirmed
	}
	for worker := range a.workers {
		if worker.id == id {
			a.mu.Unlock()
			return nil, errors.New("Claude Code 会话正在启动或运行，请稍后重试")
		}
	}
	a.mu.Unlock()
	s := settingsForContext(ctx, a.settings)
	if err := validateClaudeAPI(s); err != nil {
		return nil, err
	}
	exe, ok := a.exe()
	if !ok {
		return nil, errors.New("没有找到 Claude Code，请先安装 Claude Code")
	}
	if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		return nil, errors.New("文件夹不存在：" + cwd)
	}
	permissionLease, err := newClaudePermissionLease()
	if err != nil {
		return nil, errors.New("无法创建审批连接")
	}
	mcpFile, err := a.mcpConfig(s, id, permissionLease.token)
	if err != nil {
		return nil, errors.New("无法写入审批配置：" + err.Error())
	}
	args := claudeArgs(id, mcpFile, model, policy, resume)
	if config.mode == "plan" {
		args[len(args)-1] = "plan" // claudeArgs always appends its explicit native permission mode last.
	}
	if config.probe {
		args = append(args, "--no-session-persistence")
	}
	// Per-process settings leave the user's files untouched. Only Elicitation
	// is version-gated; explicit API routing must be pinned on every worker.
	hook, hookSettings := a.prepareElicitationHook(exe, s, id, cwd)
	started := false
	runtimeSettingsFile := ""
	defer func() {
		if !started {
			permissionLease.revoke()
			a.revokeElicitationHook(id, hook)
			_ = os.Remove(mcpFile)
			removeClaudeRuntimeSettings(runtimeSettingsFile)
		}
	}()
	runtimeSettings, err := claudeRuntimeSettings(s, hookSettings)
	if err != nil {
		return nil, err
	}
	runtimeSettingsFile, err = writeClaudeRuntimeSettings(runtimeSettings)
	if err != nil {
		return nil, err
	}
	args = append(args, "--settings", runtimeSettingsFile)
	cmd := exec.Command(exe, args...)
	prepareCmd(cmd)
	cmd.Dir = cwd
	toolEnv := map[string]string{
		"ANTHROPIC_BASE_URL":                       strings.TrimRight(s.claudeRoot(), "/"),
		"ANTHROPIC_AUTH_TOKEN":                     s.ClaudeKey,
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
		"MCP_TOOL_TIMEOUT":                         "700000", // approvals wait up to 10 minutes
	}
	dropEnv := []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT"}
	if s.ClaudeAuthMode == "api-key" {
		delete(toolEnv, "ANTHROPIC_AUTH_TOKEN")
		toolEnv["ANTHROPIC_API_KEY"] = s.ClaudeKey
		dropEnv = append(dropEnv, "ANTHROPIC_AUTH_TOKEN")
	}
	a.mu.Lock()
	if a.desktop[id] {
		// Keep Claude Desktop's mark on every turn written from the phone, so the
		// session stays in its Code tab instead of turning into a CLI session.
		toolEnv["CLAUDE_CODE_ENTRYPOINT"] = "claude-desktop"
	}
	a.mu.Unlock()
	for name, value := range claudePinnedAPIEnv(s) {
		toolEnv[name] = value
	}
	// The random hook capability is never placed in CLI arguments or settings.
	dropEnv = append(dropEnv, claudeElicitationEnv)
	if hook != nil {
		toolEnv[claudeElicitationEnv] = hook.token
	}
	cmd.Env = childEnv(exe, toolEnv, dropEnv)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		os.Remove(mcpFile)
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		closeRPCStartupPipes(cmd, stdin, nil, nil)
		os.Remove(mcpFile)
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		closeRPCStartupPipes(cmd, stdin, stdout, nil)
		os.Remove(mcpFile)
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		closeRPCStartupPipes(cmd, stdin, stdout, stderr)
		return nil, err // The startup defer retires both leases and the MCP file.
	}
	if len(guards) > 0 && guards[0] != nil {
		if err := guards[0](); err != nil {
			closeRPCStartupPipes(cmd, stdin, stdout, stderr)
			return nil, err
		}
	}
	if err := cmd.Start(); err != nil {
		closeRPCStartupPipes(cmd, stdin, stdout, stderr)
		os.Remove(mcpFile)
		return nil, errors.New("无法启动 Claude Code：" + err.Error())
	}
	p := &claudeProc{id: id, cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr, cwd: cwd, policy: policy, model: model, mcpFile: mcpFile, runtimeSettingsFile: runtimeSettingsFile, permissionLease: permissionLease, elicitationHook: hook, workerSettingsSig: claudeSettingsSig(s, a.exeOverride),
		mapper: newClaudeMapper("claude:"+id, cwd), done: make(chan struct{}), stopCh: make(chan struct{}), processWaitDone: make(chan struct{}), featureProbe: config.probe, requirePlan: config.mode == "plan"}
	a.mu.Lock()
	a.workers[p] = struct{}{}
	a.mu.Unlock()
	started = true
	a.wg.Add(2)
	go func() {
		defer a.wg.Done()
		buf := make([]byte, 4096)
		for {
			n, err := stderr.Read(buf)
			if n > 0 {
				p.mu.Lock()
				p.stderrBuf = append(p.stderrBuf, buf[:n]...)
				if len(p.stderrBuf) > 4096 {
					p.stderrBuf = p.stderrBuf[len(p.stderrBuf)-4096:]
				}
				p.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		defer a.wg.Done()
		br := bufio.NewReaderSize(stdout, 1<<20)
		for {
			line, err := readBoundedLine(br, MaxAgentLineBytes, func() {
				p.failControlRequests(ErrLineTooLarge)
				a.stop(p)
			})
			if len(line) > 0 {
				a.handleLine(p, line)
			}
			if err != nil {
				break
			}
		}
		werr := a.waitForClaudeProcessExit(p)
		a.onExit(p, werr)
	}()
	return p, nil
}

func (a *claudeAgent) writeTurn(p *claudeProc, text string, images []string, attachments []protocol.AttachmentRef, options ...TurnOptions) error {
	return a.writeTurnContext(context.Background(), p, text, images, attachments, options...)
}

func (a *claudeAgent) writeTurnContext(ctx context.Context, p *claudeProc, text string, images []string, attachments []protocol.AttachmentRef, options ...TurnOptions) error {
	p.turnMu.Lock()
	defer p.turnMu.Unlock()
	var opts TurnOptions
	if len(options) > 0 {
		opts = options[0]
	}
	displayText := text // The native skill slash prefix is transport, not user-authored body.
	referenceText, err := contextReferenceInput("claude", opts.ContextReferences)
	if err != nil {
		return err
	}
	content := []map[string]any{}
	for _, path := range images {
		block, err := claudeImageBlock(path)
		if err != nil {
			return err
		}
		content = append(content, block)
	}
	// Read attachments before refreshing the native skill definitions. The same
	// worker then acknowledges reload_skills without later attachment IO, while
	// turnMu still excludes other senders and feature controls.
	text, err = a.prepareClaudeFeatures(ctx, p, text, opts)
	if err != nil {
		return err
	}
	if strings.TrimSpace(text) != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	if referenceText != "" {
		content = append(content, map[string]any{"type": "text", "text": referenceText})
	}
	if len(content) == 0 {
		return errors.New("请输入内容或添加图片")
	}
	msg := map[string]any{"type": "user", "session_id": p.id, "parent_tool_use_id": nil,
		"message": map[string]any{"role": "user", "content": content}}
	userID := newID("u_")
	if len(opts.Skills) > 0 || len(opts.ContextReferences) > 0 || opts.CrossTaskOrigin != nil {
		// SDKUserMessage.uuid is the exact native transcript identity. Save only
		// explicit selections; a later /command text must never acquire skills.
		userID = newUUID()
		msg["uuid"] = userID
		if err := a.hist.saveInputReceipt(claudeSkillReceipt{Version: 1, SessionID: p.id, UUID: userID, ClientID: skillReceiptClientID(opts.ClientUserMessageID), Text: displayText, Skills: opts.Skills, Attachments: attachments, ContextReferences: protocol.CloneContextReferences(opts.ContextReferences), Origin: protocol.CloneMessageOrigin(opts.CrossTaskOrigin)}); err != nil {
			return err
		}
	}
	b, _ := json.Marshal(msg)
	b = append(b, '\n')
	p.wmu.Lock()
	// This is the new-turn admission boundary: all image reads and the wait
	// for an earlier writer have finished, but no turn has been reserved or
	// published. The guard may inspect this agent, so never hold a.mu/p.mu.
	if len(options) > 0 && options[0].ValidatePermission != nil {
		if err := options[0].ValidatePermission(); err != nil {
			p.wmu.Unlock()
			return err
		}
	}
	a.mu.Lock()
	p.mu.Lock()
	// Permission guards can call back into the agent, so they run above the
	// locks. Context cancellation alone is safe to recheck here after any
	// vendor-lock contention, at the actual turn-reservation boundary.
	if err := ctx.Err(); err != nil {
		p.mu.Unlock()
		a.mu.Unlock()
		p.wmu.Unlock()
		return err
	}
	retired := a.closed || a.procs[p.id] != p || p.stopped || p.transportRetired()
	if !retired && p.contextCompacting {
		p.mu.Unlock()
		a.mu.Unlock()
		p.wmu.Unlock()
		return errClaudeContextBusy
	}
	if !retired && len(options) > 0 && options[0].ClaudeApproval != "" && options[0].ClaudeApproval != p.policy {
		p.mu.Unlock()
		a.mu.Unlock()
		p.wmu.Unlock()
		return errors.New("permission_mode_changed: Claude 审批方式已改变，请重新确认")
	}
	if !retired && opts.CrossTaskOrigin != nil && (p.turns != 0 || p.activeSubtasks.Load() || a.live[p.id].Archived || a.live[p.id].ActiveTurnID != "") {
		p.mu.Unlock()
		a.mu.Unlock()
		p.wmu.Unlock()
		return errors.New("cooperation_stale: 目标任务状态已改变，消息未发送")
	}
	if !retired && len(options) > 0 && options[0].ExpectedTerminalTurnID != "" {
		info := a.live[p.id]
		if p.turns != 0 || p.activeSubtasks.Load() || info.ActiveTurnID != "" || info.LastTerminalTurnID != options[0].ExpectedTerminalTurnID {
			p.mu.Unlock()
			a.mu.Unlock()
			p.wmu.Unlock()
			return ErrTurnControlStale
		}
	}
	turnID := ""
	if !retired {
		// Reserve the local turn before the frame is visible to the worker.
		// A fast stdout result can otherwise finish a zero counter and the
		// sender then invents a running turn after that result has completed.
		p.turns++
		turnID = newID("turn_")
		p.turnIDs = append(p.turnIDs, turnID)
		a.invalidateClaudeContext(p)
	}
	p.mu.Unlock()
	a.mu.Unlock()
	if retired {
		p.wmu.Unlock()
		return errors.New("Claude Code 连接已结束，未发送这条消息，请稍后重试")
	}
	a.hist.noteOwnedTurn(p)
	sk := "claude:" + p.id
	a.emit(protocol.Event{SessionKey: sk, Type: "message", ID: userID, MessageOrigin: protocol.CloneMessageOrigin(opts.CrossTaskOrigin), ClientID: skillReceiptClientID(opts.ClientUserMessageID), TurnID: turnID, Role: "user", Text: displayText, Final: true, Attachments: attachments, Skills: append([]protocol.SkillReference(nil), opts.Skills...), ContextReferences: protocol.CloneContextReferences(opts.ContextReferences)})
	if !a.writeWorkerActive(p) {
		p.wmu.Unlock()
		return errors.New("Claude Code 连接已结束，未发送这条消息，请稍后重试")
	}
	a.emit(protocol.Event{SessionKey: sk, Type: "turn", Status: "started", TurnID: turnID})
	if !a.setWorkerRunning(p) || !a.writeWorkerActive(p) {
		p.wmu.Unlock()
		return errors.New("Claude Code 连接已结束，未发送这条消息，请稍后重试")
	}
	n, err := p.stdin.Write(b)
	p.wmu.Unlock()
	if err != nil {
		// Once a frame is attempted, a pipe error is not a vendor rejection.
		// Do not invite automatic duplicate work by calling it definitely unsent.
		a.stop(p)
		return &claudeDeliveryUncertainError{cause: err}
	}
	if n != len(b) {
		a.stop(p)
		return &claudeDeliveryUncertainError{cause: io.ErrShortWrite}
	}
	if !a.writeWorkerActive(p) {
		return errClaudeDeliveryInterrupted
	}
	return nil
}

// Caller holds a.mu; the proc lock is released before any local sink is called.
// A session ID alone cannot let a retired sender update a successor's state.
func (a *claudeAgent) writeWorkerActiveLocked(p *claudeProc) bool {
	if a.closed || a.procs[p.id] != p {
		return false
	}
	p.mu.Lock()
	active := !p.stopped && !p.transportRetired()
	p.mu.Unlock()
	return active
}

func (a *claudeAgent) writeWorkerActive(p *claudeProc) bool {
	a.mu.Lock()
	active := a.writeWorkerActiveLocked(p)
	a.mu.Unlock()
	return active
}

func (a *claudeAgent) setWorkerRunning(p *claudeProc) bool {
	a.mu.Lock()
	if !a.writeWorkerActiveLocked(p) {
		a.mu.Unlock()
		return false
	}
	info, ok := a.live[p.id]
	if !ok {
		info = protocol.SessionInfo{SessionKey: "claude:" + p.id, Tool: "claude", Client: "Claude Code", Controllable: true}
		if h, ok := a.hist.info(p.id); ok {
			info = h
		}
	}
	info.Status, info.Controllable, info.ApprovalMode = "running", true, p.policy
	p.mu.Lock()
	info.CollaborationMode = p.collaborationMode
	if len(p.turnIDs) > 0 {
		info.ActiveTurnID = p.turnIDs[0]
	}
	p.mu.Unlock()
	info = a.commitLiveStateLocked(p.id, info)
	a.mu.Unlock()
	a.emit(protocol.Event{SessionKey: info.SessionKey, Type: "session.updated", Session: ptrInfo(info)})
	return true
}

// setLive updates the cached SessionInfo of a bridge-run session and emits session.updated.
func (a *claudeAgent) setLive(id string, f func(*protocol.SessionInfo)) {
	a.mu.Lock()
	info, ok := a.live[id]
	if !ok {
		info = protocol.SessionInfo{SessionKey: "claude:" + id, Tool: "claude", Client: "Claude Code", Controllable: true, Status: "idle"}
		if h, ok := a.hist.info(id); ok {
			info = h
		}
	}
	f(&info)
	info.Controllable = true
	info = a.commitLiveStateLocked(id, info)
	a.mu.Unlock()
	a.emit(protocol.Event{SessionKey: info.SessionKey, Type: "session.updated", Session: ptrInfo(info)})
}

// Every live state mutation is versioned under a.mu before its lock-free sink
// publication. Strictly increasing milliseconds let the phone reject a prior
// snapshot even when concurrent commits share the same wall-clock tick. Disk
// history hydration keeps its original mtime until a live mutation is made.
func (a *claudeAgent) commitLiveStateLocked(id string, info protocol.SessionInfo) protocol.SessionInfo {
	previous := a.live[id].UpdatedAt
	if info.UpdatedAt > previous {
		previous = info.UpdatedAt
	}
	stamp := nowMs()
	if stamp <= previous {
		stamp = previous
		if previous < 1<<63-1 {
			stamp++
		}
	}
	info.UpdatedAt = stamp
	a.live[id] = info
	return info
}

func (a *claudeAgent) handleLine(p *claudeProc, line []byte) {
	var l claudeLine
	if json.Unmarshal(line, &l) != nil {
		return
	}
	if !a.readerWorkerActive(p) || l.SessionID != "" && l.SessionID != p.id || l.SessionIDT != "" && l.SessionIDT != p.id {
		return
	}
	if l.Type == "control_response" {
		p.acceptControlResponse(l.Response)
		return
	}
	if p.featureProbe {
		return // Initialization metadata must not create phantom history/session rows.
	}
	defer func() { p.activeSubtasks.Store(p.mapper.hasActiveSubtasks()) }()
	if l.ParentTU != nil && *l.ParentTU != "" {
		evs := p.mapper.subagentEntry(&l)
		p.activeSubtasks.Store(p.mapper.hasActiveSubtasks())
		for _, e := range evs {
			if !a.readerWorkerActive(p) {
				return
			}
			a.thr.Final(e)
		}
		return // child results never complete the parent turn
	}
	if a.observeClaudeContextLine(p, line) {
		return
	}
	sk := "claude:" + p.id
	switch l.Type {
	case "system":
		if (l.Subtype == "init" || l.Subtype == "status") && l.PermissionMode != "" {
			a.observeClaudeMode(p, l.PermissionMode, false)
		}
		if l.Subtype == "api_retry" {
			// A provider retry is neither a dropped Hub connection nor a turn
			// terminal. Keep the owned turn and report the native condition.
			a.emit(claudeAPIRetryEvent(&l, sk, p.activeTurnID()))
			return
		}
		if evs, handled := p.mapper.taskLifecycle(&l); handled {
			// Busy state must be visible before a sink can block or reenter a
			// phone command. Deferring this flag until publication returns lets
			// a model switch mistake a just-started background task for idle.
			p.activeSubtasks.Store(p.mapper.hasActiveSubtasks())
			for _, e := range evs {
				if !a.readerWorkerActive(p) {
					return
				}
				a.thr.Final(e)
			}
			a.setWorkerTaskStatus(p)
			return
		}
		if l.Subtype == "init" && l.Model != "" {
			a.mu.Lock()
			if info, ok := a.live[p.id]; ok && a.readerWorkerActiveLocked(p) {
				info.Model = l.Model
				a.commitLiveStateLocked(p.id, info)
			}
			a.mu.Unlock()
		}
	case "stream_event":
		evs := p.mapper.stream(l.Event)
		p.activeSubtasks.Store(p.mapper.hasActiveSubtasks())
		for _, e := range evs {
			if !a.readerWorkerActive(p) {
				return
			}
			e.Tool = "claude"
			e.TurnID = p.activeTurnID()
			a.thr.Update(e)
		}
	case "assistant":
		evs := p.mapper.assistant(l.Message, newID("m_"), nowMs())
		p.activeSubtasks.Store(p.mapper.hasActiveSubtasks())
		for _, e := range evs {
			if !a.readerWorkerActive(p) {
				return
			}
			e.TurnID = p.activeTurnID()
			a.thr.Final(e)
		}
	case "user":
		if !claudeUserOrigin(&l).peer {
			l.IsMeta = true // our typed text is emitted by writeTurn
		}
		evs := p.mapper.user(&l, "", nowMs())
		p.activeSubtasks.Store(p.mapper.hasActiveSubtasks())
		for _, e := range evs {
			if !a.readerWorkerActive(p) {
				return
			}
			if e.MessageOrigin == nil {
				e.TurnID = p.activeTurnID()
			}
			a.thr.Final(e)
		}
	case "result":
		// A result can belong to an injected background notification, not the
		// next phone prompt. Native origin/session identity takes precedence;
		// absence is the legacy ordered-stream contract, not a guessed origin.
		if !claudeResultIsHuman(&l, p.id) {
			return
		}
		e := claudeTerminalEvent(&l, sk)
		if l.Usage != nil || l.TotalCostUSD > 0 {
			u := protocol.Usage{CostUSD: l.TotalCostUSD}
			if l.Usage != nil {
				u.InputTokens = l.Usage.InputTokens + l.Usage.CacheReadTokens + l.Usage.CacheCreationTokens
				u.OutputTokens = l.Usage.OutputTokens
			}
			e.Usage = &u
			if l.DurationMS == nil && l.Usage != nil && l.Usage.DurationMS >= 0 {
				e.DurationMS = l.Usage.DurationMS
			}
		}
		if l.DurationMS != nil && *l.DurationMS >= 0 {
			e.DurationMS = *l.DurationMS
		}
		info, accepted := a.acceptClaudeResult(p, &l, &e)
		if !accepted {
			return
		}
		a.emit(protocol.Event{SessionKey: sk, Type: "session.updated", Session: &info})
		a.emit(e)
	}
}

// spawn starts its stdout reader before registering the process as current.
// That one owned startup generation may report init; a replaced, stopped, or
// exited generation must never update another worker with the same session ID.
// Caller holds a.mu; sink callbacks are always outside the vendor locks.
func (a *claudeAgent) readerWorkerActiveLocked(p *claudeProc) bool {
	if a.closed {
		return false
	}
	current := a.procs[p.id]
	if current != p {
		if _, starting := a.workers[p]; current != nil || !starting {
			return false
		}
	}
	p.mu.Lock()
	active := !p.stopped && !p.transportRetired()
	p.mu.Unlock()
	return active
}

func (a *claudeAgent) readerWorkerActive(p *claudeProc) bool {
	a.mu.Lock()
	active := a.readerWorkerActiveLocked(p)
	a.mu.Unlock()
	return active
}

func (a *claudeAgent) setWorkerTaskStatus(p *claudeProc) {
	a.mu.Lock()
	if !a.writeWorkerActiveLocked(p) {
		a.mu.Unlock()
		return
	}
	info := a.live[p.id]
	if info.SessionKey == "" {
		info = protocol.SessionInfo{SessionKey: "claude:" + p.id, Tool: "claude", Client: "Claude Code", Controllable: true}
	}
	p.mu.Lock()
	busy := p.turns > 0 || p.activeSubtasks.Load()
	p.mu.Unlock()
	info.Status = "idle"
	if busy {
		info.Status = "running"
	}
	info = a.commitLiveStateLocked(p.id, info)
	a.mu.Unlock()
	a.emit(protocol.Event{SessionKey: info.SessionKey, Type: "session.updated", Session: ptrInfo(info)})
}

const maxClaudeResultUUIDs = 4096

func claudeResultIsHuman(l *claudeLine, session string) bool {
	if l.AgentID != "" || l.IsSidechain || l.SessionID != "" && l.SessionID != session || l.SessionIDT != "" && l.SessionIDT != session {
		return false
	}
	origin := strings.TrimSpace(string(l.Origin))
	if origin == "" || origin == "null" {
		return true // CLI versions before origin still emit ordered root results.
	}
	var source struct {
		Kind string `json:"kind"`
	}
	return json.Unmarshal(l.Origin, &source) == nil && source.Kind == "human"
}

func claudeTerminalEvent(l *claudeLine, sessionKey string) protocol.Event {
	e := protocol.Event{SessionKey: sessionKey, Type: "turn", Status: "completed"}
	switch l.TerminalReason {
	case "aborted_streaming", "aborted_tools":
		e.Status = "interrupted"
	case "", "completed":
		if l.IsError || l.APIErrorStatus >= 400 || l.Subtype != "success" {
			e.Status = "failed"
		}
	default:
		// An unknown/new reason is not evidence of successful completion.
		e.Status = "failed"
	}
	if e.Status != "completed" {
		e.Error = l.Result
		if e.Error == "" {
			e.Error = strings.Join(l.Errors, "\n")
		}
		if e.Error == "" && l.APIErrorStatus >= 400 {
			e.Error = "Claude API error (HTTP " + strconv.Itoa(l.APIErrorStatus) + ")"
		}
		if e.Error == "" && e.Status == "failed" {
			e.Error = l.TerminalReason
		}
		if e.Error == "" && e.Status == "failed" {
			e.Error = l.Subtype
		}
		if e.Error == "" && e.Status == "failed" {
			e.Error = "Claude result missing completion status"
		}
		e.Error = truncHead(e.Error, 2000)
		if e.Status == "failed" && l.APIErrorStatus >= 400 {
			e.ErrorCode, _ = claudeAPICondition("", l.APIErrorStatus)
		}
	}
	return e
}

// acceptClaudeResult atomically consumes one root result and updates the same
// worker's live turn state. A concurrent new send must not be overwritten by an
// idle snapshot computed before that send. Sinks remain outside both locks.
func (a *claudeAgent) acceptClaudeResult(p *claudeProc, l *claudeLine, e *protocol.Event) (protocol.SessionInfo, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p.mu.Lock()
	if a.closed || a.procs[p.id] != p || p.stopped || p.transportRetired() || !p.rememberResultLocked(l.UUID) {
		p.mu.Unlock()
		return protocol.SessionInfo{}, false
	}
	if p.turns <= 0 {
		p.mu.Unlock()
		return protocol.SessionInfo{}, false // no admitted phone work to finish
	}
	if len(p.turnIDs) > 0 {
		e.TurnID = p.turnIDs[0]
		p.turnIDs = p.turnIDs[1:]
	}
	p.turns--
	busy := p.turns > 0
	info := a.live[p.id]
	if info.SessionKey == "" {
		info = protocol.SessionInfo{SessionKey: "claude:" + p.id, Tool: "claude", Client: "Claude Code", Controllable: true}
	}
	info.ActiveTurnID = ""
	if len(p.turnIDs) > 0 {
		info.ActiveTurnID = p.turnIDs[0]
	}
	p.mu.Unlock()
	info.Status, info.Controllable = "running", true
	if !busy {
		if !p.mapper.hasActiveSubtasks() {
			info.Status = "idle"
		}
		// Keep the real root terminal while background work still runs. The
		// busy guard blocks Continue until that work ends; its last task event
		// must not have to invent (or lose) the original continuation target.
		if ValidTurnID(e.TurnID) {
			info.LastTerminalTurnID = e.TurnID
		}
	}
	if !busy {
		a.hist.noteOwnedTerminal(p, e.Status)
	}
	info = a.commitLiveStateLocked(p.id, info)
	if a.continuedTerminal[p.id] == e.TurnID {
		delete(a.continuedTerminal, p.id)
	}
	return info, true
}

// The CLI emits UUIDs in modern versions. Anonymous legacy results cannot be
// content-deduplicated: two genuine turns may have identical text and usage.
// Retain a fixed exact replay window per worker, without unbounded session RAM.
func (p *claudeProc) rememberResultLocked(id string) bool {
	if id == "" {
		return true
	}
	if len(id) > 256 {
		return false
	}
	if _, exists := p.resultUUIDs[id]; exists {
		return false
	}
	if p.resultUUIDs == nil {
		p.resultUUIDs = make(map[string]struct{})
	}
	if len(p.resultOrder) < maxClaudeResultUUIDs {
		p.resultOrder = append(p.resultOrder, id)
	} else {
		delete(p.resultUUIDs, p.resultOrder[p.resultCursor])
		p.resultOrder[p.resultCursor] = id
		p.resultCursor = (p.resultCursor + 1) % maxClaudeResultUUIDs
	}
	p.resultUUIDs[id] = struct{}{}
	return true
}

func (a *claudeAgent) onExit(p *claudeProc, werr error) {
	p.exitOnce.Do(func() { a.finishExit(p, werr) })
}

func (a *claudeAgent) finishExit(p *claudeProc, werr error) {
	// A successor is allowed only after this worker's session cleanup has
	// completed; otherwise a late cancelSession could consume its approvals.
	defer func() {
		// Either this side or a later successful terminator records the
		// receipt after both session cleanup and tree confirmation. The
		// history method is idempotent and checks this exact worker.
		p.exitCleanupReady.Store(true)
		a.hist.recordOwnedExit(p, werr == nil || p.wasStopped())
		a.mu.Lock()
		if p.processExitConfirmed() {
			delete(a.workers, p)
		}
		close(p.done)
		a.mu.Unlock()
	}()
	p.permissionLease.revoke()
	a.revokeElicitationHook(p.id, p.elicitationHook)
	a.mu.Lock()
	current := a.procs[p.id]
	ownsSession := !p.featureProbe && (current == nil || current == p)
	if current == p {
		delete(a.procs, p.id)
	}
	if a.workers == nil {
		a.workers = map[*claudeProc]struct{}{}
	}
	a.workers[p] = struct{}{}
	if ownsSession {
		a.recentOwned[p.id] = time.Now()
		p.mu.Lock()
		a.planRequired[p.id] = p.requirePlan
		p.mu.Unlock()
	}
	closed := a.closed
	a.mu.Unlock()
	// Each generation has a distinct config file, so a losing worker may clean
	// its own helper file without deleting a replacement's configuration.
	_ = os.Remove(p.mcpFile)
	// The Wait goroutine owns runtime settings cleanup. A retired transport
	// can precede actual OS exit; keep the provider pin until that exit.
	// A losing startup candidate must not cancel approvals or publish terminal
	// state for another, still-current worker which uses the same session ID.
	if !ownsSession {
		return
	}
	if !closed {
		a.finishClaudeAutomaticCompaction(p, "unconfirmed", "Claude 连接已结束，压缩结果未确认")
	}
	a.aps.cancelSession("claude:" + p.id)
	p.mu.Lock()
	turns, stopped := p.turns, p.stopped
	turnIDs := append([]string(nil), p.turnIDs...)
	p.turnIDs = nil
	p.turns = 0
	tail := strings.TrimSpace(string(p.stderrBuf))
	p.mu.Unlock()
	if closed {
		return
	}
	sk := "claude:" + p.id
	if turns > 0 {
		e := protocol.Event{SessionKey: sk, Type: "turn", Status: "interrupted"}
		if !stopped {
			e.Status = "failed"
			msg := "Claude Code 意外退出"
			if werr != nil {
				msg += "：" + werr.Error()
			}
			if tail != "" {
				msg += "\n" + truncTail(tail, 800)
			}
			e.Error = msg
		}
		a.hist.noteOwnedTerminal(p, e.Status)
		if len(turnIDs) == 0 {
			a.emit(e)
		} else {
			for _, turnID := range turnIDs {
				e.TurnID = turnID
				a.emit(e)
			}
		}
	}
	a.setLive(p.id, func(i *protocol.SessionInfo) {
		i.Status = "idle"
		i.ActiveTurnID = ""
		if len(turnIDs) > 0 {
			i.LastTerminalTurnID = turnIDs[len(turnIDs)-1]
		}
	})
}

func (a *claudeAgent) stop(p *claudeProc) {
	a.stopWithTreeKiller(p, killTreeResult)
}

func (a *claudeAgent) stopWithTreeKiller(p *claudeProc, terminate func(*exec.Cmd) error) {
	p.stopOnce.Do(func() {
		// Publish pending before retirement or root termination can race
		// finishExit/another startup into dropping this worker's fence.
		a.mu.Lock()
		terminateNeeded := !p.rootProcessExitConfirmed()
		if terminateNeeded {
			p.treeTermination.begin()
		}
		p.mu.Lock()
		p.stopped = true
		p.mu.Unlock()
		a.mu.Unlock()
		if p.stopCh != nil {
			close(p.stopCh)
		}
		p.permissionLease.revoke()
		a.revokeElicitationHook(p.id, p.elicitationHook)
		if terminateNeeded {
			treeErr := terminate(p.cmd)
			p.treeTermination.finish(treeErr)
			if treeErr == nil && p.exitCleanupReady.Load() {
				a.hist.recordOwnedExit(p, true)
			}
		}
		p.closeOwnedPipes()
	})
}

// ---- Agent API ----

func (a *claudeAgent) Start(ctx context.Context, cwd, prompt, model, approval string) (string, error) {
	return a.StartWithOptions(ctx, cwd, prompt, approval, TurnOptions{Model: model})
}

func (a *claudeAgent) StartWithOptions(ctx context.Context, cwd, prompt, approval string, opts TurnOptions) (string, error) {
	if err := validateReferencedInput("claude", prompt, opts); err != nil {
		return "", err
	}
	if err := validateClaudeFeatures(opts); err != nil {
		return "", err
	}
	opts = claudeTurnOptionsForContext(ctx, opts)
	if opts.ValidatePermission != nil {
		if err := opts.ValidatePermission(); err != nil {
			return "", err
		}
	}
	model := opts.Model
	s := settingsForContext(ctx, a.settings)
	if err := validateClaudeAPI(s); err != nil {
		return "", err
	}
	if approval == "" {
		approval = s.Approval
		if approval == "auto_all" {
			approval = "ask"
		} // A desktop default is not an explicit phone choice.
	}
	approval = normalizePolicy(approval)
	if model == "" {
		model = s.ClaudeModel
	}
	id := newUUID()
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return "", errors.New("桥接程序正在退出")
	}
	a.policies[id] = approval
	client := "Claude Code"
	if opts.Desktop {
		a.desktop[id], a.fromPhone[id], client = true, true, "Claude Desktop"
	}
	a.commitLiveStateLocked(id, protocol.SessionInfo{SessionKey: "claude:" + id, Tool: "claude", Client: client, Title: titleText(prompt),
		WorkspaceMode: opts.WorkspaceMode, Cwd: cwd, Status: "running", Controllable: true, Model: model})
	a.mu.Unlock()
	p, err := a.spawnConfigured(ctx, id, cwd, model, approval, false, claudeSpawnOptions{mode: opts.CollaborationMode}, opts.ValidatePermission)
	if err != nil {
		a.mu.Lock()
		delete(a.live, id)
		delete(a.policies, id)
		delete(a.desktop, id)
		delete(a.fromPhone, id)
		a.mu.Unlock()
		return "", err
	}
	a.mu.Lock()
	if a.closed || a.hasUnconfirmedWorkerLocked() {
		closed := a.closed
		a.mu.Unlock()
		a.stop(p)
		if closed {
			return "", errors.New("桥接程序正在退出")
		}
		return "", errClaudeProcessExitUnconfirmed
	}
	a.procs[id] = p
	a.mu.Unlock()
	if err := a.writeTurnContext(ctx, p, prompt, opts.Images, opts.Attachments, opts); err != nil {
		a.stop(p)
		if protocol.ExecutionErrorCode(err) == "command_delivery_uncertain" {
			return id, err
		}
		return "", err
	}
	return id, nil
}

func (a *claudeAgent) Send(ctx context.Context, id, text string) error {
	return a.SendWithOptions(ctx, id, text, TurnOptions{})
}

// SessionRequiresFullAccessConsent reports the policy a new turn would use.
// Existing workers keep their approved policy; an owned transcript without a
// recorded grant never inherits a full-access default. Revocation blocks a
// later phone turn, not tool calls belonging to an already accepted task.
func (a *claudeAgent) SessionRequiresFullAccessConsent(id string) bool {
	if id == "" {
		return false // session.start validates its resolved, possibly explicit policy
	}
	a.mu.Lock()
	policy := a.policies[id]
	if policy == "" {
		if p := a.procs[id]; p != nil {
			policy = p.policy
		}
	}
	a.mu.Unlock()
	return normalizePolicy(policy) == "auto_all"
}

// SendWithOptions continues a session; a different model restarts the idle
// Claude Code process with --model (the conversation resumes as is).
func (a *claudeAgent) SendWithOptions(ctx context.Context, id, text string, opts TurnOptions) error {
	if err := validateReferencedInput("claude", text, opts); err != nil {
		return err
	}
	if err := validateClaudeFeatures(opts); err != nil {
		return err
	}
	if opts.ClaudeApproval != "" && opts.ClaudeApproval != "ask" && opts.ClaudeApproval != "auto_edits" && opts.ClaudeApproval != "auto_all" {
		return errors.New("permission_mode_invalid: Claude 审批方式无效")
	}
	opts = claudeTurnOptionsForContext(ctx, opts)
	if opts.ValidatePermission != nil {
		if err := opts.ValidatePermission(); err != nil {
			return err
		}
	}
	settingsNow := settingsForContext(ctx, a.settings)
	if err := validateClaudeAPI(settingsNow); err != nil {
		return err
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return errors.New("桥接程序正在退出")
	}
	p := a.procs[id]
	policy := a.policies[id]
	if opts.ClaudeApproval != "" {
		policy = opts.ClaudeApproval
	}
	liveInfo, hasLive := a.live[id]
	resumeMode := liveInfo.CollaborationMode
	if a.planRequired[id] {
		resumeMode = "plan"
	}
	if p != nil {
		p.mu.Lock()
		if p.requirePlan {
			resumeMode = "plan"
		}
		p.mu.Unlock()
	}
	if opts.CollaborationMode != "" {
		resumeMode = opts.CollaborationMode
	}
	a.mu.Unlock()
	if p != nil {
		select {
		case <-p.done:
			p = nil
		default:
		}
	}
	settingsChanged := p != nil && p.workerSettingsSig != claudeSettingsSig(settingsNow, a.exeOverride)
	if p != nil && (settingsChanged || opts.Model != "" && opts.Model != p.model || opts.ClaudeApproval != "" && opts.ClaudeApproval != p.policy) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := a.retireIdleClaudeWorker(p); err != nil {
			return err
		}
		a.stop(p)
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			return errors.New("Claude Code 尚未退出，请稍后重试")
		case <-ctx.Done():
			return ctx.Err()
		}
		a.mu.Lock()
		if a.procs[id] == p {
			delete(a.procs, id)
		}
		a.mu.Unlock()
		// Restart the worker with --resume, not a fresh session, and retain its actual cwd.
		liveInfo.Cwd, liveInfo.Model, hasLive = p.cwd, p.model, true
		p = nil
	}
	if p != nil {
		return a.writeTurnContext(ctx, p, text, opts.Images, opts.Attachments, opts)
	}
	info, ok := a.hist.info(id)
	if !ok && hasLive {
		info, ok = liveInfo, true
	}
	if !ok {
		return errors.New("找不到这个会话")
	}
	if !a.hist.ownedCompletion(id) {
		return errClaudeExternalStateUnknown
	}
	s := settingsForContext(ctx, a.settings)
	if policy == "" {
		policy = normalizePolicy(s.Approval)
		if policy == "auto_all" {
			policy = "ask"
		} // Unknown historical policy cannot grant bypass.
	}
	cwd := info.Cwd
	if cwd == "" && hasLive {
		cwd = liveInfo.Cwd
	}
	a.mu.Lock()
	a.policies[id] = policy
	if _, ok := a.live[id]; !ok {
		a.live[id] = info
	}
	if strings.Contains(strings.ToLower(info.Client), "desktop") {
		a.desktop[id] = true
	}
	a.mu.Unlock()
	model := s.ClaudeModel
	if model == "" {
		model = info.Model
	}
	if opts.Model != "" {
		model = opts.Model
		a.setLive(id, func(i *protocol.SessionInfo) { i.Model = model })
	}
	np, err := a.spawnConfigured(ctx, id, cwd, model, policy, true, claudeSpawnOptions{mode: resumeMode}, opts.ValidatePermission)
	if err != nil {
		return err
	}
	a.mu.Lock()
	if a.closed || a.hasUnconfirmedWorkerLocked() {
		closed := a.closed
		a.mu.Unlock()
		a.stop(np)
		if closed {
			return errors.New("桥接程序正在退出")
		}
		return errClaudeProcessExitUnconfirmed
	}
	if old := a.procs[id]; old != nil {
		a.mu.Unlock()
		a.stop(np)
		return a.writeTurnContext(ctx, old, text, opts.Images, opts.Attachments, opts)
	}
	a.procs[id] = np
	a.mu.Unlock()
	return a.writeTurnContext(ctx, np, text, opts.Images, opts.Attachments, opts)
}

// Cancellation may reject a new turn until its final admission guard. Once
// writeTurn has reserved/published that turn, a dropped phone command context
// must not stop it, roll it back, or silently resend an attempted pipe write.
func claudeTurnOptionsForContext(ctx context.Context, opts TurnOptions) TurnOptions {
	permissionGuard := opts.ValidatePermission
	opts.ValidatePermission = func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if permissionGuard != nil {
			if err := permissionGuard(); err != nil {
				return err
			}
		}
		return ctx.Err()
	}
	return opts
}

// Retiring an idle worker and admitting a new prompt must use the same locks.
// A separate idle check followed by stop could kill a turn accepted between
// those operations. Set the fence here; pipe/process cleanup stays outside the
// locks because it can block. Merely waiting for the writer is not admission.
func (a *claudeAgent) retireIdleClaudeWorker(p *claudeProc) error {
	// Registry callbacks may inspect vendor state, so do not nest its lock.
	// Any helper arriving after this hint is still fenced by stopped/lease
	// retirement; the turn counter below is the atomic work-admission guard.
	pending := a.aps.countFor("claude:"+p.id) > 0
	a.mu.Lock()
	defer a.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	if a.closed || a.procs[p.id] != p || p.stopped || p.transportRetired() {
		return errors.New("Claude Code 连接已变更，请稍后重试")
	}
	if p.turns > 0 || len(p.turnIDs) > 0 || p.activeSubtasks.Load() || p.contextCompacting || pending {
		return errors.New("Claude Code 正在处理任务，结束后再切换 API 或模型")
	}
	p.stopped = true
	p.permissionLease.revoke()
	p.elicitationHook.revoke()
	return nil
}

// claudeImageBlock embeds a phone image as a base64 content block.
func claudeImageBlock(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("读取图片失败")
	}
	media := "image/jpeg"
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		media = "image/png"
	case ".webp":
		media = "image/webp"
	}
	return map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": media, "data": base64.StdEncoding.EncodeToString(b)}}, nil
}

func (a *claudeAgent) Interrupt(ctx context.Context, id string) error {
	return ErrExpectedTurnRequired
}

// InterruptTurn is deliberately narrower than Claude Code's process stop:
// stream-json has no public per-turn interrupt.  We only terminate a worker
// when the requested turn is the sole admitted turn and has no child work. A
// queued worker is rejected rather than killing the queue (which could stop a
// different turn). The process exit is observed before reporting stopped.
func (a *claudeAgent) InterruptTurn(ctx context.Context, id, expected, operation string) (protocol.InterruptResult, error) {
	out := protocol.InterruptResult{TurnID: expected}
	if !ValidTurnID(expected) {
		return out, ErrExpectedTurnRequired
	}
	if !ValidControlOperationID(operation) {
		return out, errors.New("turn_control_operation_required: 操作标识无效")
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	a.mu.Lock()
	p := a.procs[id]
	info := a.live[id]
	if p == nil {
		a.mu.Unlock()
		if info.LastTerminalTurnID == expected {
			out.Status = "already_finished"
			return out, nil
		}
		return out, ErrTurnControlStale
	}
	p.mu.Lock()
	active := !a.closed && len(p.turnIDs) == 1 && p.turnIDs[0] == expected && p.turns == 1 && !p.stopped && !p.transportRetired() && !p.activeSubtasks.Load()
	if active {
		// This is the interrupt CAS. No other turn may be admitted after the
		// precise-turn check but before stop closes this worker's transport.
		p.stopped = true
		p.permissionLease.revoke()
		p.elicitationHook.revoke()
	}
	p.mu.Unlock()
	a.mu.Unlock()
	if !active {
		if info.LastTerminalTurnID == expected {
			out.Status = "already_finished"
			return out, nil
		}
		return out, ErrTurnControlStale
	}
	// stop() is idempotent and fences the worker before closing its pipes. No
	// successor can be admitted until finishExit confirms this exact process.
	a.stop(p)
	select {
	case <-p.done:
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if p.processExitConfirmed() {
			out.Status = "stopped"
			return out, nil
		}
		out.Status = "unconfirmed"
		return out, nil
	case <-ctx.Done():
		out.Status = "unconfirmed"
		return out, nil
	}
}

func (a *claudeAgent) Close() {
	a.startMu.Lock()
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		a.startMu.Unlock()
		return
	}
	a.closed = true
	close(a.closeCh)
	procs := make([]*claudeProc, 0, len(a.workers))
	for p := range a.workers {
		procs = append(procs, p)
	}
	a.mu.Unlock()
	a.startMu.Unlock()
	a.aps.cancelAll()
	a.thr.Close()
	var stops sync.WaitGroup
	for _, p := range procs {
		stops.Add(1)
		go func(p *claudeProc) { defer stops.Done(); a.stop(p) }(p)
	}
	stopped := make(chan struct{})
	go func() { stops.Wait(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
	}
	// Readers are closed by stop; the process reaper itself is not part of this
	// group. A non-cooperative callback also must not hang desktop shutdown.
	readers := make(chan struct{})
	go func() { a.wg.Wait(); close(readers) }()
	select {
	case <-readers:
	case <-time.After(5 * time.Second):
	}
}

// WatchdogSessions returns active turns from workers already owned by this
// process. It avoids scanning the transcript history on every watchdog poll.
func (a *claudeAgent) WatchdogSessions(ctx context.Context) ([]protocol.SessionInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	type candidate struct {
		info   protocol.SessionInfo
		turnID string
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil, nil
	}
	candidates := make([]candidate, 0, len(a.procs))
	for id, p := range a.procs {
		info, ok := a.live[id]
		if !ok || info.SessionKey == "" {
			continue
		}
		p.mu.Lock()
		active := !p.stopped && !p.transportRetired() && p.turns > 0 && len(p.turnIDs) > 0
		turnID := ""
		if active {
			turnID = p.turnIDs[0]
		}
		p.mu.Unlock()
		if active && ValidTurnID(turnID) {
			candidates = append(candidates, candidate{info: info, turnID: turnID})
		}
	}
	a.mu.Unlock()
	out := make([]protocol.SessionInfo, 0, len(candidates))
	for _, item := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		item.info.ActiveTurnID = item.turnID
		item.info.Status = "running"
		if a.aps.countFor(item.info.SessionKey) > 0 {
			item.info.Status = "waiting_approval"
		}
		out = append(out, item.info)
	}
	return out, nil
}

// Sessions merges transcripts on disk with sessions the bridge is running.
func (a *claudeAgent) Sessions(ctx context.Context) ([]protocol.SessionInfo, error) {
	list, err := a.hist.listContext(ctx)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	seen := map[string]bool{}
	for i := range list {
		id := strings.TrimPrefix(list[i].SessionKey, "claude:")
		seen[id] = true
		a.decorateLocked(id, &list[i])
	}
	for id, info := range a.live {
		if !seen[id] {
			a.decorateLocked(id, &info)
			list = append(list, info)
		}
	}
	a.mu.Unlock()
	sortSessions(list)
	if len(list) > maxSessions {
		list = list[:maxSessions]
	}
	return list, nil
}

// decorateLocked fills status/controllable from bridge processes and file activity.
func (a *claudeAgent) decorateLocked(id string, info *protocol.SessionInfo) {
	if p := a.procs[id]; p != nil {
		live := a.live[id]
		if live.Title != "" && info.Title == "" {
			info.Title = live.Title
		}
		if live.Model != "" && info.Model == "" {
			info.Model = live.Model
		}
		// History rows are merged with the live worker below.  Preserve the
		// worker's exact turn boundaries instead of returning the transcript's
		// older (usually empty) metadata; phone Stop/Continue must never invent
		// a target from a stale history snapshot.
		info.ActiveTurnID = live.ActiveTurnID
		info.LastTerminalTurnID = live.LastTerminalTurnID
		p.mu.Lock()
		info.CollaborationMode = p.collaborationMode
		busy := p.turns > 0 || p.activeSubtasks.Load()
		p.mu.Unlock()
		info.Status = "idle"
		if busy {
			info.Status = "running"
		}
		if a.aps.countFor(info.SessionKey) > 0 {
			info.Status = "waiting_approval"
		}
		info.Controllable = true
		return
	}
	// An external transcript is never controllable merely because it is old.
	// Only a process-local, Wait-confirmed completion lease can establish idle;
	// a recent write is a busy hint and all other states remain unknown.
	info.ActiveTurnID, info.LastTerminalTurnID = "", ""
	if state, controllable := a.hist.remoteHistoryState(id, info.UpdatedAt); state != "" {
		info.Status, info.Controllable = state, controllable
		if controllable {
			// A confirmed stop removes the worker, but its terminal ID remains
			// necessary for phone reconnect/Continue. Only merge that receipt
			// while the exact owned transcript is still unchanged.
			live := a.live[id]
			info.LastTerminalTurnID = live.LastTerminalTurnID
			if live.UpdatedAt > info.UpdatedAt {
				info.UpdatedAt = live.UpdatedAt
			}
		}
	} else {
		info.Status, info.Controllable = "unknown", false
	}
}

func (a *claudeAgent) Open(ctx context.Context, id string) (protocol.SessionInfo, []protocol.Event, error) {
	info, evs, err := a.hist.open(id)
	if err != nil {
		a.mu.Lock()
		live, ok := a.live[id]
		a.mu.Unlock()
		if !ok {
			return protocol.SessionInfo{}, nil, err
		}
		info, evs = live, nil
	}
	a.mu.Lock()
	a.decorateLocked(id, &info)
	a.mu.Unlock()
	pending := a.aps.snapshot(info.SessionKey)
	if len(pending) > 0 {
		info.Status = "waiting_approval"
	}
	return info, historyWithApprovals(evs, pending), nil
}

// ClaimDesktopImport reserves one import attempt for a phone-created session.
// Calling a URL handler does not prove that Desktop opened the intended chat.
// Do not retry automatically after an uncertain handler outcome: a second
// claude://resume can create another tab. Restart does not recreate this lease.
func (a *claudeAgent) ClaimDesktopImport(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || !a.fromPhone[id] {
		return false
	}
	delete(a.fromPhone, id)
	return true
}
