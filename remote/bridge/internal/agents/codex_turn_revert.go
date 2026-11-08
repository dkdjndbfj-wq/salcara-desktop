package agents

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"salcara/bridge/internal/atomicfile"
	"salcara/bridge/internal/protocol"
)

var errTurnRevertUnavailable = errors.New("latest_turn_revert_unavailable: 当前原生会话不支持安全编辑最新消息")
var errTurnRevertStale = errors.New("latest_turn_revert_stale: 最新消息或运行状态已经变化，请刷新后重试")

// Persist the intent before the native write and retain completed tombstones.
// Neither a process restart nor an offline phone may replay or resurrect it.
type codexTurnRevertReceipt struct {
	Result         protocol.LatestTurnRevertResult `json:"result"`
	Signature      string                          `json:"signature"`
	Method         string                          `json:"method"`
	PreviousTurnID string                          `json:"previousTurnId"`
	Rejected       bool                            `json:"rejected,omitempty"`
}

// These malformed IDs cannot name a real UUID-based native thread. Only an
// explicit thread-ID validation error establishes that the method exists.
// Unknown-method, generic invalid-params, auth, transport and timeout fail closed.
const revertProbeThread = "salcara-invalid-thread-id"

func (c *rpcConn) probeTurnRevert(ctx context.Context) {
	probe := func(method string, params map[string]any) bool {
		probeCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
		defer cancel()
		err := c.Call(probeCtx, method, params, nil)
		var rpc *rpcError
		if !errors.As(err, &rpc) || rpc.Code == -32601 {
			return false
		}
		msg := strings.ToLower(rpc.Message)
		return strings.Contains(msg, "invalid thread id") || strings.Contains(msg, "invalid thread_id") || strings.Contains(msg, "invalid threadid")
	}
	revert := probe("thread/revert", map[string]any{"threadId": revertProbeThread, "beforeTurnId": "salcara-invalid-turn-id"})
	rollback := probe("thread/rollback", map[string]any{"threadId": revertProbeThread, "numTurns": 1})
	c.mu.Lock()
	c.latestRevertSupported, c.legacyRollbackSupported = revert, rollback
	c.mu.Unlock()
}
func (c *rpcConn) supportsAnyTurnRevert() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closed && !c.transportRetired() && (c.latestRevertSupported || c.legacyRollbackSupported)
}
func (c *rpcConn) turnRevertMethod(mode string) string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.transportRetired() {
		return ""
	}
	if mode == "paginated" && c.latestRevertSupported {
		return "thread/revert"
	}
	if (mode == "legacy" || mode == "") && c.legacyRollbackSupported {
		return "thread/rollback"
	}
	return ""
}

func (a *codexAgent) turnRevertPath() string {
	if a.stateFile == "" {
		return ""
	}
	return a.stateFile + ".turn-reverts.json"
}
func (a *codexAgent) loadTurnReverts() {
	a.turnReverts = map[string][]*codexTurnRevertReceipt{}
	path := a.turnRevertPath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil || len(data) > 2<<20 || json.Unmarshal(data, &a.turnReverts) != nil || a.turnReverts == nil {
		a.revertStoreError = errors.New("撤回记录无法读取，暂不能编辑已发送消息")
		return
	}
	for id, receipts := range a.turnReverts {
		if !validChildID(id) || len(receipts) > 256 {
			a.revertStoreError = errTurnRevertUnavailable
			return
		}
		for _, op := range receipts {
			if op == nil || op.Result.SessionKey != "codex:"+id || !ValidTurnID(op.Result.TurnID) || !ValidTurnID(op.Result.MessageID) || !ValidControlOperationID(op.Result.OperationID) || (op.Result.Status != "completed" && op.Result.Status != "unconfirmed") {
				a.revertStoreError = errTurnRevertUnavailable
				return
			}
			if op.Result.Status == "unconfirmed" && !op.Rejected {
				a.threadLocked(id).revertPending = true
			}
		}
	}
}
func (a *codexAgent) saveTurnRevertsLocked() error {
	if a.revertStoreError != nil {
		return a.revertStoreError
	}
	path := a.turnRevertPath()
	if path == "" {
		return errors.New("撤回记录存储尚未配置")
	}
	data, err := json.Marshal(a.turnReverts)
	if err != nil || len(data) > 2<<20 {
		return errors.New("撤回记录已达容量")
	}
	return atomicfile.WriteFile(path, data, 0600)
}

// A lost native acknowledgement can only be reconciled by a read proving the
// exact previous boundary. This never retries rollback/revert, even after reboot.
func (a *codexAgent) reconcileTurnReverts(ctx context.Context, id string) {
	a.mu.Lock()
	t := a.threads[id]
	pending := t != nil && t.revertPending
	a.mu.Unlock()
	if !pending {
		return
	}
	if _, err := a.ensure(ctx); err != nil {
		return
	}
	t.controlMu.Lock()
	defer t.controlMu.Unlock()
	a.reconcileTurnRevertsLocked(ctx, t, nil)
}
func (a *codexAgent) reconcileTurnRevertsLocked(ctx context.Context, t *codexThread, validate func() error) {
	a.mu.Lock()
	c, version, signature := a.conn, t.turnVersion, a.connSig
	var pending *codexTurnRevertReceipt
	for _, op := range a.turnReverts[t.id] {
		if op.Result.Status == "unconfirmed" && !op.Rejected {
			if pending != nil {
				a.mu.Unlock()
				return
			}
			pending = op
		}
	}
	a.mu.Unlock()
	if c == nil || pending == nil || pending.Signature != sessionActionHash(codexSig(settingsForContext(ctx, a.settings))) {
		return
	}
	guard := func() error {
		if err := validateCodexContextCall(ctx, validate); err != nil {
			return err
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.closed || a.conn != c || !c.alive() || a.connSig != signature || t.turnVersion != version || t.turnID != "" || t.info.ActiveTurnID != "" {
			return errTurnRevertStale
		}
		return nil
	}
	var meta struct {
		Thread cxThread `json:"thread"`
	}
	if c.callGuarded(ctx, "thread/read", map[string]any{"threadId": t.id, "includeTurns": false}, &meta, guard) != nil || meta.Thread.ID != t.id || meta.Thread.Status.Type != "idle" || meta.Thread.Ephemeral {
		return
	}
	remaining, _, err := readCodexTurns(ctx, c, t.id, "", 1)
	if err != nil || len(remaining) == 0 && pending.PreviousTurnID != "" || len(remaining) > 0 && (remaining[0].ID != pending.PreviousTurnID || remaining[0].ID == pending.Result.TurnID || remaining[0].Status == "inProgress") || guard() != nil {
		return
	}
	a.mu.Lock()
	if a.closed || a.conn != c || a.connSig != signature || t.turnVersion != version || t.turnID != "" || t.info.ActiveTurnID != "" {
		a.mu.Unlock()
		return
	}
	event := a.finishTurnRevertLocked(t, pending)
	a.mu.Unlock()
	if event != nil {
		a.emit(*event)
	}
}

func (a *codexAgent) finishTurnRevertLocked(t *codexThread, op *codexTurnRevertReceipt) *protocol.Event {
	t.turnID = ""
	t.info.ActiveTurnID = ""
	t.info.LastTerminalTurnID = op.PreviousTurnID
	t.info.Status = "idle"
	t.continuedTerminal = ""
	t.turnVersion++
	t.historyReadVersion++
	t.items = nil
	t.seen = nil
	t.historyItemDigests = nil
	t.historyTurnDigests = nil
	t.tailBaselineTurn = op.PreviousTurnID
	info := t.info
	op.Result.Status = "completed"
	op.Result.Session = &info
	if err := a.saveTurnRevertsLocked(); err != nil {
		op.Result.Status = "unconfirmed"
		op.Result.Session = nil
		return nil
	}
	t.revertPending = false
	return &protocol.Event{Type: "session.reverted", SessionKey: op.Result.SessionKey, ID: op.Result.MessageID, TurnID: op.Result.TurnID, OperationID: op.Result.OperationID}
}
func (a *codexAgent) revertedHistoryEvents(id string) []protocol.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	events := []protocol.Event{}
	for _, op := range a.turnReverts[id] {
		if op.Result.Status == "completed" && !op.Rejected {
			r := op.Result
			events = append(events, protocol.Event{Type: "session.reverted", Tool: "codex", TS: nowMs(), SessionKey: r.SessionKey, ID: r.MessageID, TurnID: r.TurnID, OperationID: r.OperationID})
		}
	}
	return events
}

func cloneTurnRevertResult(in protocol.LatestTurnRevertResult) protocol.LatestTurnRevertResult {
	if in.Session != nil {
		copy := *in.Session
		in.Session = &copy
	}
	return in
}
func (a *codexAgent) turnRevertGuard(ctx context.Context, c *rpcConn, t *codexThread, signature string, version uint64, validate func() error) error {
	if err := validateCodexContextCall(ctx, validate); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.conn != c || c == nil || !c.alive() || a.connSig != signature || a.threads[t.id] != t || !a.owned[t.id] || !t.loaded || t.ephemeral || t.turnVersion != version || t.turnID != "" || t.info.ActiveTurnID != "" || !t.info.Controllable || t.statusUnconfirmed || t.permissionUnconfirmed || t.info.Archived || t.info.ParentSessionKey != "" || t.info.Status != "idle" && t.info.Status != "failed" || t.childOverflow || a.aps.countFor(t.info.SessionKey) > 0 || t.codexWorkflowBlocksMutationLocked() || t.codexCompactionBusyLocked() {
		return errTurnRevertStale
	}
	if op := t.sessionActions.busy; op != nil && !op.terminal {
		return errTurnRevertStale
	}
	for _, status := range t.childStates {
		switch status {
		case "completed", "interrupted", "errored", "failed", "shutdown", "notFound":
		default:
			return errTurnRevertStale
		}
	}
	return nil
}
func latestRevertUser(turn cxTurn, id string) bool {
	if turn.ItemsView != "full" {
		return false
	}
	if turn.Status != "completed" && turn.Status != "interrupted" && turn.Status != "failed" {
		return false
	}
	count := 0
	for _, raw := range turn.Items {
		var item cxItem
		if json.Unmarshal(raw, &item) != nil {
			return false
		}
		if item.Type != "userMessage" {
			continue
		}
		count++
		if item.ID != id {
			return false
		}
		_, origin := codexUserInputProjection(item.Content)
		if origin != nil {
			return false
		}
	}
	return count == 1
}

// The entire latest-turn CAS is serialized with Send, Steer, Stop, workflows,
// compaction and session mutations. No native worker is started or resumed here.
func (a *codexAgent) RevertLatestTurn(ctx context.Context, id, expected, message, operation string, validate func() error) (protocol.LatestTurnRevertResult, error) {
	return a.revertLatestTurn(ctx, id, expected, message, operation, validate, false)
}
func (a *codexAgent) ReconcileLatestTurnRevert(ctx context.Context, id, expected, message, operation string, validate func() error) (protocol.LatestTurnRevertResult, error) {
	return a.revertLatestTurn(ctx, id, expected, message, operation, validate, true)
}
func (a *codexAgent) revertLatestTurn(ctx context.Context, id, expected, message, operation string, validate func() error, recoverOnly bool) (protocol.LatestTurnRevertResult, error) {
	var empty protocol.LatestTurnRevertResult
	if !validChildID(id) || !ValidTurnID(expected) || !ValidTurnID(message) || !ValidControlOperationID(operation) {
		return empty, errTurnRevertStale
	}
	a.mu.Lock()
	t := a.threads[id]
	a.mu.Unlock()
	if t == nil {
		return empty, errTurnRevertUnavailable
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
	signature := codexSig(s)
	a.mu.Lock()
	for _, old := range a.turnReverts[id] {
		if old.Result.OperationID == operation {
			if old.Result.TurnID != expected || old.Result.MessageID != message || old.Signature != sessionActionHash(signature) {
				a.mu.Unlock()
				return empty, errTurnRevertStale
			}
			out := cloneTurnRevertResult(old.Result)
			rejected := old.Rejected
			a.mu.Unlock()
			if rejected {
				return empty, errors.New("latest_turn_revert_rejected: 原生运行时拒绝了此操作")
			}
			if out.Status == "unconfirmed" {
				a.reconcileTurnRevertsLocked(ctx, t, validate)
				a.mu.Lock()
				out = cloneTurnRevertResult(old.Result)
				a.mu.Unlock()
			}
			return out, nil
		}
	}
	if recoverOnly {
		a.mu.Unlock()
		return empty, errTurnRevertUnavailable
	}
	c, version, cwd := a.conn, t.turnVersion, t.info.Cwd
	if a.revertStoreError != nil || t.revertPending || len(a.turnReverts[id]) >= 256 {
		a.mu.Unlock()
		return empty, errTurnRevertUnavailable
	}
	a.mu.Unlock()
	guard := func() error { return a.turnRevertGuard(ctx, c, t, signature, version, validate) }
	if err = guard(); err != nil {
		return empty, err
	}
	if err = validateCodexEffectiveProviderAt(ctx, c, s, cwd); err != nil {
		return empty, err
	}
	var metadata struct {
		Thread cxThread `json:"thread"`
	}
	if err = c.callGuarded(ctx, "thread/read", map[string]any{"threadId": id, "includeTurns": false}, &metadata, guard); err != nil {
		return empty, err
	}
	method := c.turnRevertMethod(metadata.Thread.HistoryMode)
	if metadata.Thread.ID != id || metadata.Thread.Ephemeral || metadata.Thread.ParentThreadID != nil || metadata.Thread.Status.Type != "idle" || method == "" {
		return empty, errTurnRevertUnavailable
	}
	turns, _, err := readCodexTurns(ctx, c, id, "", 2)
	if err != nil {
		return empty, err
	}
	if len(turns) == 0 || turns[0].ID != expected || !latestRevertUser(turns[0], message) {
		return empty, errTurnRevertStale
	}
	if receipt, ok := a.readInputReceipt(id, "native", message); ok && receipt.Origin != nil {
		return empty, errTurnRevertStale
	}
	previous := ""
	if len(turns) > 1 {
		previous = turns[1].ID
	}
	// Goal continuation is autonomous; an idle status alone is not sufficient.
	var goalRaw json.RawMessage
	if err = c.callGuarded(ctx, "thread/goal/get", map[string]any{"threadId": id}, &goalRaw, guard); err != nil {
		return empty, errTurnRevertUnavailable
	}
	goal, known := decodeCodexGoal(goalRaw, id)
	if !known || goal != nil && goal.Status == "active" {
		return empty, errTurnRevertStale
	}
	if err = guard(); err != nil {
		return empty, err
	}
	params := map[string]any{"threadId": id, "beforeTurnId": expected}
	if method == "thread/rollback" {
		params = map[string]any{"threadId": id, "numTurns": 1}
	}
	op := &codexTurnRevertReceipt{Signature: sessionActionHash(signature), Method: method, PreviousTurnID: previous, Result: protocol.LatestTurnRevertResult{Status: "unconfirmed", SessionKey: "codex:" + id, TurnID: expected, MessageID: message, OperationID: operation}}
	dispatched := false
	var raw json.RawMessage
	err = c.callGuarded(ctx, method, params, &raw, func() error {
		if err := guard(); err != nil {
			return err
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.closed || a.conn != c || a.connSig != signature || t.turnVersion != version || t.turnID != "" || t.info.ActiveTurnID != "" || t.revertPending {
			return errTurnRevertStale
		}
		if a.turnReverts == nil {
			a.turnReverts = map[string][]*codexTurnRevertReceipt{}
		}
		a.turnReverts[id] = append(a.turnReverts[id], op)
		if err := a.saveTurnRevertsLocked(); err != nil {
			a.turnReverts[id] = a.turnReverts[id][:len(a.turnReverts[id])-1]
			return err
		}
		t.revertPending = true
		t.historyReadVersion++
		dispatched = true
		return nil
	})
	if !dispatched {
		return empty, err
	}
	if err != nil {
		var rejection *rpcError
		// Internal/native execution errors can arrive after history committed.
		// Only protocol-level method/parameter rejection proves no mutation.
		if errors.As(err, &rejection) && (rejection.Code == -32601 || rejection.Code == -32602) {
			a.mu.Lock()
			op.Rejected = true
			t.revertPending = false
			saveErr := a.saveTurnRevertsLocked()
			if saveErr != nil {
				op.Rejected = false
				t.revertPending = true
			}
			a.mu.Unlock()
			return empty, errors.New("latest_turn_revert_rejected: 原生运行时拒绝了此操作")
		}
		return op.Result, nil
	}
	var ack struct {
		Thread      cxThread `json:"thread"`
		TurnsCursor *string  `json:"turnsBackwardsCursor"`
		ItemsCursor *string  `json:"itemsBackwardsCursor"`
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &ack) != nil || json.Unmarshal(raw, &fields) != nil || ack.Thread.ID != id || ack.Thread.Ephemeral || ack.Thread.Status.Type != "idle" {
		return op.Result, nil
	}
	if method == "thread/revert" {
		if _, ok := fields["turnsBackwardsCursor"]; !ok {
			return op.Result, nil
		}
		if _, ok := fields["itemsBackwardsCursor"]; !ok {
			return op.Result, nil
		}
		if len(ack.Thread.Turns) != 0 {
			return op.Result, nil
		}
	}
	if err = guard(); err != nil {
		return op.Result, nil
	}
	remaining, _, err := readCodexTurns(ctx, c, id, "", 1)
	if err != nil || len(remaining) == 0 && previous != "" || len(remaining) > 0 && (remaining[0].ID != previous || remaining[0].ID == expected) {
		return op.Result, nil
	}
	if err = guard(); err != nil {
		return op.Result, nil
	}
	a.mu.Lock()
	if a.closed || a.conn != c || a.connSig != signature || t.turnVersion != version || t.turnID != "" || t.info.ActiveTurnID != "" {
		a.mu.Unlock()
		return op.Result, nil
	}
	event := a.finishTurnRevertLocked(t, op)
	result := cloneTurnRevertResult(op.Result)
	a.mu.Unlock()
	if event != nil {
		a.emit(*event)
		a.emit(protocol.Event{Type: "session.updated", SessionKey: result.SessionKey, Session: result.Session})
	}
	return result, nil
}

var _ LatestTurnReverter = (*codexAgent)(nil)
