package agents

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

const maxCodexSessionActionReceipts = 256
const codexSessionActionTimeout = 15 * time.Second

var errCodexSessionActionStale = errors.New("session_action_stale: 会话或运行连接已改变，请刷新后重试")
var errCodexSessionActionBusy = errors.New("session_action_busy: 会话管理操作尚未确认，或会话已归档")
var errCodexSessionActionUnsupported = errors.New("session_action_unsupported: 当前 Codex 运行版本未确认支持安全会话管理")
var errCodexSessionActionRead = errors.New("session_action_unavailable: 无法从原生会话目录确认当前状态")

// The only audited schema is 0.145.x. In particular, older forks may silently
// ignore deferGoalContinuation. A path, thread.cliVersion, or tool --version
// is not evidence about the initialized worker serving this request.
var codexSessionActionVersion = regexp.MustCompile(`^(?:Codex Desktop|codex_cli_rs)/0\.145\.[0-9]+(?:\s|$)`)

type codexSessionInitialize struct {
	UserAgent string `json:"userAgent"`
}

func (c *rpcConn) recordSessionInitialization(value codexSessionInitialize) {
	c.mu.Lock()
	c.initializeUA, c.actionsEpoch = value.UserAgent, config.NewUUID()
	c.mu.Unlock()
}

func (c *rpcConn) sessionActionRuntime() (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.actionsEpoch, !c.closed && !c.transportRetired() && c.actionsEpoch != "" && codexSessionActionVersion.MatchString(c.initializeUA)
}

type codexSessionActionState struct {
	conn         *rpcConn
	revision     string
	fingerprint  string
	nativeKey    string
	nativeStatus string
	change       string
	known        bool
	archived     bool
	memberCursor string
	busy         *codexSessionActionReceipt
	receipts     map[string]*codexSessionActionReceipt
	unsupported  map[string]bool
}

type codexSessionActionReceipt struct {
	conn       *rpcConn
	signature  string
	request    protocol.SessionActionRequest
	result     protocol.SessionActionResult
	err        error
	terminal   bool
	pending    <-chan rpcResult
	observerID int64
}

// Session mutations keep their response channel after the caller stops waiting.
// A later authorized read can consume a real acknowledgement exactly once. No
// goroutine waits on these channels, and the connection's normal response/exit
// paths remove them. Explicit review/goal workflows use the same bounded
// transport, but ordinary turns and read RPCs never retain abandoned replies.
func (c *rpcConn) callSessionActionGuarded(ctx context.Context, method string, params any, beforeWrite func(int64, <-chan rpcResult) error) (json.RawMessage, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	c.mu.Lock()
	if c.closed {
		err := c.err
		c.mu.Unlock()
		return nil, false, err
	}
	if len(c.sessionActionObservers) >= maxCodexSessionActionReceipts {
		c.mu.Unlock()
		return nil, false, errors.New("session_action_limit: 等待确认的原生会话操作已达上限")
	}
	c.nextID++
	id := c.nextID
	ch := make(chan rpcResult, 1)
	c.pending[id] = ch
	if c.pendingMethods == nil {
		c.pendingMethods = map[int64]string{}
	}
	c.pendingMethods[id] = method
	if c.sessionActionObservers == nil {
		c.sessionActionObservers = map[int64]struct{}{}
	}
	c.sessionActionObservers[id] = struct{}{}
	c.mu.Unlock()
	attempted, err := c.writeGuarded(ctx, map[string]any{"id": id, "method": method, "params": params}, func() error {
		return beforeWrite(id, ch)
	})
	if err != nil {
		if !attempted {
			c.releaseSessionActionObserver(id, true)
			return nil, false, err
		}
		return nil, false, errRPCDeliveryInterrupted
	}
	select {
	case reply := <-ch:
		c.releaseSessionActionObserver(id, false)
		return reply.result, true, reply.err
	case <-ctx.Done():
		return nil, false, &rpcDeliveryUncertainError{message: errRPCDeliveryInterrupted.message, cause: ctx.Err()}
	}
}

func (c *rpcConn) releaseSessionActionObserver(id int64, detach bool) {
	c.mu.Lock()
	delete(c.sessionActionObservers, id)
	if detach {
		delete(c.pending, id)
		delete(c.pendingMethods, id)
	}
	c.mu.Unlock()
}

func (t *codexThread) codexUnconfirmedForkLocked() bool {
	for _, op := range t.sessionActions.receipts {
		if op.request.Action == "fork" && op.result.State == "unconfirmed" && op.err == nil {
			return true
		}
	}
	return false
}

func (t *codexThread) codexSessionActionBusyLocked() bool {
	op := t.sessionActions.busy
	return t.revertPending || op != nil && op.conn == t.sessionActions.conn && !op.terminal
}

func (a *codexAgent) retireCodexSessionActionsLocked(t *codexThread) {
	if op := t.sessionActions.busy; op != nil && !op.terminal {
		op.result.State, op.result.Error = "unconfirmed", "运行连接已改变，无法确认原生会话操作结果；不会重发"
		op.result.Session = nil
		op.terminal = true
		op.conn.releaseSessionActionObserver(op.observerID, true)
		op.pending = nil
	}
	t.sessionActions.conn, t.sessionActions.busy = nil, nil
	t.sessionActions.known = false
	t.sessionActions.fingerprint, t.sessionActions.nativeKey = "", ""
	t.sessionActions.nativeStatus = ""
	t.sessionActions.revision = config.NewUUID()
	a.sessionActionsVersion++
}

func (a *codexAgent) syncCodexSessionActionsLocked(t *codexThread) {
	if t.sessionActions.conn != a.conn {
		a.retireCodexSessionActionsLocked(t)
		t.sessionActions.conn = a.conn
		t.sessionActions.unsupported = nil
	}
	if t.sessionActions.revision == "" {
		t.sessionActions.revision = config.NewUUID()
	}
}

func sessionActionHash(value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (a *codexAgent) codexSessionActionsLocked(t *codexThread) protocol.SessionActions {
	a.syncCodexSessionActionsLocked(t)
	a.syncCodexContextLocked(t)
	state := &t.sessionActions
	epoch, supported := a.conn.sessionActionRuntime()
	fingerprint := sessionActionHash([]any{epoch, state.nativeKey, state.change, state.known, state.archived,
		t.info.Title, t.info.Status, t.info.ActiveTurnID, t.info.LastTerminalTurnID, t.turnID, t.turnVersion,
		t.context.revision, t.info.PermissionMode, t.permissionUnconfirmed, t.statusUnconfirmed,
		t.childStates, t.childOverflow, t.codexSessionActionBusyLocked(), t.codexUnconfirmedForkLocked(),
		t.workflows.change, codexGoalIdentity(t.workflows.goal), a.aps.countFor(t.info.SessionKey)})
	if state.fingerprint != fingerprint {
		state.fingerprint, state.revision = fingerprint, config.NewUUID()
	}
	out := protocol.SessionActions{SessionKey: t.info.SessionKey, Revision: state.revision,
		Title: t.info.Title, Archived: state.archived, Actions: []string{}}
	switch {
	case a.closed || a.conn == nil || !a.conn.alive():
		out.UnavailableReason = "运行连接尚未就绪"
	case !supported:
		out.UnavailableReason = "当前 Codex 版本不支持"
	case !state.known:
		out.UnavailableReason = "尚未确认原生会话状态"
	case t.codexSessionActionBusyLocked():
		out.UnavailableReason = "上次操作结果未确认"
	case t.codexWorkflowBlocksMutationLocked():
		out.UnavailableReason = "工作流尚未确认或目标仍可继续运行"
	case t.codexCompactionBusyLocked():
		out.UnavailableReason = "原生上下文压缩尚未结束"
	case t.statusUnconfirmed || t.permissionUnconfirmed:
		out.UnavailableReason = "运行状态尚未确认"
	case t.turnID != "" || t.info.ActiveTurnID != "" || t.info.Status != "idle" || !t.info.Controllable || t.childOverflow || a.aps.countFor(t.info.SessionKey) != 0:
		out.UnavailableReason = "会话正忙"
	default:
		for _, status := range t.childStates {
			if status != "completed" && status != "interrupted" && status != "errored" && status != "failed" && status != "shutdown" && status != "notFound" {
				out.UnavailableReason = "会话正忙"
				return out
			}
		}
		actions := []string{"rename", "archive", "fork"}
		if state.archived {
			actions = []string{"unarchive"}
		}
		for _, action := range actions {
			if action == "fork" && t.codexUnconfirmedForkLocked() {
				continue
			}
			if !state.unsupported[action] {
				out.Actions = append(out.Actions, action)
			}
		}
		if len(out.Actions) == 0 {
			out.UnavailableReason = "当前 Codex 版本不支持此操作"
		}
	}
	return out
}

func (a *codexAgent) sessionActionGuard(ctx context.Context, c *rpcConn, s Settings, validate func() error) error {
	if err := validateCodexContextCall(ctx, validate); err != nil {
		return err
	}
	a.mu.Lock()
	current := !a.closed && a.conn == c && a.connSig == codexSig(s)
	a.mu.Unlock()
	if !current {
		return errCodexSessionActionStale
	}
	if _, supported := c.sessionActionRuntime(); !supported {
		return errCodexSessionActionUnsupported
	}
	return nil
}

type codexSessionListReply struct {
	Data       []cxThread `json:"data"`
	NextCursor string     `json:"nextCursor"`
}

func (a *codexAgent) readSessionActionPage(ctx context.Context, c *rpcConn, s Settings, archived bool, cursor string, limit int, validate func() error) (codexSessionListReply, error) {
	var reply codexSessionListReply
	guard := func() error { return a.sessionActionGuard(ctx, c, s, validate) }
	if err := guard(); err != nil {
		return reply, err
	}
	params := map[string]any{"archived": archived, "limit": limit, "sortKey": "updated_at", "modelProviders": []string{}, "sourceKinds": codexSourceKinds}
	if cursor != "" {
		params["cursor"] = cursor
	}
	if err := c.callGuarded(ctx, "thread/list", params, &reply, guard); err != nil {
		return reply, errCodexSessionActionRead
	}
	if err := guard(); err != nil {
		return reply, err
	}
	if reply.Data == nil || len(reply.Data) > limit || len(reply.NextCursor) > 2048 || reply.NextCursor != "" && reply.NextCursor == cursor {
		return reply, errCodexSessionActionRead
	}
	seen := map[string]bool{}
	for _, th := range reply.Data {
		if !validChildID(th.ID) || seen[th.ID] {
			return reply, errCodexSessionActionRead
		}
		seen[th.ID] = true
	}
	return reply, nil
}

// Thread has no archived field in the native schema. Only membership in an
// explicitly filtered native directory (or its mutation acknowledgement) is
// archive evidence. Absence from one page and rollout paths prove nothing.
func (a *codexAgent) findSessionActionMember(ctx context.Context, c *rpcConn, s Settings, t *codexThread, validate func() error) (cxThread, bool, string, error) {
	a.mu.Lock()
	first, hint := t.sessionActions.archived, t.sessionActions.memberCursor
	a.mu.Unlock()
	for _, archived := range []bool{first, !first} {
		cursor := ""
		if archived == first && hint != "" {
			page, err := a.readSessionActionPage(ctx, c, s, archived, hint, 100, validate)
			if err != nil {
				return cxThread{}, false, "", err
			}
			for _, th := range page.Data {
				if th.ID == t.id && !th.Ephemeral {
					return th, archived, hint, nil
				}
			}
		}
		seen := map[string]bool{}
		for pages := 0; pages < 16; pages++ {
			page, err := a.readSessionActionPage(ctx, c, s, archived, cursor, 100, validate)
			if err != nil {
				return cxThread{}, false, "", err
			}
			for _, th := range page.Data {
				if th.ID == t.id && !th.Ephemeral {
					return th, archived, cursor, nil
				}
			}
			if page.NextCursor == "" {
				break
			}
			if seen[page.NextCursor] {
				return cxThread{}, false, "", errCodexSessionActionRead
			}
			seen[page.NextCursor], cursor = true, page.NextCursor
		}
	}
	return cxThread{}, false, "", errCodexSessionActionRead
}

func (a *codexAgent) refreshSessionActions(ctx context.Context, c *rpcConn, s Settings, t *codexThread, validate func() error) (protocol.SessionActions, error) {
	var empty protocol.SessionActions
	a.mu.Lock()
	version, readVersion := t.turnVersion, t.historyReadVersion
	a.mu.Unlock()
	th, archived, memberCursor, err := a.findSessionActionMember(ctx, c, s, t, validate)
	if err != nil {
		return empty, err
	}
	if !archived {
		// A stored thread may have a quiet turn in another worker. Read its
		// native turn status before allowing any management mutation.
		var reply struct {
			Thread cxThread `json:"thread"`
		}
		guard := func() error { return a.sessionActionGuard(ctx, c, s, validate) }
		if err := c.callGuarded(ctx, "thread/read", map[string]any{"threadId": t.id, "includeTurns": true}, &reply, guard); err != nil {
			return empty, errCodexSessionActionRead
		}
		if reply.Thread.ID != t.id || reply.Thread.Ephemeral {
			return empty, errCodexSessionActionRead
		}
		th = reply.Thread
	}
	if err := a.sessionActionGuard(ctx, c, s, validate); err != nil {
		return empty, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conn != c || a.threads[t.id] != t || t.turnVersion != version || t.historyReadVersion != readVersion {
		return empty, errCodexSessionActionStale
	}
	a.syncCodexSessionActionsLocked(t)
	a.applyThreadLocked(th)
	if archived && (th.Status.Type == "idle" || th.Status.Type == "notLoaded") && !codexThreadInProgress(th) && t.turnID == "" {
		// Archiving itself can update the rollout mtime. Native archive
		// membership is stronger than the external-file recency heuristic.
		t.info.Status, t.info.Controllable = "idle", true
	}
	if th.Status.Type == "" || codexThreadInProgress(th) {
		t.info.Controllable = false
	}
	t.info.Archived = archived
	t.sessionActions.known, t.sessionActions.archived, t.sessionActions.memberCursor = true, archived, memberCursor
	t.sessionActions.nativeKey = sessionActionHash(th)
	t.sessionActions.nativeStatus = th.Status.Type
	t.historyReadVersion++
	return a.codexSessionActionsLocked(t), nil
}

func (a *codexAgent) SessionActions(ctx context.Context, id string, validate func() error) (protocol.SessionActions, error) {
	var empty protocol.SessionActions
	if !validChildID(id) {
		return empty, errCodexSessionActionStale
	}
	if err := validateCodexContextCall(ctx, validate); err != nil {
		return empty, err
	}
	s, err := a.requireRelay(ctx)
	if err != nil {
		return empty, err
	}
	c, err := a.ensure(ctx)
	if err != nil {
		return empty, err
	}
	a.mu.Lock()
	if a.closed || a.conn != c || a.connSig != codexSig(s) {
		a.mu.Unlock()
		return empty, errCodexSessionActionStale
	}
	t := a.threadLocked(id)
	a.syncCodexSessionActionsLocked(t)
	out := a.codexSessionActionsLocked(t)
	a.mu.Unlock()
	if _, supported := c.sessionActionRuntime(); !supported {
		if err := validateCodexContextCall(ctx, validate); err != nil {
			return empty, err
		}
		return out, nil
	}
	t.controlMu.Lock()
	defer t.controlMu.Unlock()
	if err := a.sessionActionGuard(ctx, c, s, validate); err != nil {
		return empty, err
	}
	a.mu.Lock()
	a.pollCodexSessionActionLocked(t, t.sessionActions.busy)
	a.mu.Unlock()
	out, err = a.refreshSessionActions(ctx, c, s, t, validate)
	if err != nil {
		return empty, err
	}
	if err := a.sessionActionGuard(ctx, c, s, validate); err != nil {
		return empty, err
	}
	return out, nil
}

func hasSessionAction(actions []string, action string) bool {
	for _, value := range actions {
		if value == action {
			return true
		}
	}
	return false
}

func cloneSessionActionResult(value protocol.SessionActionResult) protocol.SessionActionResult {
	if value.Session != nil {
		info := *value.Session
		value.Session = &info
	}
	return value
}

func (a *codexAgent) ApplySessionAction(ctx context.Context, id string, request protocol.SessionActionRequest, validate func() error) (protocol.SessionActionResult, error) {
	var empty protocol.SessionActionResult
	if !validChildID(id) || !ValidSessionAction(request) {
		return empty, errors.New("session_action_invalid: 会话操作参数无效")
	}
	if err := validateCodexContextCall(ctx, validate); err != nil {
		return empty, err
	}
	a.mu.Lock()
	t := a.threads[id]
	a.mu.Unlock()
	if t == nil {
		return empty, errCodexSessionActionStale
	}
	t.controlMu.Lock()
	defer t.controlMu.Unlock()
	if err := validateCodexContextCall(ctx, validate); err != nil {
		return empty, err
	}
	receiptSig := codexSig(settingsForContext(ctx, a.settings))
	a.mu.Lock()
	old := t.sessionActions.receipts[request.OperationID]
	if old != nil {
		if old.request != request {
			a.mu.Unlock()
			return empty, errors.New("session_action_conflict: 操作标识已用于不同内容")
		}
		if old.signature == receiptSig && a.connSig == receiptSig {
			a.pollCodexSessionActionLocked(t, old)
		}
		out, err := cloneSessionActionResult(old.result), old.err
		if (old.conn != a.conn || old.signature != receiptSig || a.connSig != receiptSig || !old.conn.alive()) && err == nil {
			out.State, out.Session, out.Error = "unconfirmed", nil, "运行连接已改变，请在电脑核对原操作；不会重发"
		}
		a.mu.Unlock()
		if validationErr := validateCodexContextCall(ctx, validate); validationErr != nil {
			return empty, validationErr
		}
		finalSig := codexSig(settingsForContext(ctx, a.settings))
		a.mu.Lock()
		if a.closed || old.conn != a.conn || !old.conn.alive() || old.signature != a.connSig || old.signature != finalSig {
			out.State, out.Session, out.Error = "unconfirmed", nil, "运行连接已改变，请在电脑核对原操作；不会重发"
		}
		a.mu.Unlock()
		return out, err
	}
	c := a.conn
	a.mu.Unlock()
	s, err := a.requireRelay(ctx)
	if err != nil {
		return empty, err
	}
	if err := a.sessionActionGuard(ctx, c, s, validate); err != nil {
		return empty, err
	}
	out, err := a.refreshSessionActions(ctx, c, s, t, validate)
	if err != nil {
		return empty, err
	}
	if out.Revision != request.ExpectedRevision || !hasSessionAction(out.Actions, request.Action) {
		return empty, errCodexSessionActionStale
	}
	a.mu.Lock()
	cwd := t.info.Cwd
	a.mu.Unlock()
	if err := validateCodexEffectiveProviderAt(ctx, c, s, cwd); err != nil {
		return empty, err
	}
	method := map[string]string{"rename": "thread/name/set", "archive": "thread/archive", "unarchive": "thread/unarchive", "fork": "thread/fork"}[request.Action]
	params := map[string]any{"threadId": id}
	if request.Action == "rename" {
		params["name"] = request.Name
	}
	if request.Action == "fork" {
		params["deferGoalContinuation"], params["excludeTurns"] = true, true
	}
	var op *codexSessionActionReceipt
	callCtx, cancel := context.WithTimeout(ctx, codexSessionActionTimeout)
	defer cancel()
	reply, answered, err := c.callSessionActionGuarded(callCtx, method, params, func(observerID int64, pending <-chan rpcResult) error {
		if err := a.sessionActionGuard(callCtx, c, s, validate); err != nil {
			return err
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		current := a.codexSessionActionsLocked(t)
		if a.conn != c || a.threads[id] != t || current.Revision != request.ExpectedRevision || !hasSessionAction(current.Actions, request.Action) {
			return errCodexSessionActionStale
		}
		if len(t.sessionActions.receipts) >= maxCodexSessionActionReceipts {
			return errors.New("session_action_limit: 当前会话的操作记录已达上限")
		}
		op = &codexSessionActionReceipt{conn: c, signature: codexSig(s), request: request, pending: pending, observerID: observerID, result: protocol.SessionActionResult{
			SessionKey: "codex:" + id, OperationID: request.OperationID, Action: request.Action, State: "unconfirmed",
			Error: "原生会话操作已发出，但尚未确认结果；不会重发"}}
		if t.sessionActions.receipts == nil {
			t.sessionActions.receipts = map[string]*codexSessionActionReceipt{}
		}
		t.sessionActions.receipts[request.OperationID], t.sessionActions.busy = op, op
		t.sessionActions.change = config.NewUUID()
		t.historyReadVersion++
		a.sessionActionsVersion++
		return nil
	})
	if op == nil {
		return empty, err
	}
	a.mu.Lock()
	if answered {
		op.pending = nil
	}
	event := a.finishCodexSessionActionLocked(t, op, reply, err)
	result, resultErr := cloneSessionActionResult(op.result), op.err
	a.mu.Unlock()
	if validationErr := validateCodexContextCall(ctx, validate); validationErr != nil {
		return empty, validationErr
	}
	a.mu.Lock()
	current := !a.closed && a.conn == c && c.alive() && a.connSig == codexSig(s)
	a.mu.Unlock()
	if !current {
		result.State, result.Session, result.Error, event = "unconfirmed", nil, "运行连接已改变，请在电脑核对原操作；不会重发", nil
	}
	if event != nil {
		a.emit(protocol.Event{SessionKey: event.SessionKey, Type: "session.updated", Session: event})
	}
	return result, resultErr
}

// Only callers holding controlMu and a.mu may consume an observer. Native idle,
// title, or directory reads never release the fence: an older archive could
// still be waiting in the shared worker and otherwise race the next Send.
func (a *codexAgent) pollCodexSessionActionLocked(t *codexThread, op *codexSessionActionReceipt) {
	if op == nil || op.terminal || op.pending == nil || op.conn != a.conn || !op.conn.alive() || t.sessionActions.conn != a.conn {
		return
	}
	select {
	case reply := <-op.pending:
		op.conn.releaseSessionActionObserver(op.observerID, false)
		op.pending = nil
		a.finishCodexSessionActionLocked(t, op, reply.result, reply.err)
	default:
	}
}

func (a *codexAgent) finishCodexSessionActionLocked(t *codexThread, op *codexSessionActionReceipt, reply json.RawMessage, err error) *protocol.SessionInfo {
	c, id, request := op.conn, t.id, op.request
	var native struct {
		Thread cxThread `json:"thread"`
		Model  string   `json:"model"`
		codexPermissionReply
	}
	validAck := err == nil && len(bytes.TrimSpace(reply)) > 0 && bytes.TrimSpace(reply)[0] == '{' && json.Unmarshal(reply, &native) == nil
	if validAck && (request.Action == "rename" || request.Action == "archive") {
		var fields map[string]json.RawMessage
		validAck = json.Unmarshal(reply, &fields) == nil && len(fields) == 0
	}
	if validAck && (request.Action == "unarchive" || request.Action == "fork") {
		validAck = validChildID(native.Thread.ID) && !native.Thread.Ephemeral && !codexThreadInProgress(native.Thread) && (native.Thread.Status.Type == "idle" || native.Thread.Status.Type == "notLoaded")
		if request.Action == "unarchive" {
			validAck = validAck && native.Thread.ID == id
		} else {
			validAck = validAck && native.Thread.ID != id && native.Thread.ForkedFromID != nil && *native.Thread.ForkedFromID == id && native.Model != ""
		}
	}
	var event *protocol.SessionInfo
	if a.conn != c || a.threads[id] != t || a.closed || !c.alive() || t.sessionActions.conn != c {
		op.result.State, op.result.Error, op.result.Session = "unconfirmed", "运行连接已改变，无法确认原生会话操作结果；不会重发", nil
		op.terminal = true
		c.releaseSessionActionObserver(op.observerID, true)
		op.pending = nil
	} else if validAck {
		op.terminal, op.result.State, op.result.Error = true, "completed", ""
		t.sessionActions.busy = nil
		switch request.Action {
		case "rename":
			t.info.Title = request.Name
		case "archive":
			t.info.Archived, t.sessionActions.archived, t.loaded = true, true, false
		case "unarchive":
			a.applyThreadLocked(native.Thread)
			t.info.Archived, t.sessionActions.archived, t.loaded = false, false, false
		case "fork":
			child := a.applyThreadLocked(native.Thread)
			child.info.Archived, child.info.Model, child.loaded = false, native.Model, true
			child.permissionUnconfirmed = true
			child.inheritedFullAccess = native.Sandbox.Type == "dangerFullAccess"
			for _, mode := range []string{"ask", "auto_review", "full_access"} {
				if native.codexPermissionReply.matches(mode) {
					child.info.PermissionMode, child.permissionUnconfirmed = mode, false
				}
			}
			a.owned[child.id] = true
			a.saveStateLocked()
			info := child.info
			op.result.Session, event = &info, &info
		}
		if request.Action != "fork" {
			info := t.info
			op.result.Session, event = &info, &info
		}
	} else {
		var rejection *rpcError
		if errors.As(err, &rejection) {
			op.terminal, t.sessionActions.busy = true, nil
			op.err = errors.New("session_action_rejected: 原生运行时拒绝了会话操作")
			if rejection.Code == -32601 {
				if t.sessionActions.unsupported == nil {
					t.sessionActions.unsupported = map[string]bool{}
				}
				t.sessionActions.unsupported[request.Action] = true
				op.err = errCodexSessionActionUnsupported
			}
		}
	}
	if a.conn == c && a.threads[id] == t && t.sessionActions.conn == c {
		t.sessionActions.change = config.NewUUID()
		a.sessionActionsVersion++
	}
	return event
}

type codexArchivedCursor struct {
	Epoch   string `json:"e"`
	Version uint64 `json:"v"`
	Next    string `json:"n"`
}

func (a *codexAgent) ArchivedSessions(ctx context.Context, cursor string, limit int, validate func() error) ([]protocol.SessionInfo, string, error) {
	if err := validateCodexContextCall(ctx, validate); err != nil {
		return nil, "", err
	}
	s, err := a.requireRelay(ctx)
	if err != nil {
		return nil, "", err
	}
	c, err := a.ensure(ctx)
	if err != nil {
		return nil, "", err
	}
	epoch, supported := c.sessionActionRuntime()
	if !supported {
		return nil, "", errCodexSessionActionUnsupported
	}
	a.mu.Lock()
	version := a.sessionActionsVersion
	a.mu.Unlock()
	nativeCursor := ""
	if cursor != "" {
		var parsed codexArchivedCursor
		if readCursor(cursor, &parsed) != nil || parsed.Epoch != epoch || parsed.Version != version || parsed.Next == "" {
			return nil, "", errCodexSessionActionStale
		}
		nativeCursor = parsed.Next
	}
	limit = pageLimit(limit, maxSessions)
	page, err := a.readSessionActionPage(ctx, c, s, true, nativeCursor, limit, validate)
	if err != nil {
		return nil, "", err
	}
	a.mu.Lock()
	if a.conn != c || a.closed || a.sessionActionsVersion != version {
		a.mu.Unlock()
		return nil, "", errCodexSessionActionStale
	}
	result := []protocol.SessionInfo{}
	for _, th := range page.Data {
		if th.Ephemeral {
			continue
		}
		t := a.applyThreadLocked(th)
		a.syncCodexSessionActionsLocked(t)
		t.info.Archived = true
		t.sessionActions.known, t.sessionActions.archived, t.sessionActions.memberCursor = true, true, nativeCursor
		t.sessionActions.nativeKey = sessionActionHash(th)
		t.sessionActions.nativeStatus = th.Status.Type
		result = append(result, t.info)
	}
	version = a.sessionActionsVersion
	a.mu.Unlock()
	if err := a.sessionActionGuard(ctx, c, s, validate); err != nil {
		return nil, "", err
	}
	next := ""
	if page.NextCursor != "" {
		next = cursorValue(codexArchivedCursor{Epoch: epoch, Version: version, Next: page.NextCursor})
	}
	return result, next, nil
}
