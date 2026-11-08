package agents

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

var errClaudeContextBusy = errors.New("context_busy: Claude 正在处理任务或压缩上下文")
var errClaudeContextChanged = errors.New("context_changed: Claude 上下文或 API 来源已变化，请刷新后重试")
var errClaudeCompactUnsupported = errors.New("compact_unsupported: 当前 Claude 未确认可用的内置压缩命令")

const claudeContextOperationLimit = 64

// Registry locks are always innermost: callbacks and sinks must run without
// this lock or the agent/proc locks. Receipts survive worker replacement.
type claudeContextRegistry struct {
	mu       sync.Mutex
	sessions map[string]*claudeContextSession
}

type claudeContextSession struct {
	worker        *claudeProc
	view          protocol.SessionContext
	version       string
	capabilities  bool
	builtin       bool
	pending       *claudeContextOperation
	automatic     *protocol.ContextCompaction
	operations    map[string]*claudeContextOperation
	order         []string
	boundaries    map[string]bool
	boundaryOrder []string
	starts        map[string]bool
	startOrder    []string
}

type claudeContextOperation struct {
	worker         *claudeProc
	expected       string
	uuid           string
	receipt        protocol.ContextCompaction
	boundary       bool
	done           chan struct{}
	finished       bool
	frozen         bool
	sourceContext  context.Context
	validateSource func() error
	startedNotice  bool
}

func cloneClaudeContext(view protocol.SessionContext) protocol.SessionContext {
	if view.Snapshot != nil {
		snapshot := *view.Snapshot
		if snapshot.WindowTokens != nil {
			window := *snapshot.WindowTokens
			snapshot.WindowTokens = &window
		}
		view.Snapshot = &snapshot
	}
	if view.Compaction != nil {
		compaction := *view.Compaction
		view.Compaction = &compaction
	}
	return view
}

func (r *claudeContextRegistry) sessionLocked(p *claudeProc) *claudeContextSession {
	if r.sessions == nil {
		r.sessions = make(map[string]*claudeContextSession)
	}
	s := r.sessions[p.id]
	if s == nil {
		s = &claudeContextSession{operations: make(map[string]*claudeContextOperation)}
		r.sessions[p.id] = s
	}
	if s.worker != p {
		if op := s.pending; op != nil && !op.finished {
			op.receipt.State, op.receipt.Error, op.receipt.UpdatedAt = "unconfirmed", "Claude 工作进程已更换，压缩结果未确认", nowMs()
			op.finished = true
			close(op.done)
		}
		if s.automatic != nil && s.automatic.State == "running" {
			s.automatic.State, s.automatic.Error, s.automatic.UpdatedAt, s.automatic.Blocking = "unconfirmed", "Claude 工作进程已更换，压缩结果未确认", nowMs(), false
		}
		s.worker, s.pending, s.version, s.capabilities, s.builtin = p, nil, "", false, false
		s.boundaries, s.boundaryOrder, s.starts, s.startOrder = make(map[string]bool), nil, make(map[string]bool), nil
		s.view = protocol.SessionContext{SessionKey: "claude:" + p.id, Revision: newID("ctx_")}
		if s.automatic != nil {
			receipt := *s.automatic
			s.view.Compaction = &receipt
		}
	}
	return s
}

// Claiming a registry generation must be atomic with checking ownership. A
// prior guard alone cannot prevent a delayed request from replacing the new
// worker's state. No callbacks or native I/O may run before unlock.
func (a *claudeAgent) lockActiveClaudeContext(p *claudeProc) (*claudeContextSession, func(), error) {
	a.mu.Lock()
	p.mu.Lock()
	a.contextState.mu.Lock()
	unlock := func() {
		a.contextState.mu.Unlock()
		p.mu.Unlock()
		a.mu.Unlock()
	}
	if a.closed || a.procs[p.id] != p || p.stopped || p.transportRetired() {
		unlock()
		return nil, nil, errClaudeContextChanged
	}
	return a.contextState.sessionLocked(p), unlock, nil
}

// Caller holds a.mu, p.mu and contextState.mu. Blocking is a live projection
// of this exact operation, never a persisted receipt or ordinary-turn state.
func (a *claudeAgent) projectClaudeContextLocked(p *claudeProc, op *claudeContextOperation) protocol.SessionContext {
	view := protocol.SessionContext{SessionKey: "claude:" + p.id, UnavailableReason: "runtime_changed"}
	s := a.contextState.sessions[p.id]
	if s != nil && s.worker == p {
		view = cloneClaudeContext(s.view)
		if op == nil && view.Compaction != nil {
			op = s.operations[view.Compaction.OperationID]
		}
	}
	if view.Compaction != nil {
		view.Compaction.Blocking = false
		if s != nil && s.worker == p && s.automatic != nil && view.Compaction.OperationID == s.automatic.OperationID {
			view.Compaction.Blocking = !a.closed && a.procs[p.id] == p && !p.stopped && !p.transportRetired() &&
				p.contextCompacting && s.automatic.State == "running"
		}
	}
	if op != nil {
		receipt := op.receipt
		// An unconfirmed result keeps this admission fence until stop retires
		// the worker. The latest receipt's exact identity owns that fence even
		// after pending is cleared; an older receipt must never inherit it.
		receipt.Blocking = !a.closed && a.procs[p.id] == p && !p.stopped && !p.transportRetired() &&
			p.contextCompacting && s != nil && s.worker == p && s.view.Compaction == &op.receipt && op.worker == p
		view.Compaction = &receipt
	}
	return view
}

func (a *claudeAgent) claudeContextView(p *claudeProc) protocol.SessionContext {
	a.mu.Lock()
	defer a.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	a.contextState.mu.Lock()
	defer a.contextState.mu.Unlock()
	return a.projectClaudeContextLocked(p, nil)
}

func (a *claudeAgent) claudeCompactReceiptView(p *claudeProc, op *claudeContextOperation) protocol.SessionContext {
	a.mu.Lock()
	defer a.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	a.contextState.mu.Lock()
	defer a.contextState.mu.Unlock()
	return a.projectClaudeContextLocked(p, op)
}

// May be called under a.mu/p.mu at turn reservation. It never calls a sink.
func (a *claudeAgent) invalidateClaudeContext(p *claudeProc) {
	a.contextState.mu.Lock()
	defer a.contextState.mu.Unlock()
	if s := a.contextState.sessions[p.id]; s != nil && s.worker == p {
		s.view.Snapshot, s.view.CanCompact = nil, false
		s.view.Revision = newID("ctx_")
	}
}

func (a *claudeAgent) claudeContextSource(ctx context.Context, validate func() error) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if validate != nil {
		if err := validate(); err != nil {
			return "", err
		}
	}
	requested := settingsForContext(ctx, a.settings)
	if err := validateClaudeAPI(requested); err != nil {
		return "", err
	}
	source := claudeSettingsSig(requested, a.exeOverride)
	// A phone's selected source may differ from the computer's local source.
	// Both immutable snapshot kinds require the dispatcher's live source guard;
	// unsnapshotted callers retain the current-settings comparison.
	if _, frozen := ctx.Value(configSnapshotKey{}).(config.Config); frozen {
		if validate == nil {
			return "", errClaudeContextChanged
		}
	} else if source != claudeSettingsSig(a.settings(), a.exeOverride) {
		return "", errClaudeContextChanged
	}
	return source, ctx.Err()
}

func (a *claudeAgent) claudeContextGuard(ctx context.Context, p *claudeProc, validate func() error) error {
	source, err := a.claudeContextSource(ctx, validate)
	if err != nil {
		return err
	}
	if p.workerSettingsSig != source {
		return errClaudeContextChanged
	}
	if !a.writeWorkerActive(p) {
		return errClaudeContextChanged
	}
	return ctx.Err()
}

// This path is only for reads. Unlike a mode-changing feature control, a read
// timing out does not leave unknown executable settings and must not kill a
// worker. A partially written JSON frame still retires the damaged transport.
func (a *claudeAgent) claudeContextControl(ctx context.Context, p *claudeProc, request map[string]any, validate func() error) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err := a.claudeContextGuard(ctx, p, validate); err != nil {
		return nil, err
	}
	id := newID("context_read_")
	ch := make(chan claudeControlReply, 1)
	p.controlMu.Lock()
	if p.controlPending == nil {
		p.controlPending = make(map[string]chan claudeControlReply)
	}
	p.controlPending[id] = ch
	p.controlMu.Unlock()
	defer func() {
		p.controlMu.Lock()
		delete(p.controlPending, id)
		p.controlMu.Unlock()
	}()
	frame, _ := json.Marshal(map[string]any{"type": "control_request", "request_id": id, "request": request})
	frame = append(frame, '\n')
	p.wmu.Lock()
	if err := a.claudeContextGuard(ctx, p, validate); err != nil {
		p.wmu.Unlock()
		return nil, err
	}
	n, err := p.stdin.Write(frame)
	p.wmu.Unlock()
	if err != nil || n != len(frame) {
		a.stop(p)
		if err == nil {
			err = io.ErrShortWrite
		}
		return nil, err
	}
	select {
	case reply := <-ch:
		if err := a.claudeContextGuard(ctx, p, validate); err != nil {
			return nil, err
		}
		return reply.data, reply.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.stopCh:
		return nil, errClaudeContextChanged
	case <-p.done:
		return nil, errClaudeContextChanged
	}
}

// Audited against official SDK 0.3.300 / CLI 2.1.300. CLI 2.1.220 silently
// ignores detail and runs the full report; never probe it with a summary read.
// Missing/unrecognized versions stay unknown rather than assuming support.
func claudeContextSummarySupported(version string) bool {
	parts := strings.Split(version, ".")
	if len(parts) != 3 || parts[0] != "2" || parts[1] != "1" {
		return false
	}
	patch, err := strconv.Atoi(parts[2])
	return err == nil && patch >= 300 && strconv.Itoa(patch) == parts[2]
}

func (a *claudeAgent) claudeContextCapabilities(ctx context.Context, p *claudeProc, validate func() error, refresh bool) error {
	s, unlock, err := a.lockActiveClaudeContext(p)
	if err != nil {
		return err
	}
	known := s.capabilities
	unlock()
	if known && !refresh {
		return nil
	}
	raw, err := a.claudeContextControl(ctx, p, map[string]any{"subtype": "initialize"}, validate)
	if err != nil {
		return err
	}
	var initialized struct {
		Version  string `json:"claude_code_version"`
		Commands *[]struct {
			Name    string `json:"name"`
			Builtin bool   `json:"builtin"`
		} `json:"commands"`
	}
	if json.Unmarshal(raw, &initialized) != nil || initialized.Commands == nil {
		return errClaudeControlInvalid
	}
	p.initialized = true // caller holds turnMu
	if initialized.Version == "" {
		versionRaw, versionErr := a.claudeContextControl(ctx, p, map[string]any{"subtype": "get_binary_version"}, validate)
		if versionErr != nil {
			var rejection *claudeControlRejection
			if !errors.As(versionErr, &rejection) || rejection.unsupportedSubtype != "get_binary_version" {
				return versionErr
			}
		} else {
			var version struct {
				Version string `json:"version"`
			}
			if json.Unmarshal(versionRaw, &version) != nil {
				return errClaudeControlInvalid
			}
			initialized.Version = version.Version
		}
	}
	builtins := 0
	for _, command := range *initialized.Commands {
		if command.Name == "compact" && command.Builtin {
			builtins++
		}
	}
	s, unlock, err = a.lockActiveClaudeContext(p)
	if err != nil {
		return err
	}
	s.version, s.builtin, s.capabilities = initialized.Version, builtins == 1, true
	unlock()
	return nil
}

func parseClaudeContextSummary(raw json.RawMessage) (*protocol.ContextSnapshot, error) {
	var usage struct {
		Total  *int64 `json:"totalTokens"`
		Window *int64 `json:"rawMaxTokens"`
		Model  string `json:"model"`
	}
	if json.Unmarshal(raw, &usage) != nil || usage.Total == nil || *usage.Total < 0 || *usage.Total > 1<<53-1 || len(usage.Model) > 256 {
		return nil, errClaudeControlInvalid
	}
	if usage.Window != nil && (*usage.Window <= 0 || *usage.Window > 1<<53-1) {
		return nil, errClaudeControlInvalid
	}
	return &protocol.ContextSnapshot{UsedTokens: *usage.Total, WindowTokens: usage.Window, Model: usage.Model,
		Source: "claude_context_usage", Basis: "runtime_estimate", WindowKind: "effective", Estimated: true, MeasuredAt: nowMs()}, nil
}

func sameClaudeContextReading(a, b *protocol.ContextSnapshot) bool {
	if a == nil || b == nil || a.UsedTokens != b.UsedTokens || a.Model != b.Model || (a.WindowTokens == nil) != (b.WindowTokens == nil) {
		return false
	}
	return a.WindowTokens == nil || *a.WindowTokens == *b.WindowTokens
}

// An exited worker has no readable context, but losing its transport must not
// erase a dispatched operation receipt. Keep only that source-bound receipt.
func (a *claudeAgent) detachedClaudeContext(ctx context.Context, id, expectedRevision, operationID string, validate func() error) (protocol.SessionContext, error) {
	source, err := a.claudeContextSource(ctx, validate)
	if err != nil {
		return protocol.SessionContext{}, err
	}
	view, receiptOp, err := func() (protocol.SessionContext, *claudeContextOperation, error) {
		view := protocol.SessionContext{SessionKey: "claude:" + id, UnavailableReason: "runtime_not_owned"}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.closed || a.procs[id] != nil {
			return protocol.SessionContext{}, nil, errClaudeContextChanged
		}
		a.contextState.mu.Lock()
		defer a.contextState.mu.Unlock()
		s := a.contextState.sessions[id]
		if s == nil || s.worker == nil || s.worker.workerSettingsSig != source {
			if operationID != "" {
				return protocol.SessionContext{}, nil, errClaudeContextChanged
			}
			return view, nil, nil
		}
		if op := s.pending; op != nil && !op.finished {
			op.receipt.State, op.receipt.Error, op.receipt.UpdatedAt = "unconfirmed", "Claude 连接已结束，压缩结果未确认", nowMs()
			op.finished = true
			close(op.done)
			s.pending = nil
			s.view.Revision = newID("ctx_")
		}
		view = cloneClaudeContext(s.view)
		view.Snapshot, view.CanCompact, view.UnavailableReason = nil, false, "runtime_not_owned"
		var receiptOp *claudeContextOperation
		if operationID != "" {
			receiptOp = s.operations[operationID]
			if receiptOp == nil || receiptOp.expected != expectedRevision || receiptOp.worker.workerSettingsSig != source {
				return protocol.SessionContext{}, nil, errClaudeContextChanged
			}
			receipt := receiptOp.receipt
			view.Compaction = &receipt
		}
		if view.Compaction != nil {
			view.Compaction.Blocking = false
			receiptOp = s.operations[view.Compaction.OperationID]
		}
		return view, receiptOp, nil
	}()
	if err != nil {
		return protocol.SessionContext{}, err
	}
	// Recheck after the registry wait and outside all vendor locks. The old
	// operation's authorization epoch cannot be revived by a new caller's proof.
	if current, err := a.claudeContextSource(ctx, validate); err != nil {
		return protocol.SessionContext{}, err
	} else if current != source {
		return protocol.SessionContext{}, errClaudeContextChanged
	}
	if receiptOp != nil && receiptOp.validateSource != nil {
		if err := receiptOp.validateSource(); err != nil {
			return protocol.SessionContext{}, err
		}
	}
	a.mu.Lock()
	active := !a.closed && a.procs[id] == nil
	a.mu.Unlock()
	if !active {
		return protocol.SessionContext{}, errClaudeContextChanged
	}
	return view, ctx.Err()
}
func (a *claudeAgent) SessionContext(ctx context.Context, id string, validate func() error) (protocol.SessionContext, error) {
	a.mu.Lock()
	p := a.procs[id]
	a.mu.Unlock()
	if p == nil {
		return a.detachedClaudeContext(ctx, id, "", "", validate)
	}
	if err := a.claudeContextGuard(ctx, p, validate); err != nil {
		return protocol.SessionContext{}, err
	}
	if !p.turnMu.TryLock() {
		view := a.claudeContextView(p)
		view.CanCompact, view.UnavailableReason = false, "runtime_busy"
		return view, nil
	}
	defer p.turnMu.Unlock()
	if err := a.claudeFeatureIdle(p); err != nil {
		view := a.claudeContextView(p)
		view.CanCompact, view.UnavailableReason = false, "runtime_busy"
		return view, nil
	}
	if err := a.claudeContextCapabilities(ctx, p, validate, false); err != nil {
		return protocol.SessionContext{}, err
	}
	s, unlock, err := a.lockActiveClaudeContext(p)
	if err != nil {
		return protocol.SessionContext{}, err
	}
	version, revision := s.version, s.view.Revision
	s.view.CanCompact = s.builtin
	s.view.UnavailableReason = "context_summary_unavailable"
	unlock()
	if !claudeContextSummarySupported(version) {
		if err := a.claudeContextGuard(ctx, p, validate); err != nil {
			return protocol.SessionContext{}, err
		}
		return a.claudeContextView(p), nil
	}
	raw, err := a.claudeContextControl(ctx, p, map[string]any{"subtype": "get_context_usage", "detail": "summary"}, validate)
	if err != nil {
		var rejection *claudeControlRejection
		if errors.As(err, &rejection) && rejection.unsupportedSubtype == "get_context_usage" {
			return a.claudeContextView(p), nil
		}
		return protocol.SessionContext{}, err
	}
	snapshot, err := parseClaudeContextSummary(raw)
	if err != nil {
		return protocol.SessionContext{}, err
	}
	if err := a.claudeContextGuard(ctx, p, validate); err != nil {
		return protocol.SessionContext{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	a.contextState.mu.Lock()
	defer a.contextState.mu.Unlock()
	s = a.contextState.sessions[p.id]
	if a.closed || a.procs[p.id] != p || p.stopped || p.transportRetired() || s == nil || s.worker != p || s.view.Revision != revision {
		return protocol.SessionContext{}, errClaudeContextChanged
	}
	if !sameClaudeContextReading(s.view.Snapshot, snapshot) {
		s.view.Revision = newID("ctx_")
	}
	s.view.Snapshot, s.view.UnavailableReason = snapshot, ""
	return a.projectClaudeContextLocked(p, nil), nil
}

func (a *claudeAgent) CompactContext(ctx context.Context, id, expectedRevision, operationID string, validate func() error) (protocol.SessionContext, error) {
	if expectedRevision == "" || len(expectedRevision) > 256 || !ValidControlOperationID(operationID) {
		return protocol.SessionContext{}, errors.New("context_revision_required: 压缩需要有效的上下文版本和操作标识")
	}
	a.mu.Lock()
	p := a.procs[id]
	a.mu.Unlock()
	if p == nil {
		return a.detachedClaudeContext(ctx, id, expectedRevision, operationID, validate)
	}
	if err := a.claudeContextGuard(ctx, p, validate); err != nil {
		return protocol.SessionContext{}, err
	}
	_, frozen := ctx.Value(configSnapshotKey{}).(config.Config)
	lifecycleGuard := ContextLifecycleGuard(ctx)
	if frozen && lifecycleGuard == nil {
		return protocol.SessionContext{}, errClaudeContextChanged
	}
	s, unlock, err := a.lockActiveClaudeContext(p)
	if err != nil {
		return protocol.SessionContext{}, err
	}
	if op := s.operations[operationID]; op != nil {
		view := a.projectClaudeContextLocked(p, op)
		valid := op.expected == expectedRevision && op.worker.workerSettingsSig == p.workerSettingsSig
		unlock()
		if !valid {
			return protocol.SessionContext{}, errClaudeContextChanged
		}
		if err := a.claudeContextGuard(ctx, p, validate); err != nil {
			return protocol.SessionContext{}, err
		}
		if op.validateSource != nil {
			if err := op.validateSource(); err != nil {
				return protocol.SessionContext{}, err
			}
		}
		return view, nil
	}
	current := s.view.Revision == expectedRevision
	unlock()
	if !current {
		return protocol.SessionContext{}, errClaudeContextChanged
	}
	if !p.turnMu.TryLock() {
		return protocol.SessionContext{}, errClaudeContextBusy
	}
	locked := true
	defer func() {
		if locked {
			p.turnMu.Unlock()
		}
	}()
	if err := a.claudeFeatureIdle(p); err != nil {
		return protocol.SessionContext{}, errClaudeContextBusy
	}
	if err := a.claudeContextCapabilities(ctx, p, validate, true); err != nil {
		return protocol.SessionContext{}, err
	}
	p.wmu.Lock()
	if err := a.claudeContextGuard(ctx, p, validate); err != nil {
		p.wmu.Unlock()
		return protocol.SessionContext{}, err
	}
	pendingApproval := a.aps.countFor("claude:"+p.id) != 0
	a.mu.Lock()
	p.mu.Lock()
	a.contextState.mu.Lock()
	s = a.contextState.sessions[p.id]
	var admissionErr error
	switch {
	case ctx.Err() != nil:
		admissionErr = ctx.Err()
	case a.closed || a.procs[id] != p || p.stopped || p.transportRetired() || s == nil || s.worker != p || s.view.Revision != expectedRevision:
		admissionErr = errClaudeContextChanged
	case p.turns != 0 || len(p.turnIDs) != 0 || p.activeSubtasks.Load() || p.contextCompacting || pendingApproval:
		admissionErr = errClaudeContextBusy
	case !s.builtin:
		admissionErr = errClaudeCompactUnsupported
	}
	if admissionErr != nil {
		a.contextState.mu.Unlock()
		p.mu.Unlock()
		a.mu.Unlock()
		p.wmu.Unlock()
		return protocol.SessionContext{}, admissionErr
	}
	stamp := nowMs()
	op := &claudeContextOperation{worker: p, expected: expectedRevision, uuid: newUUID(), done: make(chan struct{}),
		frozen: frozen, sourceContext: context.WithoutCancel(ctx), validateSource: lifecycleGuard,
		receipt: protocol.ContextCompaction{OperationID: operationID, State: "requested", StartedAt: stamp, UpdatedAt: stamp}}
	s.pending, s.operations[operationID] = op, op
	s.order = append(s.order, operationID)
	if len(s.order) > claudeContextOperationLimit {
		delete(s.operations, s.order[0])
		s.order = s.order[1:]
	}
	s.view.Revision, s.view.Snapshot, s.view.CanCompact, s.view.Compaction = newID("ctx_"), nil, false, &op.receipt
	p.contextCompacting = true
	a.contextState.mu.Unlock()
	p.mu.Unlock()
	a.mu.Unlock()
	frame, _ := json.Marshal(map[string]any{"type": "user", "session_id": id, "parent_tool_use_id": nil, "uuid": op.uuid,
		"message": map[string]any{"role": "user", "content": "/compact"}})
	frame = append(frame, '\n')
	n, writeErr := p.stdin.Write(frame)
	p.wmu.Unlock()
	p.turnMu.Unlock()
	locked = false
	if writeErr != nil || n != len(frame) {
		a.finishClaudeCompact(p, op, "unconfirmed", "压缩命令交付未确认；不会自动重发")
		a.stop(p)
		return a.claudeCompactReceiptView(p, op), nil
	}
	a.contextState.mu.Lock()
	if !op.finished && op.receipt.State == "requested" {
		op.receipt.State, op.receipt.UpdatedAt = "running", nowMs()
	}
	emitStartedNotice := !op.startedNotice
	op.startedNotice = true
	a.contextState.mu.Unlock()
	view := a.claudeCompactReceiptView(p, op)
	if a.writeWorkerActive(p) {
		a.emit(protocol.Event{SessionKey: view.SessionKey, Type: "context.updated", Context: &view})
		if emitStartedNotice {
			a.emit(protocol.Event{SessionKey: view.SessionKey, ID: claudeContextCompactionNoticeID(op.receipt.OperationID), Type: "notice", Level: "info", NoticeKind: "context_compaction", Progress: true, Text: "正在压缩上下文"})
		}
	}
	go a.watchClaudeCompact(p, op)
	return view, nil
}

func (a *claudeAgent) watchClaudeCompact(p *claudeProc, op *claudeContextOperation) {
	timer := time.NewTimer(2 * time.Minute)
	defer timer.Stop()
	select {
	case <-op.done:
		a.contextState.mu.Lock()
		s := a.contextState.sessions[p.id]
		completed := op.receipt.State == "completed" && s != nil && s.worker == p && claudeContextSummarySupported(s.version)
		a.contextState.mu.Unlock()
		if completed {
			guard := func() error { return a.claudeCompactSourceGuard(p, op) }
			if view, err := a.SessionContext(op.sourceContext, p.id, guard); err == nil && view.Snapshot != nil && a.writeWorkerActive(p) {
				a.emit(protocol.Event{SessionKey: view.SessionKey, Type: "context.updated", Context: &view})
			}
		}
		return
	case <-p.stopCh:
	case <-p.done:
	case <-timer.C:
		a.finishClaudeCompact(p, op, "unconfirmed", "等待原生压缩结果超时；不会自动重发")
		a.stop(p)
		return
	}
	a.finishClaudeCompact(p, op, "unconfirmed", "Claude 连接已结束，压缩结果未确认")
}

func (a *claudeAgent) claudeCompactSourceGuard(p *claudeProc, op *claudeContextOperation) error {
	if op.sourceContext == nil || op.frozen && op.validateSource == nil {
		return errClaudeContextChanged
	}
	return a.claudeContextGuard(op.sourceContext, p, op.validateSource)
}

func (a *claudeAgent) finishClaudeCompact(p *claudeProc, op *claudeContextOperation, state, reason string) {
	a.mu.Lock()
	p.mu.Lock()
	a.contextState.mu.Lock()
	s := a.contextState.sessions[p.id]
	if op.finished || s == nil || s.worker != p || s.pending != op {
		a.contextState.mu.Unlock()
		p.mu.Unlock()
		a.mu.Unlock()
		return
	}
	op.receipt.State, op.receipt.Error, op.receipt.UpdatedAt, op.finished = state, reason, nowMs(), true
	close(op.done)
	s.pending = nil
	s.view.Revision, s.view.Snapshot = newID("ctx_"), nil
	// Unknown delivery/completion cannot reopen admission before stop retires
	// the worker. Otherwise a new turn could slip between this state and stop.
	p.contextCompacting = state == "unconfirmed"
	active := !a.closed && a.procs[p.id] == p && !p.stopped && !p.transportRetired()
	s.view.CanCompact = active && s.builtin && state != "unconfirmed" && p.turns == 0 && !p.activeSubtasks.Load()
	view := a.projectClaudeContextLocked(p, nil)
	a.contextState.mu.Unlock()
	p.mu.Unlock()
	a.mu.Unlock()
	if active {
		a.emit(protocol.Event{SessionKey: view.SessionKey, Type: "context.updated", Context: &view})
		text := "正在压缩上下文"
		switch state {
		case "completed":
			text = "上下文已压缩"
		case "failed":
			text = "上下文压缩失败"
		case "unconfirmed":
			text = "压缩结果未确认"
		}
		a.emit(protocol.Event{SessionKey: view.SessionKey, ID: claudeContextCompactionNoticeID(op.receipt.OperationID), Type: "notice", Level: "info", NoticeKind: "context_compaction", Text: text})
	}
}

func claudeContextCompactionNoticeID(operationID string) string {
	if operationID == "" {
		return ""
	}
	return "context-compaction:" + operationID
}

func rememberClaudeCompactionID(ids map[string]bool, order *[]string, id string) bool {
	if id == "" || len(id) > 256 {
		return true
	}
	if ids[id] {
		return false
	}
	ids[id] = true
	*order = append(*order, id)
	if len(*order) > claudeContextOperationLimit {
		delete(ids, (*order)[0])
		*order = (*order)[1:]
	}
	return true
}

// Claude Code emits this status before its compact_boundary. It is distinct
// from ordinary assistant/tool activity, which must never be called compaction.
func (a *claudeAgent) observeClaudeCompactionStart(p *claudeProc, nativeID string) {
	a.mu.Lock()
	p.mu.Lock()
	a.contextState.mu.Lock()
	if a.closed || a.procs[p.id] != p || p.stopped || p.transportRetired() {
		a.contextState.mu.Unlock()
		p.mu.Unlock()
		a.mu.Unlock()
		return
	}
	s := a.contextState.sessionLocked(p)
	if s.starts == nil {
		s.starts = make(map[string]bool)
	}
	if !rememberClaudeCompactionID(s.starts, &s.startOrder, nativeID) {
		a.contextState.mu.Unlock()
		p.mu.Unlock()
		a.mu.Unlock()
		return
	}
	changed, emitNotice, operationID := false, false, ""
	if op := s.pending; op != nil && !op.finished {
		operationID = op.receipt.OperationID
		if op.receipt.State == "requested" {
			op.receipt.State, op.receipt.UpdatedAt = "running", nowMs()
			s.view.Revision, s.view.Snapshot, s.view.CanCompact = newID("ctx_"), nil, false
			changed = true
		}
		if !op.startedNotice {
			op.startedNotice, emitNotice = true, true
		}
		p.contextCompacting = true
	} else {
		if s.automatic != nil && s.automatic.State == "running" {
			a.contextState.mu.Unlock()
			p.mu.Unlock()
			a.mu.Unlock()
			return
		}
		now := nowMs()
		receipt := protocol.ContextCompaction{OperationID: newUUID(), State: "running", Blocking: true, StartedAt: now, UpdatedAt: now}
		s.automatic = &receipt
		s.view.Revision, s.view.Snapshot, s.view.CanCompact = newID("ctx_"), nil, false
		s.view.Compaction = &receipt
		p.contextCompacting = true
		operationID, changed, emitNotice = receipt.OperationID, true, true
	}
	view := a.projectClaudeContextLocked(p, nil)
	a.contextState.mu.Unlock()
	p.mu.Unlock()
	a.mu.Unlock()
	if changed {
		a.emit(protocol.Event{SessionKey: view.SessionKey, Type: "context.updated", Context: &view})
	}
	if emitNotice {
		a.emit(protocol.Event{SessionKey: view.SessionKey, ID: claudeContextCompactionNoticeID(operationID), Type: "notice", Level: "info", NoticeKind: "context_compaction", Progress: true, Text: "正在压缩上下文"})
	}
}

func (a *claudeAgent) observeClaudeCompactionBoundary(p *claudeProc, nativeID, trigger string) {
	a.mu.Lock()
	p.mu.Lock()
	a.contextState.mu.Lock()
	if a.closed || a.procs[p.id] != p || p.stopped || p.transportRetired() {
		a.contextState.mu.Unlock()
		p.mu.Unlock()
		a.mu.Unlock()
		return
	}
	s := a.contextState.sessionLocked(p)
	if s.boundaries == nil {
		s.boundaries = make(map[string]bool)
	}
	if !rememberClaudeCompactionID(s.boundaries, &s.boundaryOrder, nativeID) {
		a.contextState.mu.Unlock()
		p.mu.Unlock()
		a.mu.Unlock()
		return
	}
	changed, emitNotice, operationID := false, false, ""
	if op := s.pending; op != nil && !op.finished && (trigger == "manual" || trigger == "") {
		op.boundary = true
		operationID = op.receipt.OperationID
	} else if s.automatic != nil && s.automatic.State == "running" {
		operationID = s.automatic.OperationID
		now := nowMs()
		s.automatic.State, s.automatic.Error, s.automatic.UpdatedAt, s.automatic.Blocking = "completed", "", now, false
		receipt := *s.automatic
		s.view.Revision, s.view.Snapshot, s.view.Compaction = newID("ctx_"), nil, &receipt
		p.contextCompacting = false
		s.view.CanCompact = s.builtin && p.turns == 0 && !p.activeSubtasks.Load()
		changed, emitNotice = true, true
	} else {
		// A boundary is definitive completion on CLI versions that omit the
		// preceding progress frame. Preserve the unknown start time as zero.
		now := nowMs()
		receipt := protocol.ContextCompaction{OperationID: newUUID(), State: "completed", UpdatedAt: now}
		s.automatic = &receipt
		s.view.Revision, s.view.Snapshot, s.view.Compaction = newID("ctx_"), nil, &receipt
		s.view.CanCompact = s.builtin && p.turns == 0 && !p.activeSubtasks.Load()
		operationID, changed, emitNotice = receipt.OperationID, true, true
	}
	view := a.projectClaudeContextLocked(p, nil)
	a.contextState.mu.Unlock()
	p.mu.Unlock()
	a.mu.Unlock()
	if changed {
		a.emit(protocol.Event{SessionKey: view.SessionKey, Type: "context.updated", Context: &view})
	}
	if emitNotice {
		a.emit(protocol.Event{SessionKey: view.SessionKey, ID: claudeContextCompactionNoticeID(operationID), Type: "notice", Level: "info", NoticeKind: "context_compaction", Progress: false, Text: "上下文已压缩"})
	}
}

func (a *claudeAgent) finishClaudeAutomaticCompaction(p *claudeProc, state, reason string) bool {
	a.mu.Lock()
	p.mu.Lock()
	a.contextState.mu.Lock()
	s := a.contextState.sessions[p.id]
	if s == nil || s.worker != p || s.pending != nil || s.automatic == nil || s.automatic.State != "running" {
		a.contextState.mu.Unlock()
		p.mu.Unlock()
		a.mu.Unlock()
		return false
	}
	now := nowMs()
	s.automatic.State, s.automatic.Error, s.automatic.UpdatedAt, s.automatic.Blocking = state, reason, now, false
	s.view.Revision, s.view.Snapshot = newID("ctx_"), nil
	receipt := *s.automatic
	s.view.Compaction = &receipt
	p.contextCompacting = false
	view := a.projectClaudeContextLocked(p, nil)
	operationID := s.automatic.OperationID
	active := !a.closed
	a.contextState.mu.Unlock()
	p.mu.Unlock()
	a.mu.Unlock()
	if active {
		a.emit(protocol.Event{SessionKey: view.SessionKey, Type: "context.updated", Context: &view})
		text := "压缩结果未确认"
		if state == "failed" {
			text = "上下文压缩失败"
		}
		a.emit(protocol.Event{SessionKey: view.SessionKey, ID: claudeContextCompactionNoticeID(operationID), Type: "notice", Level: "info", NoticeKind: "context_compaction", Text: text})
	}
	return true
}

// Called only for this worker's root stream, after the reader's generation and
// parent-tool filters. Billing result/modelUsage fields are deliberately unused.
func (a *claudeAgent) observeClaudeContextLine(p *claudeProc, raw []byte) bool {
	var frame struct {
		Type, Subtype string
		Status        string          `json:"status"`
		Content       string          `json:"content"`
		IsError       bool            `json:"is_error"`
		UserUUID      string          `json:"user_message_uuid"`
		LocalCommand  string          `json:"local_command"`
		UUID          string          `json:"uuid"`
		AgentID       string          `json:"agent_id"`
		Sidechain     bool            `json:"isSidechain"`
		Origin        json.RawMessage `json:"origin"`
		Compact       struct {
			Trigger string `json:"trigger"`
		} `json:"compact_metadata"`
	}
	if json.Unmarshal(raw, &frame) != nil {
		return false
	}
	if frame.AgentID != "" || frame.Sidechain {
		return false
	}
	if frame.Type == "system" && (frame.Subtype == "status" && frame.Status == "compacting" ||
		frame.Subtype == "informational" && strings.TrimSpace(frame.Content) == "Compacting conversation…") {
		a.observeClaudeCompactionStart(p, frame.UUID)
		return true
	}
	if frame.Type == "system" && frame.Subtype == "compact_boundary" {
		a.observeClaudeCompactionBoundary(p, frame.UUID, frame.Compact.Trigger)
		return true
	}
	if frame.Type == "result" && claudeResultIsHuman(&claudeLine{Origin: frame.Origin}, p.id) {
		// A successful compact_boundary is the completion proof. If Claude has
		// returned to the root turn without one, retire the spinner as unknown.
		if frame.IsError || frame.Subtype != "success" {
			_ = a.finishClaudeAutomaticCompaction(p, "failed", "Claude 原生上下文压缩失败")
		} else {
			_ = a.finishClaudeAutomaticCompaction(p, "unconfirmed", "Claude 未提供压缩完成边界")
		}
	}
	a.contextState.mu.Lock()
	s := a.contextState.sessions[p.id]
	if s == nil || s.worker != p {
		a.contextState.mu.Unlock()
		return false
	}
	op := s.pending
	boundary := op != nil && op.boundary
	a.contextState.mu.Unlock()
	if op == nil {
		if frame.Type == "assistant" {
			a.invalidateClaudeContext(p)
		}
		return false
	}
	if frame.Type != "result" {
		return false
	}
	line := claudeLine{Origin: frame.Origin}
	if !claudeResultIsHuman(&line, p.id) || frame.UserUUID != "" && frame.UserUUID != op.uuid || frame.LocalCommand != "" && frame.LocalCommand != "compact" {
		return true
	}
	p.mu.Lock()
	fresh := p.rememberResultLocked(frame.UUID)
	p.mu.Unlock()
	if !fresh {
		return true
	}
	if err := a.claudeCompactSourceGuard(p, op); err != nil {
		a.finishClaudeCompact(p, op, "unconfirmed", "API 来源已变化，压缩结果未确认")
		a.stop(p)
		return true
	}
	if frame.Subtype == "success" && !frame.IsError && boundary {
		a.finishClaudeCompact(p, op, "completed", "")
	} else if frame.Subtype == "success" && !frame.IsError {
		a.finishClaudeCompact(p, op, "failed", "原生命令已结束但未执行压缩，可能没有足够的会话内容")
	} else {
		a.finishClaudeCompact(p, op, "failed", "Claude 原生压缩失败")
	}
	return true
}

var _ ContextProvider = (*claudeAgent)(nil)
var _ ContextCompactor = (*claudeAgent)(nil)
