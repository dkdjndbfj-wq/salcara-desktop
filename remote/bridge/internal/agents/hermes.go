package agents

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
	"sort"
	"strings"
	"sync"
	"time"
)

const hermesID = "hermes"

type hermesAgent struct {
	settings  func() Settings
	stateDir  string
	mu        sync.Mutex
	connectMu sync.Mutex
	sink      Sink
	closed    bool
	questions bool // negotiated by runtime.describe through the installed client
	conn      hermesGateway
	sessions  map[string]*hermesSession
	dial      func(context.Context, func(string, json.RawMessage)) (hermesGateway, error)
}
type hermesSession struct {
	control                               sync.Mutex
	Info                                  protocol.SessionInfo             `json:"info"`
	NativeID                              string                           `json:"nativeId"`
	Source                                [32]byte                         `json:"sourceFingerprint"`
	APIIdentity                           [32]byte                         `json:"apiIdentity,omitempty"`
	FrozenRoot                            string                           `json:"frozenRoot,omitempty"`
	Policy                                string                           `json:"policy,omitempty"`
	Inputs                                map[string]string                `json:"inputs,omitempty"`
	Admissions                            map[string]string                `json:"admissions,omitempty"`
	Receipts                              map[string]hermesPersistedTurn   `json:"receipts,omitempty"`
	ReplyIDs                              map[string]string                `json:"replyIds,omitempty"`
	InputOrder                            []string                         `json:"inputOrder,omitempty"`
	AdmissionOrder                        []string                         `json:"admissionOrder,omitempty"`
	ReceiptOrder                          []string                         `json:"receiptOrder,omitempty"`
	ReplyOrder                            []string                         `json:"replyOrder,omitempty"`
	Approvals                             map[string]hermesApprovalAttempt `json:"approvalAttempts,omitempty"`
	ApprovalOrder                         []string                         `json:"approvalOrder,omitempty"`
	PromptDeadlines                       map[string]int64                 `json:"promptDeadlines,omitempty"`
	Epoch                                 int64                            `json:"authorityEpoch"`
	Generation                            int64                            `json:"generation"`
	Events                                []protocol.Event                 `json:"events"`
	replay                                string
	sequence                              int64
	busy, uncertain, recovering, overflow bool
	buffered                              []hermesNativeEvent
	completed                             map[string]bool
	streams                               map[string]string
	prompts                               map[string]hermesPrompt
}

func newHermesAgent(settings func() Settings, stateDir string) *hermesAgent {
	a := &hermesAgent{settings: settings, sessions: map[string]*hermesSession{}}
	if stateDir == "" {
		return a
	}
	a.stateDir = filepath.Join(stateDir, hermesID)
	entries, _ := os.ReadDir(a.stateDir)
	for _, entry := range entries {
		id := strings.TrimSuffix(entry.Name(), ".json")
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || !validDeepSeekSessionID(id) {
			continue
		}
		path := filepath.Join(a.stateDir, entry.Name())
		st, err := os.Stat(path)
		if err != nil || st.Size() > 8<<20 {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var s hermesSession
		if json.Unmarshal(data, &s) != nil || s.Info.SessionKey != hermesID+":"+id || s.Info.Tool != hermesID || !ValidTurnID(s.NativeID) || !filepath.IsAbs(s.Info.Cwd) {
			continue
		}
		initHermesSession(&s)
		s.Info.Status = "unknown"
		s.Info.ActiveTurnID = ""
		s.uncertain = true
		a.sessions[id] = &s
	}
	return a
}
func initHermesSession(s *hermesSession) {
	if s.Approvals == nil {
		s.Approvals = map[string]hermesApprovalAttempt{}
	}
	if s.PromptDeadlines == nil {
		s.PromptDeadlines = map[string]int64{}
	}
	normalizeHermesOrder(s.Approvals, &s.ApprovalOrder)
	if s.Inputs == nil {
		s.Inputs = map[string]string{}
	}
	if s.Admissions == nil {
		s.Admissions = map[string]string{}
	}
	if s.Receipts == nil {
		s.Receipts = map[string]hermesPersistedTurn{}
	}
	if s.ReplyIDs == nil {
		s.ReplyIDs = map[string]string{}
	}
	normalizeHermesOrder(s.Inputs, &s.InputOrder)
	normalizeHermesOrder(s.Admissions, &s.AdmissionOrder)
	normalizeHermesOrder(s.Receipts, &s.ReceiptOrder)
	normalizeHermesOrder(s.ReplyIDs, &s.ReplyOrder)
	s.streams = map[string]string{}
	s.prompts = map[string]hermesPrompt{}
	s.completed = map[string]bool{}
	if len(s.Events) > 400 {
		s.Events = append([]protocol.Event(nil), s.Events[len(s.Events)-400:]...)
	}
	for i := range s.Events {
		s.Events[i] = protocol.NormalizeTimelineEvent(s.Events[i])
	}
}
func (*hermesAgent) ID() string   { return hermesID }
func (*hermesAgent) Name() string { return "Hermes Agent" }
func (a *hermesAgent) Descriptor() protocol.AgentDescriptor {
	a.mu.Lock()
	questions := a.questions
	a.mu.Unlock()
	return protocol.AgentDescriptor{ID: a.ID(), Tool: a.ID(), Name: a.Name(), APIFamily: a.ID(), ControlSurface: "cli", Capabilities: protocol.AgentCapabilities{List: true, Read: true, Send: true, Interrupt: true, PreciseInterrupt: true, Approval: true, Questions: questions, StartModelOverride: true}}
}
func (a *hermesAgent) BindEventSink(sink Sink) { a.mu.Lock(); a.sink = sink; a.mu.Unlock() }
func (a *hermesAgent) Detect(ctx context.Context) protocol.Tool {
	tool := protocol.Tool{ID: a.ID(), Name: a.Name()}
	exe, ok := findExecutable("hermes", a.settings().HermesPath)
	if !ok {
		return tool
	}
	if _, err := hermesGatewayCommand(ctx, exe); err != nil {
		return tool
	}
	tool.Available = true
	tool.Version, _ = toolVersion(ctx, exe)
	return tool
}
func (a *hermesAgent) Models(ctx context.Context) []string {
	if model := settingsForContext(ctx, a.settings).Extensions[hermesID].Model; model != "" {
		return []string{model}
	}
	return nil
}
func (a *hermesAgent) Sessions(context.Context) ([]protocol.SessionInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	rows := make([]protocol.SessionInfo, 0, len(a.sessions))
	for _, s := range a.sessions {
		rows = append(rows, s.Info)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].UpdatedAt > rows[j].UpdatedAt })
	return rows, nil
}

// A detached viewer does not stop native work. Unknown/recovering sessions
// must be refreshed before a source switch can retire their route lease.
func (a *hermesAgent) ActiveRemoteTurns() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.sessions {
		if s.busy || s.uncertain || s.recovering || len(s.prompts) > 0 {
			return true
		}
	}
	return false
}

// Legacy indexes did not retain yolo. Unknown policy must not inherit a grant
// after the computer revokes phone full access.
func (a *hermesAgent) SessionRequiresFullAccessConsent(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.sessions[id]
	return s != nil && (s.Policy == "" || s.Policy == "auto_all")
}

func hermesAPIIdentity(api ExtensionSettings, model string) [32]byte {
	if api.SourceFingerprint == "" {
		return [32]byte{}
	}
	return sha256.Sum256([]byte(api.SourceFingerprint + "\x00" + model))
}

// Native rebind_launch_key accepts only the original launch key, including
// after an owner restart. A renewed Bridge lease cannot replace that key.
func hermesFrozenSourceError(api ExtensionSettings, s *hermesSession, model string) error {
	if model != "" && model != s.Info.Model {
		return errors.New("Hermes 原会话模型已冻结，请新建会话选择其他模型")
	}
	if api.Key == "" || api.Root == "" {
		return errors.New("Hermes 当前 API 未就绪；原会话仍可查看和停止，配置 API 后请新建会话")
	}
	if s.APIIdentity != ([32]byte{}) && hermesAPIIdentity(api, s.Info.Model) != s.APIIdentity {
		return errors.New("Hermes 原会话使用的 API 已变化。旧记录保留，原任务仍可查看和停止；可切回原配置核对，若原启动凭据已更新则需新建会话")
	}
	root, _, err := config.APIBase(api.Root)
	if err != nil {
		return errors.New("Hermes 当前 API 地址无效，请在电脑核对配置")
	}
	if s.FrozenRoot != "" && root != s.FrozenRoot {
		return errors.New("Hermes 原会话地址或 Bridge 端口已变化；历史仍可查看、任务仍可停止，请新建会话继续")
	}
	if extensionSource(api, s.Info.Model) != s.Source {
		return errors.New("Hermes 原会话的启动凭据已失效（Bridge 重启或授权更新）；官方不支持替换该凭据。历史仍可查看、任务仍可停止，请新建会话继续")
	}
	return nil
}
func (a *hermesAgent) session(id string) (*hermesSession, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.sessions[id]
	if s == nil {
		return nil, errors.New("Hermes 会话不存在；这里只列出 Salcara 创建的会话")
	}
	return s, nil
}

func (a *hermesAgent) runtime(ctx context.Context) (hermesGateway, error) {
	a.connectMu.Lock()
	defer a.connectMu.Unlock()
	a.mu.Lock()
	conn, closed := a.conn, a.closed
	a.mu.Unlock()
	if closed {
		return nil, errors.New("Hermes 已关闭")
	}
	if conn != nil && conn.alive() {
		return conn, nil
	}
	if conn != nil {
		conn.detach()
	}
	var next hermesGateway
	var err error
	ready := make(chan struct{})
	notify := func(method string, raw json.RawMessage) { <-ready; a.notify(next, method, raw) }
	if a.dial != nil {
		next, err = a.dial(ctx, notify)
	} else {
		exe, ok := findExecutable("hermes", a.settings().HermesPath)
		if !ok {
			return nil, errors.New("未找到官方 Hermes")
		}
		cmd, cmdErr := hermesGatewayCommand(ctx, exe)
		if cmdErr != nil {
			return nil, cmdErr
		}
		var rpc *rpcConn
		rpc, err = startRPCProtocol(cmd, "2.0", notify, nil)
		if err == nil {
			next = &hermesPipe{rpc}
		}
	}
	if err != nil {
		close(ready)
		return nil, errors.New("Hermes Gateway 连接失败，请在电脑检查安装")
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		close(ready)
		next.detach()
		return nil, errors.New("Hermes 已关闭")
	}
	a.conn = next
	a.mu.Unlock()
	close(ready)
	if pipe, ok := next.(*hermesPipe); ok {
		go func() { <-pipe.done; a.notify(next, "gateway.disconnected", nil) }()
	}
	initCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var hello struct {
		Protocol  string `json:"protocol"`
		Questions bool   `json:"questions"`
	}
	if err = next.Call(initCtx, "salcara.initialize", map[string]any{}, &hello); err != nil || hello.Protocol != "salcara-hermes-gateway-v1" {
		a.mu.Lock()
		if a.conn == next {
			a.conn = nil
		}
		a.mu.Unlock()
		next.detach()
		return nil, errors.New("Hermes Gateway 会话协议不兼容或未就绪，请更新 Hermes")
	}
	a.mu.Lock()
	a.questions = hello.Questions
	a.mu.Unlock()
	return next, nil
}
func (a *hermesAgent) Open(ctx context.Context, id string) (protocol.SessionInfo, []protocol.Event, error) {
	s, err := a.session(id)
	if err != nil {
		return protocol.SessionInfo{}, nil, err
	}
	s.control.Lock()
	defer s.control.Unlock()
	conn, err := a.runtime(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return protocol.SessionInfo{}, nil, ctx.Err()
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.closed {
			return protocol.SessionInfo{}, nil, err
		}
		// Local transcripts must remain readable when the native Gateway is
		// unavailable. Cached events are not evidence of current native state
		// and must not revive an approval or authorize another execution.
		s.uncertain = true
		s.Info.Status, s.Info.ActiveTurnID, s.Info.Controllable = "unknown", "", false
		events := make([]protocol.Event, 0, len(s.Events)+1)
		for _, event := range s.Events {
			if event.Type != "approval.request" && event.Type != "approval.resolved" && event.Type != "session.updated" && event.Type != "turn" {
				events = append(events, event)
			}
		}
		events = append(events, protocol.Event{Tool: hermesID, SessionKey: s.Info.SessionKey, Type: "notice", ID: "hermes-cached-history", Level: "warn", Text: "Hermes 运行器暂不可用，当前显示上次保存的记录，可能缺少最新消息。请在电脑检查 Hermes 并刷新；任务状态未确认，暂不能继续发送。"})
		return s.Info, events, nil
	}
	if err = a.resume(ctx, id, s, conn, ""); err != nil {
		return protocol.SessionInfo{}, nil, err
	}
	api := settingsForContext(ctx, a.settings).Extensions[hermesID]
	a.mu.Lock()
	defer a.mu.Unlock()
	events := append([]protocol.Event(nil), s.Events...)
	info := s.Info
	if sourceErr := hermesFrozenSourceError(api, s, ""); sourceErr != nil {
		events = append(events, protocol.Event{Tool: hermesID, SessionKey: s.Info.SessionKey, Type: "notice", ID: "hermes-frozen-source", Level: "warn", NoticeKind: "session_source_frozen", ErrorCode: "session_source_frozen", Text: sourceErr.Error()})
		if !s.busy {
			info.Controllable = false
		}
	}
	return info, events, nil
}
func (a *hermesAgent) Start(ctx context.Context, cwd, prompt, model, approval string) (string, error) {
	return a.StartWithOptions(ctx, cwd, prompt, approval, TurnOptions{Model: model})
}
func validHermesOptions(o TurnOptions) bool {
	return o.Effort == "" && o.PermissionMode == "" && o.ClaudeApproval == "" && o.CollaborationMode == "" && !o.Desktop && len(o.Images) == 0 && len(o.Attachments) == 0 && len(o.Skills) == 0 && len(o.ContextReferences) == 0 && o.CrossTaskOrigin == nil
}
func (a *hermesAgent) StartWithOptions(ctx context.Context, cwd, prompt, approval string, opts TurnOptions) (string, error) {
	if !validHermesOptions(opts) {
		return "", errors.New("Hermes 当前不支持这些会话选项")
	}
	if !filepath.IsAbs(cwd) {
		return "", errors.New("Hermes 工作目录必须为绝对路径")
	}
	if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		return "", errors.New("Hermes 工作目录不存在")
	}
	if strings.TrimSpace(prompt) == "" || len(prompt) > 4<<20 {
		return "", errors.New("请输入有效任务内容")
	}
	settings := settingsForContext(ctx, a.settings)
	api := settings.Extensions[hermesID]
	model := opts.Model
	if model == "" {
		model = api.Model
	}
	if api.Key == "" || api.Root == "" || model == "" {
		return "", errors.New("请先在电脑为 Hermes 应用 API 和模型")
	}
	root, _, err := config.APIBase(api.Root)
	if err != nil {
		return "", err
	}
	if approval == "" {
		approval = settings.Approval
	}
	if approval == "" {
		approval = "ask"
	}
	if approval != "ask" && approval != "auto_all" {
		return "", errors.New("Hermes 支持询问或完全放行，请选择对应审批方式")
	}
	if a.stateDir == "" {
		return "", errors.New("Hermes 会话存储不可用")
	}
	conn, err := a.runtime(ctx)
	if err != nil {
		return "", err
	}
	id := deepSeekNewID()
	if id == "" {
		return "", errors.New("无法生成会话标识")
	}
	var snapshot hermesSnapshot
	err = hermesGuardedCall(ctx, conn, "session.create", map[string]any{"request_id": id, "source": "cli", "cwd": cwd, "model": model, "provider": "custom", "base_url": strings.TrimRight(root, "/") + "/v1", "api_key": api.Key, "yolo": approval == "auto_all"}, &snapshot, opts.ValidatePermission)
	if err != nil {
		return "", hermesError(err)
	}
	if !ValidTurnID(snapshot.NativeID) || snapshot.SessionID != snapshot.NativeID || snapshot.Info.Cwd != cwd {
		return "", hermesUnknown()
	}
	// The native owner may resolve an alias. Pin that actual model, and do not
	// repeat a creation-only override during the first resume/submit.
	if snapshot.Info.Model != "" {
		model = snapshot.Info.Model
	}
	s := &hermesSession{Info: protocol.SessionInfo{SessionKey: hermesID + ":" + id, Tool: hermesID, Client: "salcara", ControlSurface: "cli", Cwd: cwd, Model: model, Title: truncHead(prompt, 100), Controllable: true, Status: "idle", UpdatedAt: time.Now().UnixMilli()}, NativeID: snapshot.NativeID, Source: extensionSource(api, model), APIIdentity: hermesAPIIdentity(api, model), FrozenRoot: root}
	initHermesSession(s)
	s.Policy = approval
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return "", errors.New("Hermes 已关闭")
	}
	a.sessions[id] = s
	a.applySnapshotLocked(id, s, snapshot)
	err = a.persistLocked(id, s)
	a.mu.Unlock()
	if err != nil {
		return "", errors.New("Hermes 会话已创建，但本地索引保存失败；任务尚未发送")
	}
	opts.Model = ""
	if err = a.SendWithOptions(ctx, id, prompt, opts); err != nil {
		return id, err // Native identity exists; a new UUID could duplicate it.
	}
	return id, nil
}
func (a *hermesAgent) Send(ctx context.Context, id, text string) error {
	return a.SendWithOptions(ctx, id, text, TurnOptions{})
}
func (a *hermesAgent) SendWithOptions(ctx context.Context, id, text string, opts TurnOptions) error {
	if !validHermesOptions(opts) || strings.TrimSpace(text) == "" || len(text) > 4<<20 {
		return errors.New("Hermes 消息或选项无效")
	}
	s, err := a.session(id)
	if err != nil {
		return err
	}
	s.control.Lock()
	defer s.control.Unlock()
	api := settingsForContext(ctx, a.settings).Extensions[hermesID]
	a.mu.Lock()
	sourceErr := hermesFrozenSourceError(api, s, opts.Model)
	a.mu.Unlock()
	if sourceErr != nil {
		return sourceErr
	}
	conn, err := a.runtime(ctx)
	if err != nil {
		return err
	}
	if err = a.resume(ctx, id, s, conn, api.Key); err != nil {
		return err
	}
	a.mu.Lock()
	blocked := s.busy || s.uncertain || len(s.prompts) > 0 || a.closed
	a.mu.Unlock()
	if blocked {
		return errors.New("Hermes 会话仍在运行或结果待核对，请先查看或停止当前任务")
	}
	input := opts.ClientUserMessageID
	if input == "" {
		input = deepSeekNewID()
	}
	if !ValidTurnID(input) {
		return errors.New("Hermes 消息标识无效")
	}
	a.mu.Lock()
	boundedHermesPut(s.Inputs, &s.InputOrder, input, opts.ClientUserMessageID)
	_ = a.persistLocked(id, s)
	a.mu.Unlock()
	var receipt struct {
		Admission  string `json:"admission_id"`
		Status     string `json:"status"`
		Generation *int64 `json:"execution_generation"`
		Ref        struct {
			Session string `json:"session_id"`
		} `json:"ref"`
	}
	err = hermesGuardedCall(ctx, conn, "prompt.submit", map[string]string{"session_id": s.NativeID, "input_id": input, "text": text}, &receipt, opts.ValidatePermission)
	if err != nil {
		err = hermesError(err)
		if protocol.ExecutionErrorCode(err) == "command_delivery_uncertain" {
			a.markUnknown(id, s)
		}
		return err
	}
	if !ValidTurnID(receipt.Admission) || receipt.Ref.Session != s.NativeID || !containsHermes([]string{"queued", "started", "terminal"}, receipt.Status) {
		a.markUnknown(id, s)
		return hermesUnknown()
	}
	a.mu.Lock()
	boundedHermesPut(s.Admissions, &s.AdmissionOrder, receipt.Admission, input)
	if !s.completed[receipt.Admission] && receipt.Status != "terminal" {
		s.busy = true
		s.Info.Status = "running"
		if receipt.Generation != nil && *receipt.Generation > s.Generation {
			s.Generation = *receipt.Generation
			s.Info.ActiveTurnID = hermesTurnID(s.Epoch, s.Generation)
		}
	}
	info := s.Info
	a.mu.Unlock()
	a.emit(id, protocol.Event{Type: "message", ID: "hermes-input-" + input, Role: "user", Text: text, Final: true, ClientID: opts.ClientUserMessageID})
	a.emit(id, protocol.Event{Type: "session.updated", Session: &info})
	return nil
}
func (a *hermesAgent) Close() {
	a.mu.Lock()
	a.closed = true
	conn := a.conn
	a.conn = nil
	a.mu.Unlock()
	if conn != nil {
		conn.detach()
	}
}
func hermesUnknown() error {
	return &rpcDeliveryUncertainError{message: "Hermes 操作结果未确认，请核对原会话；不会自动重发"}
}

type hermesReadFailure struct {
	cause  error
	reason string
}

func (e *hermesReadFailure) Error() string {
	return "Hermes 会话读取失败（" + e.reason + "），请检查电脑连接后刷新；任务未重发"
}
func (e *hermesReadFailure) Unwrap() error { return e.cause }
func newHermesReadFailure(err error) error {
	reason := "连接中断或协议错误"
	var remote *rpcError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		reason = "读取超时"
	case errors.Is(err, context.Canceled):
		reason = "读取已取消"
	case errors.Is(err, ErrRPCResponseTooLarge):
		reason = "返回记录超过读取上限"
	case errors.As(err, &remote):
		reason = fmt.Sprintf("原生错误码 %d", remote.Code)
		if containsHermes([]string{"gateway_read_failed", "not_found", "permission_denied", "admission_conflict", "session_busy", "runtime_draining", "profile_mismatch"}, remote.Message) {
			reason += " / " + remote.Message
		}
	}
	return &hermesReadFailure{cause: err, reason: reason}
}
func hermesError(err error) error {
	if err == nil {
		return nil
	}
	var unsent *hermesNotSentError
	if errors.As(err, &unsent) {
		return unsent
	}
	var rpc *rpcError
	if errors.As(err, &rpc) && rpc.Message != "command_delivery_uncertain" {
		return fmt.Errorf("Hermes Gateway 拒绝请求：%s", truncHead(rpc.Message, 100))
	}
	return hermesUnknown()
}
func (a *hermesAgent) markUnknown(id string, s *hermesSession) {
	a.mu.Lock()
	s.uncertain = true
	s.Info.Status = "unknown"
	info := s.Info
	_ = a.persistLocked(id, s)
	a.mu.Unlock()
	a.emit(id, protocol.Event{Type: "session.updated", Session: &info})
}
func containsHermes(values []string, wanted string) bool {
	for _, v := range values {
		if v == wanted {
			return true
		}
	}
	return false
}
func hermesTurnID(epoch, generation int64) string {
	if generation <= 0 {
		return ""
	}
	return fmt.Sprintf("hermes-%d-%d", epoch, generation)
}
func (a *hermesAgent) persistLocked(id string, s *hermesSession) error {
	if a.stateDir == "" {
		return nil
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if len(data) > 8<<20 {
		return errors.New("Hermes 历史超过缓存上限")
	}
	if err = os.MkdirAll(a.stateDir, 0700); err != nil {
		return err
	}
	path := filepath.Join(a.stateDir, id+".json")
	tmp := path + ".tmp"
	if err = os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (a *hermesAgent) persistHermesApprovalLocked(id string, s *hermesSession) error {
	if a.stateDir == "" {
		return errors.New("Hermes 审批存储不可用")
	}
	if err := a.persistLocked(id, s); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(a.stateDir, id+".json"), os.O_RDWR, 0)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
func (a *hermesAgent) emit(id string, e protocol.Event) {
	e.Tool = hermesID
	e = protocol.NormalizeTimelineEvent(e)
	a.mu.Lock()
	s := a.sessions[id]
	if s == nil || a.closed {
		a.mu.Unlock()
		return
	}
	e.Tool, e.SessionKey = hermesID, s.Info.SessionKey
	if e.TS == 0 {
		e.TS = time.Now().UnixMilli()
	}
	if e.Type != "session.updated" {
		replaced := false
		if e.ID != "" {
			for i := len(s.Events) - 1; i >= 0; i-- {
				if s.Events[i].ID == e.ID && s.Events[i].Type == e.Type {
					s.Events[i] = e
					replaced = true
					break
				}
			}
		}
		if !replaced {
			s.Events = append(s.Events, e)
		}
		if len(s.Events) > 400 {
			s.Events = append([]protocol.Event(nil), s.Events[len(s.Events)-400:]...)
		}
		_ = a.persistLocked(id, s)
	}
	sink := a.sink
	a.mu.Unlock()
	if sink != nil {
		sink(e)
	}
}
