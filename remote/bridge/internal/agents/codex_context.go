package agents

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

const maxCodexCompactReceipts = 256
const maxContextTokenValue = int64(1<<53 - 1)
const codexCompactionDeadline = 2 * time.Minute

var errCodexContextStale = errors.New("context_stale: 上下文或运行状态已改变，请刷新后重试")
var errCodexCompactionBusy = errors.New("context_compaction_busy: 原生上下文压缩尚未结束，请稍后重试")

// All fields are owned by a.mu. controlMu serializes mutation admissions, but
// the RPC reader never takes it: notifications can arrive before the reply.
type codexContextState struct {
	conn          *rpcConn
	revision      string
	snapshot      *protocol.ContextSnapshot
	snapshotModel string
	compactedTurn string
	compaction    *codexCompaction
	receipts      map[string]*codexCompaction
	unavailable   string
	unsupported   bool
	terminalTurns map[string]bool
	terminalOrder []string
}

type codexCompaction struct {
	value                   protocol.ContextCompaction
	conn                    *rpcConn
	expectedRevision        string
	previousTerminal        string
	baseVersion             uint64
	turnID                  string
	itemID                  string
	manual                  bool
	observed                bool
	completed               bool
	terminal                bool
	timedOut                bool
	startedNoticeEmitted    bool
	completionNoticeEmitted bool
	done                    chan struct{}
	err                     error
}

func cloneCodexContextSnapshot(value *protocol.ContextSnapshot) *protocol.ContextSnapshot {
	if value == nil {
		return nil
	}
	copy := *value
	if value.WindowTokens != nil {
		window := *value.WindowTokens
		copy.WindowTokens = &window
	}
	return &copy
}

func (t *codexThread) codexCompactionBusyLocked() bool {
	op := t.context.compaction
	return op != nil && op.conn == t.context.conn && !op.terminal
}

func (a *codexAgent) retireCodexContextLocked(t *codexThread) {
	t.context.snapshot = nil
	t.context.revision = config.NewUUID()
	if op := t.context.compaction; op != nil && !op.terminal {
		op.value.State, op.value.Error, op.value.UpdatedAt = "unconfirmed", "运行连接已断开，无法确认压缩结果", nowMs()
		op.err = errRPCDeliveryInterrupted
		stopCodexCompactionTimerLocked(op)
	}
	t.context.conn = nil
}

func (a *codexAgent) syncCodexContextLocked(t *codexThread) {
	if t.context.conn != a.conn {
		a.retireCodexContextLocked(t)
		t.context.conn = a.conn
		if op := t.context.compaction; a.conn != nil && op != nil && op.conn != a.conn {
			// Receipts still fence an explicit replay of the old operation, but
			// an old worker's uncertainty is not the new worker's current task.
			t.context.compaction = nil
		}
		t.context.unavailable, t.context.unsupported = "", false
		t.context.terminalTurns, t.context.terminalOrder = nil, nil
		t.context.compactedTurn = ""
	}
	if t.context.revision == "" {
		t.context.revision = config.NewUUID()
	}
	if t.context.snapshot != nil && t.context.snapshotModel != t.info.Model {
		t.context.snapshot = nil
		t.context.revision = config.NewUUID()
	}
}

func stopCodexCompactionTimerLocked(op *codexCompaction) {
	if op.done != nil {
		close(op.done)
		op.done = nil
	}
}

// The deadline retires only progress certainty, never the native task or the
// busy lease. No timer may kill a shared connection, retry, or release a send.
func (a *codexAgent) watchCodexCompactionDeadlineLocked(t *codexThread, op *codexCompaction) {
	op.done = make(chan struct{})
	done := op.done
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		timer := time.NewTimer(codexCompactionDeadline)
		defer timer.Stop()
		select {
		case <-a.closeCh:
		case <-done:
		case <-timer.C:
			a.expireCodexCompaction(t, op)
		}
	}()
}

func (a *codexAgent) expireCodexCompaction(t *codexThread, op *codexCompaction) {
	a.mu.Lock()
	if a.closed || a.conn != op.conn || a.threads[t.id] != t || t.context.conn != op.conn || t.context.compaction != op || op.terminal || op.done == nil {
		a.mu.Unlock()
		return
	}
	stopCodexCompactionTimerLocked(op)
	op.timedOut = true
	// Native completion/failure is stronger than the progress deadline. Its
	// separate busy lease still waits for the actual native turn terminal.
	if op.value.State != "requested" && op.value.State != "running" {
		a.mu.Unlock()
		return
	}
	op.value.State, op.value.Error, op.value.UpdatedAt = "unconfirmed", "未收到原生压缩结束确认，请在电脑核对", nowMs()
	var notice *protocol.Event
	if op.startedNoticeEmitted && !op.completionNoticeEmitted {
		op.completionNoticeEmitted = true
		notice = &protocol.Event{SessionKey: "codex:" + t.id, ID: codexCompactionNoticeID(t.id, op), TurnID: op.turnID,
			Type: "notice", Level: "info", NoticeKind: "context_compaction", Text: "压缩结果未确认"}
	}
	t.context.revision = config.NewUUID()
	out := a.codexContextLocked(t)
	a.mu.Unlock()
	a.emit(protocol.Event{SessionKey: out.SessionKey, Type: "context.updated", Context: &out})
	if notice != nil {
		a.emit(*notice)
	}
}

func (a *codexAgent) codexContextLocked(t *codexThread) protocol.SessionContext {
	a.syncCodexContextLocked(t)
	out := protocol.SessionContext{SessionKey: "codex:" + t.id, Revision: t.context.revision}
	if op := t.context.compaction; op != nil {
		value := op.value
		value.Blocking = !a.closed && a.conn != nil && a.conn.alive() && t.loaded && t.codexCompactionBusyLocked()
		out.Compaction = &value
	}
	switch {
	case a.closed || a.conn == nil || !a.conn.alive() || !t.loaded:
		out.UnavailableReason = "runtime_not_loaded"
	case t.context.unavailable != "":
		out.UnavailableReason = t.context.unavailable
	case t.context.unsupported:
		out.UnavailableReason = "context_compaction_unsupported"
	case t.codexSessionActionBusyLocked() || t.info.Archived:
		out.UnavailableReason = "session_action_busy"
	case t.codexWorkflowBlocksMutationLocked():
		out.UnavailableReason = "session_workflow_busy"
	case t.codexCompactionBusyLocked():
		out.UnavailableReason = "context_compaction_busy"
	case t.statusUnconfirmed || t.permissionUnconfirmed || !validCodexPermissionMode(t.info.PermissionMode):
		out.UnavailableReason = "runtime_state_unconfirmed"
	case !t.info.Controllable || t.turnID != "" || t.info.ActiveTurnID != "" || t.info.Status != "idle" || t.childOverflow:
		out.UnavailableReason = "session_busy"
	default:
		out.CanCompact = true
		for _, state := range t.childStates {
			if state != "completed" && state != "interrupted" && state != "errored" && state != "failed" && state != "shutdown" && state != "notFound" {
				out.CanCompact, out.UnavailableReason = false, "session_busy"
				break
			}
		}
	}
	if !a.closed && a.conn != nil && a.conn.alive() && t.loaded && t.context.unavailable == "" {
		out.Snapshot = cloneCodexContextSnapshot(t.context.snapshot)
	}
	return out
}

func validateCodexContextCall(ctx context.Context, validate func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if validate != nil {
		return validate()
	}
	return nil
}

// SessionContext is an in-memory observation. thread/read has no tokenUsage
// field, so a missing native notification stays unknown; no resume, file scan,
// model request or cumulative billing counter is used to fabricate a reading.
func (a *codexAgent) SessionContext(ctx context.Context, id string, validate func() error) (protocol.SessionContext, error) {
	if err := validateCodexContextCall(ctx, validate); err != nil {
		return protocol.SessionContext{}, err
	}
	if !validChildID(id) {
		return protocol.SessionContext{}, errCodexContextStale
	}
	s := settingsForContext(ctx, a.settings)
	pending := a.aps.countFor("codex:" + id)
	a.mu.Lock()
	t := a.threads[id]
	out := protocol.SessionContext{SessionKey: "codex:" + id, UnavailableReason: "runtime_not_loaded"}
	if t != nil {
		out = a.codexContextLocked(t)
		if a.connSig != codexSig(s) {
			out.Snapshot, out.CanCompact, out.UnavailableReason = nil, false, "runtime_configuration_changed"
		} else if pending != 0 {
			out.CanCompact, out.UnavailableReason = false, "session_busy"
		}
	}
	a.mu.Unlock()
	if err := validateCodexContextCall(ctx, validate); err != nil {
		return protocol.SessionContext{}, err
	}
	return out, nil
}

// CompactContext only controls an already loaded Bridge-owned thread. Its
// empty native acknowledgement means requested, never completed. No operation
// ID, permissions, model override or slash-command text is sent to Codex.
func (a *codexAgent) CompactContext(ctx context.Context, id, expectedRevision, operationID string, validate func() error) (protocol.SessionContext, error) {
	var empty protocol.SessionContext
	if !validChildID(id) || expectedRevision == "" || len(expectedRevision) > 256 {
		return empty, errCodexContextStale
	}
	if !ValidControlOperationID(operationID) {
		return empty, errors.New("context_operation_required: 操作标识无效")
	}
	if err := validateCodexContextCall(ctx, validate); err != nil {
		return empty, err
	}
	a.mu.Lock()
	t := a.threads[id]
	a.mu.Unlock()
	if t == nil {
		return empty, errCodexContextStale
	}
	t.controlMu.Lock()
	defer t.controlMu.Unlock()
	if err := validateCodexContextCall(ctx, validate); err != nil {
		return empty, err
	}
	s, err := a.requireRelay(ctx)
	if err != nil {
		return empty, err
	}
	a.mu.Lock()
	if a.threads[id] != t {
		a.mu.Unlock()
		return empty, errCodexContextStale
	}
	a.syncCodexContextLocked(t)
	if old := t.context.receipts[operationID]; old != nil {
		if old.expectedRevision != expectedRevision {
			a.mu.Unlock()
			return empty, errors.New("context_operation_conflict: 操作内容已改变")
		}
		out := a.codexContextLocked(t)
		value := old.value
		value.Blocking = old == t.context.compaction && out.Compaction != nil && out.Compaction.Blocking
		out.Compaction = &value
		if a.connSig != codexSig(s) {
			out.Snapshot, out.CanCompact, out.UnavailableReason = nil, false, "runtime_configuration_changed"
		}
		err := old.err
		a.mu.Unlock()
		if validationErr := validateCodexContextCall(ctx, validate); validationErr != nil {
			return empty, validationErr
		}
		return out, err
	}
	c, cwd := a.conn, t.info.Cwd
	a.mu.Unlock()
	guard := func() error {
		if err := validateCodexContextCall(ctx, validate); err != nil {
			return err
		}
		if a.aps.countFor("codex:"+id) != 0 {
			return errCodexContextStale
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.closed || c == nil || a.conn != c || a.connSig != codexSig(s) || a.threads[id] != t {
			return errCodexContextStale
		}
		out := a.codexContextLocked(t)
		if !out.CanCompact || out.Revision != expectedRevision {
			return errCodexContextStale
		}
		if len(t.context.receipts) >= maxCodexCompactReceipts {
			return errors.New("context_operation_limit: 当前会话的压缩操作记录已达上限")
		}
		return nil
	}
	if err := guard(); err != nil {
		return empty, err
	}
	if err := validateCodexEffectiveProviderAt(ctx, c, s, cwd); err != nil {
		return empty, err
	}
	var op *codexCompaction
	var reply map[string]json.RawMessage
	err = c.callGuarded(ctx, "thread/compact/start", map[string]any{"threadId": id}, &reply, func() error {
		if err := guard(); err != nil {
			return err
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		// guard releases mu before returning; a native notification can win that
		// gap. Recheck the exact CAS while reserving the dispatch atomically.
		current := a.codexContextLocked(t)
		if a.conn != c || a.threads[id] != t || current.Revision != expectedRevision || !current.CanCompact {
			return errCodexContextStale
		}
		now := nowMs()
		op = &codexCompaction{conn: c, expectedRevision: expectedRevision, previousTerminal: t.info.LastTerminalTurnID, baseVersion: t.turnVersion, manual: true,
			value: protocol.ContextCompaction{OperationID: operationID, State: "requested", StartedAt: now, UpdatedAt: now}}
		if t.context.receipts == nil {
			t.context.receipts = make(map[string]*codexCompaction)
		}
		t.context.compaction, t.context.receipts[operationID] = op, op
		t.context.snapshot = nil
		t.context.revision = config.NewUUID()
		a.watchCodexCompactionDeadlineLocked(t, op)
		return nil
	})
	if op == nil {
		return empty, err
	}
	if err == nil && reply == nil {
		err = errRPCDeliveryInterrupted
	}
	a.mu.Lock()
	if a.conn != c || a.threads[id] != t || a.closed {
		// A late reply owns only its old operation. In particular it cannot
		// erase observations or retire a compaction on a replacement connection.
		if !op.terminal {
			op.value.State, op.value.Error, op.value.UpdatedAt = "unconfirmed", "运行连接已改变，无法确认压缩结果", nowMs()
			op.err = errRPCDeliveryInterrupted
		}
		stopCodexCompactionTimerLocked(op)
		a.mu.Unlock()
		return empty, errRPCDeliveryInterrupted
	} else if err != nil && !op.observed {
		op.err = err
		op.value.UpdatedAt = nowMs()
		var rejection *rpcError
		if errors.As(err, &rejection) {
			op.value.State, op.value.Error, op.terminal = "failed", "原生运行时拒绝了压缩请求", true
			stopCodexCompactionTimerLocked(op)
			if rejection.Code == -32601 {
				t.context.unsupported = true
				op.value.Error = "当前 Codex 版本不支持原生上下文压缩"
			}
		} else {
			op.value.State, op.value.Error = "unconfirmed", "压缩请求已发出，但尚未确认结果"
		}
		t.context.revision = config.NewUUID()
	} else if op.observed {
		// Correlated native progress is stronger evidence than a lost RPC reply.
		op.err, err = nil, nil
	}
	out := a.codexContextLocked(t)
	a.mu.Unlock()
	a.emit(protocol.Event{SessionKey: out.SessionKey, Type: "context.updated", Context: &out})
	if validate != nil {
		if validationErr := validate(); validationErr != nil {
			return empty, validationErr
		}
	}
	return out, err
}

func codexContextTurnLocked(t *codexThread, turnID string) bool {
	return ValidTurnID(turnID) && (t.turnID == turnID || t.turnID == "" && t.info.LastTerminalTurnID == turnID)
}

// Turn replay must be rejected before the ordinary timeline projection can
// overwrite turnID/LastTerminalTurnID. The native reader is ordered; a later
// turn cannot start while the current one is still active on this connection.
func (a *codexAgent) acceptCodexContextNotification(c *rpcConn, method string, params json.RawMessage) bool {
	if method != "turn/started" && method != "turn/completed" {
		return true
	}
	var n struct {
		ThreadID string  `json:"threadId"`
		Turn     *cxTurn `json:"turn"`
	}
	if json.Unmarshal(params, &n) != nil || !validChildID(n.ThreadID) || n.Turn == nil || !ValidTurnID(n.Turn.ID) {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.conn != c && (a.starting == nil || a.starting.conn != c || a.starting.ctx.Err() != nil) {
		return false
	}
	t := a.threads[n.ThreadID]
	if t == nil {
		return true
	}
	if t.info.LastTerminalTurnID == n.Turn.ID || t.context.conn == c && t.context.terminalTurns[n.Turn.ID] {
		return false
	}
	return t.turnID == "" || t.turnID == n.Turn.ID
}

func rememberCodexContextTerminalLocked(t *codexThread, turnID string) {
	if t.context.terminalTurns == nil {
		t.context.terminalTurns = make(map[string]bool)
	}
	if t.context.terminalTurns[turnID] {
		return
	}
	if len(t.context.terminalOrder) == 256 {
		delete(t.context.terminalTurns, t.context.terminalOrder[0])
		t.context.terminalOrder = t.context.terminalOrder[1:]
	}
	t.context.terminalTurns[turnID] = true
	t.context.terminalOrder = append(t.context.terminalOrder, turnID)
}

func (a *codexAgent) contextCompactionForItemLocked(t *codexThread, c *rpcConn, turnID, itemID string) *codexCompaction {
	if !codexContextTurnLocked(t, turnID) {
		return nil
	}
	op := t.context.compaction
	if op != nil && op.conn == c && op.turnID == turnID && op.terminal &&
		(itemID == "" || op.itemID == "" || op.itemID == itemID) {
		return op
	}
	if op != nil && op.conn == c && !op.terminal {
		if op.turnID != "" && op.turnID != turnID || op.itemID != "" && itemID != "" && op.itemID != itemID {
			return nil
		}
		if op.manual && op.turnID == "" && (turnID == op.previousTerminal || t.turnVersion < op.baseVersion) {
			return nil
		}
	} else {
		now := nowMs()
		op = &codexCompaction{conn: c, value: protocol.ContextCompaction{State: "running", StartedAt: now, UpdatedAt: now}}
		t.context.compaction = op
		a.watchCodexCompactionDeadlineLocked(t, op)
	}
	op.turnID = turnID
	if itemID != "" {
		op.itemID = itemID
	}
	op.observed, op.err = true, nil
	return op
}

func codexCompactionNoticeID(threadID string, op *codexCompaction) string {
	identity := op.itemID
	if identity == "" {
		identity = op.turnID
	}
	if identity == "" {
		identity = op.value.OperationID
	}
	if identity == "" {
		identity = "native"
	}
	return "context-compaction:" + threadID + ":" + identity
}

// Called after the normal notification projection, with the source connection
// retained. It neither takes controlMu nor performs any native RPC.
func (a *codexAgent) onCodexContextNotify(c *rpcConn, method string, params json.RawMessage) {
	switch method {
	case "thread/tokenUsage/updated", "turn/started", "turn/completed", "item/started", "item/completed", "thread/compacted", "thread/status/changed", "thread/closed", "model/rerouted", "error":
	default:
		return
	}
	var n struct {
		ThreadID string          `json:"threadId"`
		TurnID   string          `json:"turnId"`
		Turn     *cxTurn         `json:"turn"`
		Status   *cxThreadStatus `json:"status"`
		Item     struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"item"`
		WillRetry bool         `json:"willRetry"`
		Error     *cxTurnError `json:"error"`
		Usage     *struct {
			Last *struct {
				TotalTokens *int64 `json:"totalTokens"`
			} `json:"last"`
			Window *int64 `json:"modelContextWindow"`
		} `json:"tokenUsage"`
	}
	if json.Unmarshal(params, &n) != nil || !validChildID(n.ThreadID) {
		return
	}
	a.mu.Lock()
	t := a.threads[n.ThreadID]
	if a.closed || a.conn != c || t == nil || !t.loaded {
		a.mu.Unlock()
		return
	}
	a.syncCodexContextLocked(t)
	changed := false
	var compactionNotice *protocol.Event
	op := t.context.compaction
	switch method {
	case "thread/tokenUsage/updated":
		if !t.codexCompactionBusyLocked() && t.context.compactedTurn != n.TurnID && codexContextTurnLocked(t, n.TurnID) && n.Usage != nil && n.Usage.Last != nil && n.Usage.Last.TotalTokens != nil &&
			*n.Usage.Last.TotalTokens >= 0 && *n.Usage.Last.TotalTokens <= maxContextTokenValue &&
			(n.Usage.Window == nil || *n.Usage.Window > 0 && *n.Usage.Window <= maxContextTokenValue) {
			t.context.snapshot = &protocol.ContextSnapshot{UsedTokens: *n.Usage.Last.TotalTokens, WindowTokens: n.Usage.Window,
				Source: "codex_token_usage", Basis: "last_request", WindowKind: "model", MeasuredAt: nowMs()}
			// The usage notification has no model ID. Keep its current thread
			// generation privately for invalidation, without labeling a request
			// with a model override whose acknowledgement may still be in flight.
			t.context.snapshotModel = t.info.Model
			changed = true
		}
	case "turn/started":
		if n.Turn != nil && ValidTurnID(n.Turn.ID) && t.turnID == n.Turn.ID {
			if op != nil && op.conn == c && !op.terminal && op.manual && op.turnID == "" && t.turnVersion == op.baseVersion+1 {
				op.turnID = n.Turn.ID
				t.context.compactedTurn = n.Turn.ID
			}
			changed = true
		}
	case "item/started", "item/completed", "thread/compacted":
		if method != "thread/compacted" && (n.Item.Type != "contextCompaction" || !validChildID(n.Item.ID)) {
			break
		}
		op = a.contextCompactionForItemLocked(t, c, n.TurnID, n.Item.ID)
		if op == nil {
			break
		}
		if op.terminal || method == "item/started" && op.completed || method != "item/started" && op.completed {
			// Replayed item/started or the deprecated completion notification
			// cannot revive an operation or replace its stable operation ID.
			break
		}
		if method == "item/started" && op.timedOut {
			// A delayed native start is not a new lifecycle transition. Keep the
			// unconfirmed receipt and its revision until stronger evidence arrives.
			break
		}
		t.context.snapshot = nil
		t.context.compactedTurn = n.TurnID
		op.value.State, op.value.Error, op.value.UpdatedAt = "running", "", nowMs()
		if method != "item/started" {
			op.value.State = "completed"
			op.completed = true
			op.terminal = !op.manual || t.turnID == ""
			stopCodexCompactionTimerLocked(op)
			completionNoticeAlreadyEmitted := op.completionNoticeEmitted
			op.completionNoticeEmitted = true // item completion is projected by codexItemEvent
			if method == "thread/compacted" && !completionNoticeAlreadyEmitted {
				compactionNotice = &protocol.Event{SessionKey: "codex:" + t.id, ID: codexCompactionNoticeID(t.id, op), TurnID: op.turnID,
					Type: "notice", Level: "info", NoticeKind: "context_compaction", Text: "上下文已压缩"}
			}
		} else if op.timedOut {
			// A delayed start is still not an end confirmation and must not
			// revive an indefinite progress indicator after the deadline.
			op.value.State, op.value.Error = "unconfirmed", "未收到原生压缩结束确认，请在电脑核对"
		}
		if method == "item/started" && op.value.State == "running" {
			op.startedNoticeEmitted = true // codexItemEvent emits the stable progress row
		}
		changed = true
	case "turn/completed":
		if n.Turn == nil || !ValidTurnID(n.Turn.ID) || t.info.LastTerminalTurnID != n.Turn.ID || t.turnID != "" ||
			(n.Turn.Status != "completed" && n.Turn.Status != "failed" && n.Turn.Status != "interrupted") {
			break
		}
		rememberCodexContextTerminalLocked(t, n.Turn.ID)
		if op != nil && op.conn == c && op.turnID == n.Turn.ID && !op.terminal {
			op.terminal, op.value.UpdatedAt = true, nowMs()
			stopCodexCompactionTimerLocked(op)
			op.err = nil
			if n.Turn.Status == "completed" && op.completed {
				op.value.State, op.value.Error, op.err = "completed", "", nil
				t.context.snapshot = nil
			} else if n.Turn.Status == "completed" {
				op.value.State, op.value.Error = "unconfirmed", "原生任务已结束，但未收到压缩完成确认"
			} else {
				op.value.State, op.value.Error = "failed", "原生上下文压缩失败"
				if n.Turn.Status == "interrupted" {
					op.value.Error = "原生上下文压缩已中断"
				}
			}
			if op.startedNoticeEmitted && !op.completionNoticeEmitted {
				op.completionNoticeEmitted = true
				text := "压缩结果未确认"
				if op.value.State == "failed" {
					text = "上下文压缩失败"
				}
				compactionNotice = &protocol.Event{SessionKey: "codex:" + t.id, ID: codexCompactionNoticeID(t.id, op), TurnID: op.turnID,
					Type: "notice", Level: "info", NoticeKind: "context_compaction", Text: text}
			}
		}
		changed = true
	case "thread/status/changed":
		if n.Status == nil {
			break
		}
		switch n.Status.Type {
		case "active", "idle":
			t.context.unavailable = ""
		case "notLoaded", "systemError":
			t.context.snapshot, t.context.unavailable = nil, "runtime_state_unconfirmed"
		default:
			t.context.unavailable = "runtime_state_unconfirmed"
		}
		changed = true
	case "thread/closed":
		if op != nil && op.conn == c && !op.terminal && op.startedNoticeEmitted && !op.completionNoticeEmitted {
			op.completionNoticeEmitted = true
			compactionNotice = &protocol.Event{SessionKey: "codex:" + t.id, ID: codexCompactionNoticeID(t.id, op), TurnID: op.turnID,
				Type: "notice", Level: "info", NoticeKind: "context_compaction", Text: "压缩结果未确认"}
		}
		a.retireCodexContextLocked(t)
		t.context.conn, t.context.unavailable = c, "runtime_not_loaded"
		changed = true
	case "model/rerouted":
		t.context.snapshot = nil
		changed = true
	case "error":
		if !n.WillRetry && n.Error != nil && op != nil && op.conn == c && !op.terminal && op.turnID != "" && op.turnID == n.TurnID {
			op.value.State, op.value.Error, op.value.UpdatedAt = "failed", "原生上下文压缩发生错误，等待任务结束确认", nowMs()
			if op.startedNoticeEmitted && !op.completionNoticeEmitted {
				op.completionNoticeEmitted = true
				compactionNotice = &protocol.Event{SessionKey: "codex:" + t.id, ID: codexCompactionNoticeID(t.id, op), TurnID: op.turnID,
					Type: "notice", Level: "info", NoticeKind: "context_compaction", Text: "上下文压缩失败"}
			}
			changed = true
		}
	}
	if !changed {
		a.mu.Unlock()
		return
	}
	t.context.revision = config.NewUUID()
	out := a.codexContextLocked(t)
	a.mu.Unlock()
	a.emit(protocol.Event{SessionKey: out.SessionKey, Type: "context.updated", Context: &out})
	if compactionNotice != nil {
		a.emit(*compactionNotice)
	}
}

var _ ContextProvider = (*codexAgent)(nil)
var _ ContextCompactor = (*codexAgent)(nil)
