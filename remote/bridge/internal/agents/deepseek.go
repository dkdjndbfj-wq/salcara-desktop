package agents

// DeepSeek Harness uses its documented SDK JSON-RPC stdio profile. Each
// Bridge-owned session has a separate runtime: the SDK has no cancel method,
// so stopping that runtime must never stop another session's work.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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

const deepSeekID = "deepseek-harness"

type deepSeekAgent struct {
	settings func() Settings
	stateDir string
	boot     deepSeekBootProof
	mu       sync.Mutex
	sink     Sink
	sessions map[string]*deepSeekSession
	closed   bool
}

type deepSeekSession struct {
	control        sync.Mutex
	stopControl    sync.Mutex
	Info           protocol.SessionInfo `json:"info"`
	Events         []protocol.Event     `json:"events"`
	Started        bool                 `json:"started"`
	Policy         string               `json:"policy"`
	Unconfirmed    bool                 `json:"runtimeUnconfirmed,omitempty"`
	RuntimePending bool                 `json:"runtimePending,omitempty"`
	RuntimeBoot    deepSeekBootProof    `json:"runtimeBoot,omitempty"`
	conn           *rpcConn
	stoppedConn    *rpcConn
	source         [32]byte
	busy           bool
	failedTurn     bool
	nativeStatus   string
	runtimeID      string
	children       map[string]bool
	unknownChild   bool
	approvals      map[string]deepSeekApproval
}

type deepSeekApproval struct {
	rpcID  json.RawMessage
	conn   *rpcConn
	source [32]byte
}

func newDeepSeekAgent(settings func() Settings, stateDir string) *deepSeekAgent {
	return newDeepSeekAgentWithBoot(settings, stateDir, deepSeekCurrentBoot())
}

func newDeepSeekAgentWithBoot(settings func() Settings, stateDir string, boot deepSeekBootProof) *deepSeekAgent {
	a := &deepSeekAgent{settings: settings, stateDir: filepath.Join(stateDir, deepSeekID), boot: boot, sessions: map[string]*deepSeekSession{}}
	if stateDir == "" {
		a.stateDir = ""
		return a
	}
	entries, _ := os.ReadDir(a.stateDir)
	for _, entry := range entries {
		if !entry.IsDir() || !validDeepSeekSessionID(entry.Name()) {
			continue
		}
		path := filepath.Join(a.stateDir, entry.Name(), "session.json")
		st, err := os.Stat(path)
		if err != nil || st.Size() > 8<<20 {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var s deepSeekSession
		if json.Unmarshal(data, &s) != nil || s.Info.SessionKey != deepSeekID+":"+entry.Name() || s.Info.Tool != deepSeekID {
			continue
		}
		if !filepath.IsAbs(s.Info.Cwd) {
			continue
		}
		s.approvals = map[string]deepSeekApproval{}
		if len(s.Events) > 400 {
			s.Events = s.Events[len(s.Events)-400:]
		}
		filtered := s.Events[:0]
		for _, e := range s.Events {
			if e.Tool == deepSeekID && e.SessionKey == s.Info.SessionKey && e.Type != "approval.request" {
				filtered = append(filtered, e)
			}
		}
		s.Events = filtered
		a.restoreDeepSeekRuntime(entry.Name(), &s)
		a.sessions[entry.Name()] = &s
	}
	return a
}

func (*deepSeekAgent) ID() string   { return deepSeekID }
func (*deepSeekAgent) Name() string { return "DeepSeek Harness" }
func (a *deepSeekAgent) Descriptor() protocol.AgentDescriptor {
	return protocol.AgentDescriptor{ID: a.ID(), Name: a.Name(), Tool: a.ID(), APIFamily: a.ID(), ControlSurface: "cli", Capabilities: protocol.AgentCapabilities{
		List: true, Read: true, Send: true, Interrupt: true, PreciseInterrupt: true, Approval: true, ModelOverride: true,
	}}
}
func (a *deepSeekAgent) BindEventSink(sink Sink) { a.mu.Lock(); a.sink = sink; a.mu.Unlock() }
func (a *deepSeekAgent) Detect(ctx context.Context) protocol.Tool {
	p, ok := findExecutable("dsh", a.settings().DeepSeekPath)
	t := protocol.Tool{ID: a.ID(), Name: a.Name(), Available: ok}
	if ok {
		t.Version, t.Available = deepSeekVersion(ctx, p)
	}
	return t
}
func (a *deepSeekAgent) Models(ctx context.Context) []string {
	m := settingsForContext(ctx, a.settings).Extensions[deepSeekID].Model
	if m == "" {
		return nil
	}
	return []string{m}
}

// API provenance cannot change while SDK-owned work or a pending approval is
// still active, including a process tree whose forced exit was not confirmed.
func (a *deepSeekAgent) ActiveRemoteTurns() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.sessions {
		if s.busy || s.Unconfirmed || len(s.approvals) != 0 {
			return true
		}
	}
	return false
}
func (a *deepSeekAgent) Sessions(context.Context) ([]protocol.SessionInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]protocol.SessionInfo, 0, len(a.sessions))
	for _, s := range a.sessions {
		out = append(out, s.Info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	return out, nil
}
func (a *deepSeekAgent) Open(ctx context.Context, id string) (protocol.SessionInfo, []protocol.Event, error) {
	info, events, _, err := a.OpenPage(ctx, id, "", maxHistoryEvents)
	return info, events, err
}

func extensionSource(s ExtensionSettings, model string) [32]byte {
	return sha256.Sum256([]byte(s.Root + "\x00" + s.Key + "\x00" + model))
}
func validDeepSeekSessionID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}
func deepSeekNewID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(id[:])
}

func (a *deepSeekAgent) Start(ctx context.Context, cwd, prompt, model, approval string) (string, error) {
	return a.start(ctx, cwd, prompt, model, approval, nil)
}

func deepSeekOptions(opts TurnOptions) error {
	if opts.Effort != "" || opts.PermissionMode != "" || opts.ClaudeApproval != "" || opts.HasFeatures() || len(opts.ContextReferences) != 0 || len(opts.Images) != 0 || len(opts.Attachments) != 0 || opts.CrossTaskOrigin != nil || opts.ClientUserMessageID != "" || opts.ExpectedTerminalTurnID != "" || opts.ControlOperationID != "" || opts.Desktop {
		return errors.New("当前 DeepSeek Harness 适配器仅支持文本和模型选择")
	}
	if len(opts.Model) > 200 || strings.TrimSpace(opts.Model) != opts.Model || strings.ContainsAny(opts.Model, "\r\n\x00") {
		return errors.New("模型名称无效")
	}
	return nil
}

func (a *deepSeekAgent) StartWithOptions(ctx context.Context, cwd, prompt, approval string, opts TurnOptions) (string, error) {
	if err := deepSeekOptions(opts); err != nil {
		return "", err
	}
	return a.start(ctx, cwd, prompt, opts.Model, approval, opts.ValidatePermission)
}

func (a *deepSeekAgent) start(ctx context.Context, cwd, prompt, model, approval string, validate func() error) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if strings.TrimSpace(prompt) == "" || len(prompt) > 4<<20 {
		return "", errors.New("消息为空或超过大小限制")
	}
	if err := deepSeekOptions(TurnOptions{Model: model}); err != nil {
		return "", err
	}
	if validate != nil {
		if err := validate(); err != nil {
			return "", err
		}
	}
	if !filepath.IsAbs(cwd) {
		return "", errors.New("DeepSeek Harness 工作目录必须为绝对路径")
	}
	if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		return "", errors.New("DeepSeek Harness 工作目录不存在")
	}
	settings := settingsForContext(ctx, a.settings)
	if model == "" {
		model = settings.Extensions[deepSeekID].Model
	}
	if model == "" {
		return "", errors.New("请先为 DeepSeek Harness 选择模型")
	}
	if approval == "" {
		approval = settings.Approval
	}
	if approval == "" {
		approval = "ask"
	}
	if approval != "ask" && approval != "auto_edits" && approval != "auto_all" {
		return "", errors.New("不支持的 DeepSeek Harness 审批策略")
	}
	if a.stateDir == "" {
		return "", errors.New("DeepSeek Harness 会话存储不可用")
	}
	id := deepSeekNewID()
	if id == "" {
		return "", errors.New("无法生成会话标识")
	}
	s := &deepSeekSession{Info: protocol.SessionInfo{SessionKey: deepSeekID + ":" + id, Tool: deepSeekID, Client: "salcara", ControlSurface: "cli", Cwd: cwd, Model: model, Title: truncHead(prompt, 100), Status: "idle", Controllable: true, UpdatedAt: time.Now().UnixMilli()}, Policy: approval, approvals: map[string]deepSeekApproval{}}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return "", errors.New("Agent 已关闭")
	}
	a.sessions[id] = s
	if err := a.persistLocked(id, s); err != nil {
		delete(a.sessions, id)
		a.mu.Unlock()
		return "", errors.New("无法保存 DeepSeek Harness 会话：" + err.Error())
	}
	a.mu.Unlock()
	if err := a.send(ctx, id, prompt, TurnOptions{ValidatePermission: validate}); err != nil {
		a.mu.Lock()
		created := s.Started
		a.mu.Unlock()
		if created {
			return id, err
		}
		return "", err
	}
	return id, nil
}

func (a *deepSeekAgent) Send(ctx context.Context, id, text string) error {
	return a.send(ctx, id, text, TurnOptions{})
}

func (a *deepSeekAgent) SendWithOptions(ctx context.Context, id, text string, opts TurnOptions) error {
	if err := deepSeekOptions(opts); err != nil {
		return err
	}
	return a.send(ctx, id, text, opts)
}

func (a *deepSeekAgent) KnownSessionModel(id string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.sessions[id]; s != nil {
		return s.Info.Model
	}
	return ""
}

func (a *deepSeekAgent) SessionRequiresFullAccessConsent(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.sessions[id]
	return s != nil && (s.Policy == "" || s.Policy == "auto_all")
}

func (a *deepSeekAgent) send(ctx context.Context, id, text string, opts TurnOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(text) == "" || len(text) > 4<<20 {
		return errors.New("消息为空或超过大小限制")
	}
	a.mu.Lock()
	s := a.sessions[id]
	a.mu.Unlock()
	if s == nil {
		return errors.New("DeepSeek Harness 会话不存在")
	}
	s.control.Lock()
	defer s.control.Unlock()
	if opts.ValidatePermission != nil {
		if err := opts.ValidatePermission(); err != nil {
			return err
		}
	}
	a.mu.Lock()
	if a.closed || s.busy || s.Unconfirmed {
		a.mu.Unlock()
		return errors.New("会话已关闭或仍在运行，请先停止当前任务")
	}
	previousInfo, previousBoot := s.Info, s.RuntimeBoot
	s.busy = true
	s.failedTurn = false
	if opts.Model != "" {
		s.Info.Model = opts.Model
	}
	s.Info.Status = "running"
	s.Info.UpdatedAt = time.Now().UnixMilli()
	s.RuntimeBoot = a.boot
	// A prompt cannot reach an existing or a new runtime before its restart
	// fence is durable. session.updated is a live notification, not a disk write.
	if err := a.persistDeepSeekFenceLocked(id, s); err != nil {
		s.busy, s.Info = false, previousInfo
		s.RuntimeBoot = previousBoot
		a.mu.Unlock()
		return errors.New("无法保存 DeepSeek 运行状态，任务未发送：" + err.Error())
	}
	info := s.Info
	a.mu.Unlock()
	a.emit(id, protocol.Event{Type: "session.updated", Session: &info})
	settings := settingsForContext(ctx, a.settings)
	conn, err := a.runtime(ctx, id, s, settings)
	if err == nil {
		var receipt struct {
			MessageID string `json:"messageId"`
		}
		err = conn.callGuarded(ctx, "session/prompt", map[string]any{"sessionId": id, "contentBlocks": []any{map[string]string{"type": "text", "text": text}}}, &receipt, opts.ValidatePermission)
		if err == nil && receipt.MessageID == "" {
			// A successful RPC without its required receipt is not proof that
			// the prompt was unsent. Preserve this identity just like a lost reply.
			err = &rpcDeliveryUncertainError{message: "DeepSeek Harness 未返回消息接收凭据，操作可能已执行；请核对原会话，不会自动重发"}
		}
		var uncertain *rpcDeliveryUncertainError
		if err != nil && errors.As(err, &uncertain) {
			// Do not create a replacement identity after a possibly delivered
			// prompt. The next attempt must restore this exact native session.
			a.mu.Lock()
			s.Started = true
			a.mu.Unlock()
		}
	}
	if err != nil {
		if conn != nil {
			_ = a.stopRuntime(s, conn)
		}
		a.finish(id, s, conn, "failed", "DeepSeek Harness 请求失败："+a.safeError(err, settings.Extensions[deepSeekID].Key))
		return &deepSeekRedactedError{text: a.safeError(err, settings.Extensions[deepSeekID].Key), cause: err}
	}
	a.mu.Lock()
	s.Started = true
	a.persistLocked(id, s)
	a.mu.Unlock()
	// session/wait includes managed descendants; an idle notification alone is
	// not sufficient to admit another task on the same session.
	go a.waitRuntime(id, s, conn)
	return nil
}

func (a *deepSeekAgent) waitRuntime(id string, s *deepSeekSession, conn *rpcConn) {
	err := conn.Call(context.Background(), "session/wait", map[string]string{"sessionId": id}, nil)
	if err != nil {
		_ = a.stopRuntime(s, conn)
		a.finish(id, s, conn, "failed", "DeepSeek Harness 运行中断，请检查会话")
	} else {
		a.mu.Lock()
		failed := s.failedTurn
		a.mu.Unlock()
		status := "idle"
		if failed {
			status = "failed"
		}
		a.finish(id, s, conn, status, "")
	}
}

func (a *deepSeekAgent) runtime(ctx context.Context, id string, s *deepSeekSession, settings Settings) (*rpcConn, error) {
	api := settings.Extensions[deepSeekID]
	if api.Key == "" || api.Root == "" {
		return nil, errors.New("请先在电脑为 DeepSeek Harness 应用 API 或在手机选择 API")
	}
	root, _, err := config.APIBase(api.Root)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	old, sig, model, cwd, started, policy := s.conn, s.source, s.Info.Model, s.Info.Cwd, s.Started, s.Policy
	a.mu.Unlock()
	source := extensionSource(api, model)
	if old != nil && old.alive() && sig == source {
		return old, nil
	}
	if old != nil {
		a.mu.Lock()
		if s.conn == old {
			s.conn = nil
		}
		a.mu.Unlock()
		if err := a.stopRuntime(s, old); err != nil {
			return nil, err
		}
	}
	if !started {
		// A process may have saved native history before Bridge saved the prompt
		// receipt. Never create over that identity or replay its earlier prompt.
		exists, err := a.deepSeekNativeSessionExists(ctx, id)
		if err != nil {
			return nil, err
		}
		if exists {
			a.mu.Lock()
			s.Started = true
			err = a.persistLocked(id, s)
			a.mu.Unlock()
			if err != nil {
				return nil, errors.New("无法保存 DeepSeek 原生会话身份：" + err.Error())
			}
			started = true
		}
	}
	exe, ok := findExecutable("dsh", settings.DeepSeekPath)
	if !ok {
		return nil, errors.New("未找到 DeepSeek Harness；请先安装 @deepseek-ai/dsh")
	}
	dir := filepath.Join(a.stateDir, id)
	if err := os.MkdirAll(filepath.Join(dir, "native"), 0o700); err != nil {
		return nil, err
	}
	patch, err := json.Marshal([]any{map[string]any{"id": "llm-pi-ai", "config": map[string]any{"providers": map[string]any{"salcara": map[string]any{"apiKeyEnv": "SALCARA_DSH_API_KEY", "api": "openai-completions", "baseURL": strings.TrimRight(root, "/") + "/v1", "models": []any{map[string]string{"id": model}}}}}}})
	if err != nil {
		return nil, err
	}
	patchPath := filepath.Join(dir, "gateway.patch.yml")
	if err := os.WriteFile(patchPath, patch, 0o600); err != nil {
		return nil, err
	}
	cmd, err := DeepSeekCommand(ctx, exe, "--profile", "sdk", "--patch", patchPath)
	if err != nil {
		return nil, err
	}
	mode := "workspace-write"
	if policy == "ask" {
		mode = "read-only"
	}
	if policy == "auto_all" {
		mode = "danger-full-access"
	}
	cmd.Dir = cwd
	cmd.Env = childEnv(exe, map[string]string{"DSH_HOME": filepath.Join(dir, "native"), "DSH_PERMISSION_MODE": mode, "SALCARA_DSH_API_KEY": api.Key}, []string{"DEEPSEEK_API_KEY", "DEEPSEEK_BASE_URL"})
	prepareCmd(cmd)
	// Persist ownership before starting the process, even while it is idle or
	// initializing. A later Bridge crash must not mistake it for a stopped writer.
	a.mu.Lock()
	s.RuntimePending, s.RuntimeBoot = true, a.boot
	err = a.persistDeepSeekFenceLocked(id, s)
	a.mu.Unlock()
	if err != nil {
		return nil, errors.New("无法保存 DeepSeek 进程隔离状态，未启动工具：" + err.Error())
	}
	var conn *rpcConn
	ready := make(chan struct{})
	conn, err = startRPCProtocol(cmd, "2.0", func(method string, raw json.RawMessage) { <-ready; a.notify(id, s, conn, method, raw) }, func(rpcID json.RawMessage, method string, raw json.RawMessage) {
		go func() { <-ready; a.request(id, s, conn, rpcID, method, raw) }()
	})
	if err != nil {
		// startRPCProtocol returns an error only before cmd.Start succeeds.
		// No process owns this reservation; keep storage failures fenced.
		_ = a.confirmDeepSeekRuntimeStopped(s, nil)
		return nil, err
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		close(ready)
		_ = a.stopRuntime(s, conn)
		return nil, errors.New("Agent 已关闭")
	}
	s.conn, s.source = conn, source
	s.runtimeID, s.children, s.unknownChild = deepSeekNewID(), map[string]bool{}, false
	a.mu.Unlock()
	close(ready)
	initCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var init struct {
		ServerInfo struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	err = conn.Call(initCtx, "initialize", map[string]any{"cwd": cwd, "provider": "salcara", "model": model, "approvalRequests": true}, &init)
	if err == nil && init.ServerInfo.Name != "deepseek-harness-sdk-runtime" {
		err = errors.New("DeepSeek Harness SDK 身份不匹配，请更新工具")
	}
	if err == nil && started {
		err = conn.Call(initCtx, "session/resume", map[string]string{"sessionId": id}, nil)
	}
	if err != nil {
		_ = a.stopRuntime(s, conn)
		return conn, err
	}
	go func() {
		<-conn.done
		a.mu.Lock()
		active := s.conn == conn
		a.mu.Unlock()
		if active {
			_ = a.stopRuntime(s, conn)
			a.finish(id, s, conn, "failed", "DeepSeek Harness 进程已退出")
		}
	}()
	return conn, nil
}

func (a *deepSeekAgent) safeError(err error, key string) string {
	text := strings.ReplaceAll(err.Error(), "Codex", "DeepSeek Harness")
	if key != "" {
		text = strings.ReplaceAll(text, key, "[redacted]")
	}
	return truncHead(text, 1000)
}

type deepSeekRedactedError struct {
	text  string
	cause error
}

func (e *deepSeekRedactedError) Error() string { return e.text }
func (e *deepSeekRedactedError) Unwrap() error { return e.cause }

func (a *deepSeekAgent) finish(id string, s *deepSeekSession, conn *rpcConn, status, detail string) {
	a.mu.Lock()
	if conn != nil && s.conn != conn {
		a.mu.Unlock()
		return
	}
	// A new native activation can follow the wait response before its Go
	// waiter resumes. Preserve that work's admission fence and observe again.
	if conn != nil && detail == "" && s.nativeStatus == "running" && !a.closed {
		a.mu.Unlock()
		go a.waitRuntime(id, s, conn)
		return
	}
	s.busy = false
	s.Info.Status = status
	if s.Unconfirmed {
		s.Info.Status = "unknown"
		s.Info.Controllable = false
	}
	s.Info.ActiveTurnID = ""
	s.Info.UpdatedAt = time.Now().UnixMilli()
	pending := s.approvals
	s.approvals = map[string]deepSeekApproval{}
	info := s.Info
	a.persistLocked(id, s)
	a.mu.Unlock()
	for approval := range pending {
		a.emit(id, protocol.Event{Type: "approval.resolved", ApprovalID: approval, Decision: "deny"})
	}
	if detail != "" {
		a.emit(id, protocol.Event{Type: "notice", Level: "error", Text: detail})
	}
	a.emit(id, protocol.Event{Type: "session.updated", Session: &info})
}

func (a *deepSeekAgent) stopRuntime(s *deepSeekSession, conn *rpcConn) error {
	s.stopControl.Lock()
	defer s.stopControl.Unlock()
	a.mu.Lock()
	if s.stoppedConn == conn && conn.rootProcessExitConfirmed() && !conn.treeTermination.unconfirmed() {
		// A late waiter for the retired runtime must not touch a successor's
		// pre-spawn reservation while its connection slot is still nil.
		a.mu.Unlock()
		return nil
	}
	if s.conn != nil && s.conn != conn {
		a.mu.Unlock()
		return errors.New("DeepSeek 运行实例已变化")
	}
	// Transport/root exit alone must not briefly admit an API change while
	// we still lack descendant-exit evidence.
	s.Unconfirmed = true
	a.mu.Unlock()
	if conn.rootProcessExitConfirmed() && !conn.treeTermination.unconfirmed() {
		return errors.New("DeepSeek 主进程已退出，但子进程退出未确认；仍保留隔离")
	}
	// The SDK shutdown request cancels owned work and flushes native session
	// storage. A bounded forced tree stop remains the fallback for a hung SDK.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	err := conn.Call(shutdownCtx, "shutdown", nil, nil)
	cancel()
	if err == nil {
		select {
		case <-conn.processWaitDone:
		case <-time.After(2 * time.Second):
		}
		if conn.rootProcessExitConfirmed() && !conn.treeTermination.unconfirmed() {
			return a.confirmDeepSeekRuntimeStopped(s, conn)
		}
	}
	treeStopped := false
	conn.killWithTreeKiller(func(cmd *exec.Cmd) error {
		err := killTreeResult(cmd)
		treeStopped = err == nil
		return err
	})
	select {
	case <-conn.processWaitDone:
	case <-time.After(4 * time.Second):
	}
	if !treeStopped || conn.treeTermination.unconfirmed() || !conn.rootProcessExitConfirmed() {
		a.mu.Lock()
		s.Unconfirmed = true
		a.mu.Unlock()
		return errors.New("DeepSeek Harness 进程树退出未确认，已禁止继续启动；请在电脑检查")
	}
	return a.confirmDeepSeekRuntimeStopped(s, conn)
}

func (a *deepSeekAgent) Interrupt(context.Context, string) error { return ErrExpectedTurnRequired }

func (a *deepSeekAgent) InterruptTurn(ctx context.Context, id, expected, operation string) (protocol.InterruptResult, error) {
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
	s := a.sessions[id]
	a.mu.Unlock()
	if s == nil {
		return out, ErrTurnControlStale
	}
	s.control.Lock()
	defer s.control.Unlock()
	a.mu.Lock()
	if s.Info.LastTerminalTurnID == expected && s.Info.ActiveTurnID != expected {
		a.mu.Unlock()
		out.Status = "already_finished"
		return out, nil
	}
	if a.closed || !s.busy || s.conn == nil || s.Info.ActiveTurnID != expected || len(s.children) != 0 || s.unknownChild || s.Unconfirmed {
		a.mu.Unlock()
		return out, ErrTurnControlStale
	}
	conn := s.conn
	// Retire before releasing the turn CAS. No new prompt can be admitted
	// under control, and this runtime's late callbacks cannot target its successor.
	s.conn = nil
	a.mu.Unlock()
	if err := a.stopRuntime(s, conn); err != nil {
		a.finish(id, s, nil, "failed", err.Error())
		out.Status = "unconfirmed"
		return out, nil
	}
	a.mu.Lock()
	s.Info.LastTerminalTurnID = expected
	a.mu.Unlock()
	a.finish(id, s, nil, "idle", "")
	a.emit(id, protocol.Event{Type: "turn", Status: "interrupted", TurnID: expected})
	out.Status = "stopped"
	return out, nil
}

func (a *deepSeekAgent) stopSession(id string) error {
	a.mu.Lock()
	s := a.sessions[id]
	a.mu.Unlock()
	if s == nil {
		return errors.New("DeepSeek Harness 会话不存在")
	}
	s.control.Lock()
	defer s.control.Unlock()
	a.mu.Lock()
	conn, busy := s.conn, s.busy
	s.conn = nil
	a.mu.Unlock()
	if conn != nil {
		if err := a.stopRuntime(s, conn); err != nil {
			a.finish(id, s, nil, "failed", err.Error())
			return err
		}
	}
	a.finish(id, s, nil, "idle", "")
	if busy {
		a.emit(id, protocol.Event{Type: "turn", Status: "interrupted"})
	}
	return nil
}
func (a *deepSeekAgent) Close() {
	a.mu.Lock()
	a.closed = true
	ids := make([]string, 0, len(a.sessions))
	owned := map[*deepSeekSession]*rpcConn{}
	for id := range a.sessions {
		ids = append(ids, id)
		if conn := a.sessions[id].conn; conn != nil {
			owned[a.sessions[id]] = conn
		}
	}
	a.mu.Unlock()
	// Interrupt startup handshakes before waiting on their control mutex.
	for session, conn := range owned {
		_ = a.stopRuntime(session, conn)
	}
	for _, id := range ids {
		_ = a.stopSession(id)
	}
}

func (a *deepSeekAgent) persistLocked(id string, s *deepSeekSession) error {
	if a.stateDir == "" {
		return errors.New("会话存储目录不可用")
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	for len(data) > 8<<20 && len(s.Events) > 1 {
		s.Events = append([]protocol.Event(nil), s.Events[len(s.Events)/2:]...)
		data, err = json.Marshal(s)
		if err != nil {
			return err
		}
	}
	if len(data) > 8<<20 {
		return errors.New("会话记录过大")
	}
	dir := filepath.Join(a.stateDir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "session.json.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "session.json"))
}

func (a *deepSeekAgent) emit(id string, e protocol.Event) {
	a.emitFrom(id, e, nil)
}

func (a *deepSeekAgent) emitFrom(id string, e protocol.Event, expected *rpcConn) {
	// Native storage retains full content. The phone transcript is a bounded
	// view, including when a tool returns a very large generated document.
	e.Text, e.Detail, e.Output = truncHead(e.Text, 256<<10), truncHead(e.Detail, 128<<10), truncHead(e.Output, 256<<10)
	e.Title, e.Error = truncHead(e.Title, 4096), truncHead(e.Error, 16<<10)
	e.Tool, e.SessionKey = deepSeekID, deepSeekID+":"+id
	if e.TS == 0 {
		e.TS = time.Now().UnixMilli()
	}
	a.mu.Lock()
	s := a.sessions[id]
	if s == nil || expected != nil && (s.conn != expected || a.closed) {
		a.mu.Unlock()
		return
	}
	if e.Type == "session.updated" {
		info := s.Info
		e.Session = &info
	}
	if e.Type != "session.updated" {
		s.Events = append(s.Events, e)
		if len(s.Events) > 400 {
			s.Events = append([]protocol.Event(nil), s.Events[len(s.Events)-400:]...)
		}
		a.persistLocked(id, s)
	}
	sink := a.sink
	a.mu.Unlock()
	if sink != nil {
		sink(e)
	}
}

func (a *deepSeekAgent) notify(id string, s *deepSeekSession, conn *rpcConn, method string, raw json.RawMessage) {
	a.mu.Lock()
	current := s.conn == conn && !a.closed
	a.mu.Unlock()
	if !current {
		return
	}
	var p struct {
		SessionID string   `json:"sessionId"`
		Status    string   `json:"status"`
		Event     dshEvent `json:"event"`
		ParentID  string   `json:"parentSessionId"`
		ChildID   string   `json:"childSessionId"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	if method == "subagent.started" || method == "subagent.finished" {
		a.mu.Lock()
		if s.conn == conn && (p.ParentID == id || s.children[p.ParentID]) && p.ChildID != "" {
			if method == "subagent.started" {
				if len(s.children) >= 1024 {
					s.unknownChild = true
				} else {
					s.children[p.ChildID] = true
				}
			} else {
				delete(s.children, p.ChildID)
			}
		}
		a.mu.Unlock()
		return
	}
	if p.SessionID != id {
		if method == "session.status" && p.Status == "running" {
			a.mu.Lock()
			if s.conn == conn && !s.children[p.SessionID] {
				s.unknownChild = true
			}
			a.mu.Unlock()
		}
		return
	}
	if method == "session.status" && (p.Status == "running" || p.Status == "idle") {
		a.mu.Lock()
		if s.conn != conn || a.closed {
			a.mu.Unlock()
			return
		}
		s.nativeStatus = p.Status
		observe := p.Status == "running" && !s.busy
		if p.Status == "running" {
			s.Info.Status, s.busy = "running", true
			if observe {
				s.failedTurn = false
			}
		}
		info := s.Info
		a.mu.Unlock()
		a.emitFrom(id, protocol.Event{Type: "session.updated", Session: &info}, conn)
		if observe {
			go a.waitRuntime(id, s, conn)
		}
	}
	if method != "session.event" {
		return
	}
	if p.Event.Type == "turn/start" || p.Event.Type == "user/message" {
		a.mu.Lock()
		if s.conn != conn || a.closed {
			a.mu.Unlock()
			return
		}
		s.Started = true
		a.mu.Unlock()
	}
	for _, event := range mapDeepSeekEvent(p.Event) {
		a.mu.Lock()
		if s.conn != conn || a.closed {
			a.mu.Unlock()
			return
		}
		if event.TurnID != "" {
			event.TurnID = s.runtimeID + ":" + event.TurnID
		}
		if event.Type == "turn" && event.Status == "started" {
			s.Info.ActiveTurnID = event.TurnID
		}
		if event.Type == "turn" && event.Status != "started" {
			s.Info.LastTerminalTurnID = event.TurnID
			s.Info.ActiveTurnID = ""
			s.failedTurn = event.Status == "failed"
		}
		a.mu.Unlock()
		a.emitFrom(id, event, conn)
	}
}

func (a *deepSeekAgent) request(id string, s *deepSeekSession, conn *rpcConn, rpcID json.RawMessage, method string, raw json.RawMessage) {
	a.mu.Lock()
	current, source := s.conn == conn && !s.Unconfirmed && !a.closed, s.source
	a.mu.Unlock()
	if !current {
		return
	}
	if method != "approval/request" {
		_ = conn.ReplyError(rpcID, -32601, "This host does not implement this request")
		return
	}
	var p struct {
		SessionID string `json:"sessionId"`
		ToolName  string `json:"toolName"`
		Reason    string `json:"reason"`
	}
	if json.Unmarshal(raw, &p) != nil || p.SessionID != id {
		_ = conn.Reply(rpcID, map[string]string{"outcome": "rejected"})
		return
	}
	approval := deepSeekID + ":" + id + ":" + deepSeekNewID()
	a.mu.Lock()
	if s.conn != conn || !s.busy || s.Unconfirmed || a.closed {
		a.mu.Unlock()
		_ = conn.Reply(rpcID, map[string]string{"outcome": "cancelled"})
		return
	}
	s.approvals[approval] = deepSeekApproval{rpcID: append(json.RawMessage(nil), rpcID...), conn: conn, source: source}
	s.Info.Status = "waiting_approval"
	info := s.Info
	a.mu.Unlock()
	a.emitFrom(id, protocol.Event{Type: "approval.request", ApprovalID: approval, Kind: "tool", Title: p.ToolName, Detail: p.Reason, ApprovalOptions: &protocol.ApprovalOptions{Version: 1, Decisions: []string{"allow", "deny"}}}, conn)
	a.emitFrom(id, protocol.Event{Type: "session.updated", Session: &info}, conn)
}
func (a *deepSeekAgent) Respond(approval, decision, _ string) bool {
	return a.respond(context.Background(), "", approval, decision, false)
}
func (a *deepSeekAgent) RespondForSession(ctx context.Context, sessionKey, approval string, response ApprovalResponse) bool {
	if response.Answers != nil {
		return false
	}
	return a.respond(ctx, sessionKey, approval, response.Decision, true)
}
func (a *deepSeekAgent) respond(ctx context.Context, sessionKey, approval, decision string, scoped bool) bool {
	if decision != "allow" && decision != "deny" {
		return false
	}
	parts := strings.Split(approval, ":")
	if len(parts) != 3 || parts[0] != deepSeekID {
		return false
	}
	id := parts[1]
	if scoped && sessionKey != deepSeekID+":"+id {
		return false
	}
	a.mu.Lock()
	s := a.sessions[id]
	a.mu.Unlock()
	if s == nil {
		return false
	}
	// Serialize the actual approval reply with stopping, not only its lookup.
	// A shutdown cannot race between the final admission check and the write.
	s.stopControl.Lock()
	defer s.stopControl.Unlock()
	a.mu.Lock()
	request, ok := s.approvals[approval]
	if !ok || s.conn != request.conn || !s.busy || s.Unconfirmed || a.closed || s.stoppedConn == request.conn {
		a.mu.Unlock()
		return false
	}
	model := s.Info.Model
	a.mu.Unlock()
	if scoped && extensionSource(settingsForContext(ctx, a.settings).Extensions[deepSeekID], model) != request.source {
		return false
	}
	a.mu.Lock()
	if current, ok := s.approvals[approval]; !ok || current.conn != request.conn || s.conn != request.conn || !s.busy || s.Unconfirmed || a.closed || s.stoppedConn == request.conn {
		a.mu.Unlock()
		return false
	}
	delete(s.approvals, approval)
	a.mu.Unlock()
	outcome := "rejected"
	if decision == "allow" {
		outcome = "allowed-once"
	}
	if request.conn.Reply(request.rpcID, map[string]string{"outcome": outcome}) != nil {
		return false
	}
	a.mu.Lock()
	if s.conn == request.conn && s.busy && !s.Unconfirmed && !a.closed {
		s.Info.Status = "running"
	}
	info := s.Info
	a.mu.Unlock()
	a.emit(id, protocol.Event{Type: "approval.resolved", ApprovalID: approval, Decision: decision})
	a.emit(id, protocol.Event{Type: "session.updated", Session: &info})
	return true
}

type dshEvent struct {
	Type string          `json:"type"`
	Seq  int64           `json:"seq"`
	Time int64           `json:"time"`
	Data json.RawMessage `json:"data"`
}

func mapDeepSeekEvent(raw dshEvent) []protocol.Event {
	var d struct {
		Turn      int64  `json:"turn"`
		CallID    string `json:"callId"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Message   struct {
			ID         string `json:"id"`
			Role       string `json:"role"`
			ToolCallID string `json:"toolCallId"`
			IsError    bool   `json:"isError"`
			Content    []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
		Reason struct {
			Kind  string `json:"kind"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"reason"`
		Usage *protocol.Usage `json:"usage"`
	}
	if json.Unmarshal(raw.Data, &d) != nil {
		return nil
	}
	if raw.Type == "user/message" {
		if json.Unmarshal(raw.Data, &d.Message) != nil {
			return nil
		}
		var source struct {
			Source struct {
				Kind string `json:"kind"`
			} `json:"source"`
		}
		_ = json.Unmarshal(raw.Data, &source)
		// Runtime context and injected instructions are not human messages.
		if source.Source.Kind != "user" {
			return nil
		}
	}
	e := protocol.Event{TS: raw.Time, ID: "dsh-event-" + strconv.FormatInt(raw.Seq, 10)}
	if d.Turn > 0 {
		e.TurnID = strconv.FormatInt(d.Turn, 10)
	}
	switch raw.Type {
	case "turn/start":
		e.Type, e.Status = "turn", "started"
	case "turn/end":
		e.Type, e.Status = "turn", "completed"
		if d.Reason.Kind == "aborted" || d.Reason.Kind == "interrupted" {
			e.Status = "interrupted"
		}
		if d.Reason.Kind == "error" || d.Reason.Kind == "blocked" || d.Reason.Kind == "max-tokens" {
			e.Status, e.Error = "failed", d.Reason.Error.Message
			if e.Error == "" {
				e.Error = d.Reason.Kind
			}
		}
	case "user/message", "assistant/message":
		e.Type, e.Role, e.ID, e.Final, e.Usage = "message", d.Message.Role, d.Message.ID, true, d.Usage
		if e.Role != "user" && e.Role != "assistant" {
			return nil
		}
		var reason strings.Builder
		for _, block := range d.Message.Content {
			if block.Type == "text" {
				e.Text += block.Text
			}
			if block.Type == "reasoning" {
				reason.WriteString(block.Text)
			}
		}
		if e.Text == "" && reason.Len() == 0 {
			return nil
		}
		if reason.Len() > 0 {
			return []protocol.Event{{TS: raw.Time, ID: e.ID + "-reasoning", TurnID: e.TurnID, Type: "reasoning", Role: "assistant", Text: reason.String(), Final: true}, e}
		}
	case "tool/call":
		e.Type, e.Kind, e.ID, e.Title, e.Detail, e.Status = "tool", "tool", d.CallID, d.Name, d.Arguments, "running"
	case "tool/result":
		e.Type, e.Kind, e.ID, e.Status, e.OutputFinal = "tool", "tool", d.Message.ToolCallID, "completed", true
		if d.Message.IsError {
			e.Status = "failed"
		}
		for _, block := range d.Message.Content {
			if block.Type == "text" {
				e.Output += block.Text
			}
		}
	default:
		return nil
	}
	if e.ID == "" {
		e.ID = fmt.Sprintf("dsh-event-%d", raw.Seq)
	}
	return []protocol.Event{e}
}
