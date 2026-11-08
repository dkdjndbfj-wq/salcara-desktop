package agents

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

const maxCodexWorkflowReceipts = 256
const codexWorkflowTimeout = 15 * time.Second

var errCodexWorkflowStale = errors.New("session_workflow_stale: 会话、目标或运行连接已改变，请刷新")
var errCodexWorkflowBusy = errors.New("session_workflow_busy: 原生工作流尚未确认，或目标仍可继续运行")
var errCodexWorkflowUnavailable = errors.New("session_workflow_unavailable: 无法确认原生工作流状态")
var errCodexWorkflowUnsupported = errors.New("session_workflow_unsupported: 当前 Codex 版本不支持此工作流")

// All fields belong to a.mu; controlMu serializes admissions and receipt
// consumption. Notifications never take controlMu or issue a native RPC.
type codexWorkflowState struct {
	conn              *rpcConn
	revision          string
	fingerprint       string
	change            string
	goal              *protocol.SessionGoal
	goalKnown         bool
	goalVersion       uint64
	goalUnsupported   bool
	reviewUnsupported bool
	busy              *codexWorkflowReceipt
	receipts          map[string]*codexWorkflowReceipt
}

type codexWorkflowReceipt struct {
	conn            *rpcConn
	signature       string
	request         protocol.SessionWorkflowRequest
	requestHash     string
	result          protocol.SessionWorkflowResult
	err             error
	terminal        bool
	pending         <-chan rpcResult
	observerID      int64
	baseTurnVersion uint64
	baseGoalVersion uint64
	baseGoal        *protocol.SessionGoal
}

func cloneCodexGoal(value *protocol.SessionGoal) *protocol.SessionGoal {
	if value == nil {
		return nil
	}
	out := *value
	if value.TokenBudget != nil {
		budget := *value.TokenBudget
		out.TokenBudget = &budget
	}
	return &out
}

func cloneCodexWorkflowRequest(value protocol.SessionWorkflowRequest) protocol.SessionWorkflowRequest {
	if value.Target != nil {
		target := *value.Target
		value.Target = &target
	}
	if value.TokenBudget != nil {
		budget := *value.TokenBudget
		value.TokenBudget = &budget
	}
	return value
}

func cloneCodexWorkflowResult(value protocol.SessionWorkflowResult) protocol.SessionWorkflowResult {
	value.Goal = cloneCodexGoal(value.Goal)
	return value
}

func validCodexGoal(value *protocol.SessionGoal, id string) bool {
	if value == nil || value.ThreadID != id || !workflowText(value.Objective, 64000, true) ||
		value.TokensUsed < 0 || value.TokensUsed > maxContextTokenValue || value.TimeUsedSeconds < 0 || value.TimeUsedSeconds > maxContextTokenValue ||
		value.CreatedAt < 0 || value.UpdatedAt < value.CreatedAt || value.UpdatedAt > maxContextTokenValue ||
		value.TokenBudget != nil && (*value.TokenBudget <= 0 || *value.TokenBudget > maxContextTokenValue) {
		return false
	}
	switch value.Status {
	case "active", "paused", "blocked", "usageLimited", "budgetLimited", "complete":
		return true
	}
	return false
}

func decodeCodexGoal(raw json.RawMessage, id string) (*protocol.SessionGoal, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, false
	}
	value, exists := fields["goal"]
	if !exists {
		return nil, false
	}
	if strings.TrimSpace(string(value)) == "null" {
		return nil, true
	}
	var goalFields map[string]json.RawMessage
	if json.Unmarshal(value, &goalFields) != nil {
		return nil, false
	}
	for _, key := range []string{"threadId", "objective", "status", "tokenBudget", "tokensUsed", "timeUsedSeconds", "createdAt", "updatedAt"} {
		if _, ok := goalFields[key]; !ok {
			return nil, false
		}
	}
	var goal protocol.SessionGoal
	if json.Unmarshal(value, &goal) != nil || !validCodexGoal(&goal, id) {
		return nil, false
	}
	return &goal, true
}

func (t *codexThread) codexWorkflowBusyLocked() bool {
	op := t.workflows.busy
	return op != nil && !op.terminal && op.conn == t.workflows.conn
}

func (t *codexThread) codexGoalActiveLocked() bool {
	return t.workflows.conn != nil && t.workflows.goal != nil && t.workflows.goal.Status == "active"
}

func (t *codexThread) codexWorkflowBlocksMutationLocked() bool {
	return t.codexWorkflowBusyLocked() || t.codexGoalActiveLocked()
}

func (a *codexAgent) retireCodexWorkflowsLocked(t *codexThread) {
	if op := t.workflows.busy; op != nil && !op.terminal {
		op.result.State, op.result.TurnID, op.result.Goal = "unconfirmed", "", nil
		op.result.Error = "运行连接已改变，无法确认原生工作流；不会重发"
		op.terminal, op.pending = true, nil
		op.conn.releaseSessionActionObserver(op.observerID, true)
	}
	t.workflows.conn, t.workflows.busy, t.workflows.goal = nil, nil, nil
	t.workflows.goalKnown = false
	t.workflows.goalVersion++
	t.workflows.fingerprint, t.workflows.revision = "", config.NewUUID()
}

func (a *codexAgent) syncCodexWorkflowsLocked(t *codexThread) {
	if t.workflows.conn != a.conn {
		a.retireCodexWorkflowsLocked(t)
		t.workflows.conn = a.conn
		t.workflows.goalUnsupported, t.workflows.reviewUnsupported = false, false
	}
	if t.workflows.revision == "" {
		t.workflows.revision = config.NewUUID()
	}
}

func codexGoalIdentity(goal *protocol.SessionGoal) any {
	if goal == nil {
		return nil
	}
	// Progress counters can change while a user pauses a running goal. CAS
	// binds its identity/configuration, not the continuously advancing meter.
	return []any{goal.ThreadID, goal.Objective, goal.Status, goal.TokenBudget, goal.CreatedAt}
}

func (a *codexAgent) codexWorkflowsLocked(t *codexThread) protocol.SessionWorkflows {
	a.syncCodexWorkflowsLocked(t)
	a.syncCodexContextLocked(t)
	state := &t.workflows
	epoch, supported := a.conn.sessionActionRuntime()
	idle := t.turnID == "" && t.info.ActiveTurnID == "" && t.info.Status == "idle" && t.info.Controllable
	childrenBusy := t.childOverflow
	for _, status := range t.childStates {
		switch status {
		case "completed", "interrupted", "errored", "failed", "shutdown", "notFound":
		default:
			childrenBusy = true
		}
	}
	approvals := a.aps.countFor(t.info.SessionKey)
	fingerprint := sessionActionHash([]any{epoch, state.change, state.goalKnown, codexGoalIdentity(state.goal), state.goalUnsupported, state.reviewUnsupported,
		a.owned[t.id], t.loaded, t.sessionActions.nativeStatus, t.info.Archived, t.info.Cwd, t.info.Model, t.info.Status, t.turnID, t.info.ActiveTurnID, t.turnVersion,
		t.permissionUnconfirmed, t.info.PermissionMode, t.statusUnconfirmed, childrenBusy, approvals,
		t.codexSessionActionBusyLocked(), t.codexCompactionBusyLocked(), t.codexWorkflowBusyLocked()})
	if state.fingerprint != fingerprint {
		state.fingerprint, state.revision = fingerprint, config.NewUUID()
	}
	out := protocol.SessionWorkflows{SessionKey: t.info.SessionKey, Revision: state.revision, GoalActions: []string{}}
	if supported && state.goalKnown && !state.goalUnsupported {
		out.GoalSupported, out.Goal = true, cloneCodexGoal(state.goal)
	}
	switch {
	case a.closed || a.conn == nil || !a.conn.alive():
		out.UnavailableReason = "运行连接尚未就绪"
	case !supported:
		out.UnavailableReason = "当前 Codex 版本不支持"
	case !a.owned[t.id] || !t.loaded:
		out.UnavailableReason = "会话未在当前桥接连接中加载"
	case t.info.Archived:
		out.UnavailableReason = "会话已归档"
	case t.sessionActions.nativeStatus != "idle" && t.sessionActions.nativeStatus != "active":
		out.UnavailableReason = "原生会话尚未加载或状态未确认"
	case state.goalUnsupported || !state.goalKnown:
		out.UnavailableReason = "无法确认原生目标状态"
	case t.statusUnconfirmed || t.permissionUnconfirmed || !validCodexPermissionMode(t.info.PermissionMode):
		out.UnavailableReason = "运行权限或状态尚未确认"
	case t.codexSessionActionBusyLocked() || t.codexCompactionBusyLocked() || t.codexWorkflowBusyLocked():
		out.UnavailableReason = "原生操作尚未结束或确认"
	case t.codexGoalActiveLocked():
		out.GoalActions = append(out.GoalActions, "pause")
		if idle && !childrenBusy && approvals == 0 {
			out.GoalActions = append(out.GoalActions, "clear")
		}
		out.UnavailableReason = "目标仍可继续运行，可先暂停目标"
	case !idle || childrenBusy || approvals != 0:
		out.UnavailableReason = "会话正忙"
	default:
		out.CanReview = !state.reviewUnsupported
		out.GoalActions = append(out.GoalActions, "set")
		if state.goal != nil {
			out.GoalActions = append(out.GoalActions, "resume", "clear")
		}
	}
	return out
}

func (a *codexAgent) workflowGuard(ctx context.Context, c *rpcConn, s Settings, validate func() error) error {
	if err := validateCodexContextCall(ctx, validate); err != nil {
		return err
	}
	a.mu.Lock()
	current := !a.closed && a.conn == c && a.connSig == codexSig(s)
	a.mu.Unlock()
	if !current {
		return errCodexWorkflowStale
	}
	if _, supported := c.sessionActionRuntime(); !supported {
		return errCodexWorkflowUnsupported
	}
	return nil
}

func (a *codexAgent) refreshCodexWorkflows(ctx context.Context, c *rpcConn, s Settings, t *codexThread, validate func() error) (protocol.SessionWorkflows, error) {
	var empty protocol.SessionWorkflows
	if _, err := a.refreshSessionActions(ctx, c, s, t, validate); err != nil {
		return empty, err
	}
	a.mu.Lock()
	a.syncCodexWorkflowsLocked(t)
	version, turnVersion := t.workflows.goalVersion, t.turnVersion
	unsupported := t.workflows.goalUnsupported
	a.mu.Unlock()
	if !unsupported {
		callCtx, cancel := context.WithTimeout(ctx, codexWorkflowTimeout)
		defer cancel()
		var raw json.RawMessage
		guard := func() error { return a.workflowGuard(callCtx, c, s, validate) }
		err := c.callGuarded(callCtx, "thread/goal/get", map[string]any{"threadId": t.id}, &raw, guard)
		goal, valid := decodeCodexGoal(raw, t.id)
		if err != nil && !featureMethodUnavailable(err) {
			return empty, errCodexWorkflowUnavailable
		}
		if err == nil && !valid {
			return empty, errCodexWorkflowUnavailable
		}
		if err := a.workflowGuard(ctx, c, s, validate); err != nil {
			return empty, err
		}
		a.mu.Lock()
		if a.conn != c || a.threads[t.id] != t || t.workflows.conn != c || t.workflows.goalVersion != version || t.turnVersion != turnVersion {
			a.mu.Unlock()
			return empty, errCodexWorkflowStale
		}
		t.workflows.goalUnsupported, t.workflows.goalKnown = err != nil, err == nil
		if err == nil {
			t.workflows.goal = goal
		}
		a.mu.Unlock()
	}
	a.mu.Lock()
	out := a.codexWorkflowsLocked(t)
	a.mu.Unlock()
	return out, nil
}

func (a *codexAgent) SessionWorkflows(ctx context.Context, id string, validate func() error) (protocol.SessionWorkflows, error) {
	var empty protocol.SessionWorkflows
	if !validChildID(id) {
		return empty, errCodexWorkflowStale
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
	t := a.threadLocked(id)
	out := a.codexWorkflowsLocked(t)
	owned := a.owned[id]
	a.mu.Unlock()
	if _, supported := c.sessionActionRuntime(); !supported || !owned {
		if err := validateCodexContextCall(ctx, validate); err != nil {
			return empty, err
		}
		a.mu.Lock()
		current := !a.closed && a.conn == c && a.connSig == codexSig(s)
		a.mu.Unlock()
		if !current {
			return empty, errCodexWorkflowStale
		}
		return out, nil
	}
	t.controlMu.Lock()
	defer t.controlMu.Unlock()
	if err := a.workflowGuard(ctx, c, s, validate); err != nil {
		return empty, err
	}
	a.mu.Lock()
	a.pollCodexWorkflowLocked(t, t.workflows.busy)
	a.mu.Unlock()
	out, err = a.refreshCodexWorkflows(ctx, c, s, t, validate)
	if err != nil {
		return empty, err
	}
	if err := a.workflowGuard(ctx, c, s, validate); err != nil {
		return empty, err
	}
	return out, nil
}

func workflowAvailable(view protocol.SessionWorkflows, kind string) bool {
	if kind == "review" {
		return view.CanReview
	}
	return hasSessionAction(view.GoalActions, strings.TrimPrefix(kind, "goal."))
}

func (a *codexAgent) ApplySessionWorkflow(ctx context.Context, id string, request protocol.SessionWorkflowRequest, validate func() error) (protocol.SessionWorkflowResult, error) {
	var empty protocol.SessionWorkflowResult
	request = cloneCodexWorkflowRequest(request)
	if !validChildID(id) || !ValidSessionWorkflow(request) {
		return empty, errors.New("session_workflow_invalid: 工作流参数无效")
	}
	if err := validateCodexContextCall(ctx, validate); err != nil {
		return empty, err
	}
	a.mu.Lock()
	t := a.threads[id]
	a.mu.Unlock()
	if t == nil {
		return empty, errCodexWorkflowStale
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
	signature, requestHash := codexSig(s), sessionActionHash(request)
	a.mu.Lock()
	old := t.workflows.receipts[request.OperationID]
	if old != nil {
		if old.requestHash != requestHash {
			a.mu.Unlock()
			return empty, errors.New("session_workflow_conflict: 操作标识已用于不同内容")
		}
		if old.signature == signature && a.connSig == signature {
			a.pollCodexWorkflowLocked(t, old)
		}
		result, resultErr := cloneCodexWorkflowResult(old.result), old.err
		current := old.conn == a.conn && a.connSig == signature && old.signature == signature && old.conn.alive()
		a.mu.Unlock()
		if err := validateCodexContextCall(ctx, validate); err != nil {
			return empty, err
		}
		finalSig := codexSig(settingsForContext(ctx, a.settings))
		a.mu.Lock()
		current = current && !a.closed && old.conn == a.conn && old.conn.alive() && a.connSig == finalSig && old.signature == finalSig
		a.mu.Unlock()
		if !current {
			result.State, result.TurnID, result.Goal, result.Error = "unconfirmed", "", nil, "运行连接已改变，原操作不会重发"
		}
		return result, resultErr
	}
	c := a.conn
	a.mu.Unlock()
	if err := a.workflowGuard(ctx, c, s, validate); err != nil {
		return empty, err
	}
	view, err := a.refreshCodexWorkflows(ctx, c, s, t, validate)
	if err != nil {
		return empty, err
	}
	if view.Revision != request.ExpectedRevision || !workflowAvailable(view, request.Kind) {
		return empty, errCodexWorkflowStale
	}
	a.mu.Lock()
	cwd := t.info.Cwd
	a.mu.Unlock()
	if err := validateCodexEffectiveProviderAt(ctx, c, s, cwd); err != nil {
		return empty, err
	}
	method := "thread/goal/set"
	params := map[string]any{"threadId": id}
	switch request.Kind {
	case "review":
		method = "review/start"
		target := map[string]any{"type": request.Target.Type}
		switch request.Target.Type {
		case "baseBranch":
			target["branch"] = request.Target.Branch
		case "commit":
			target["sha"], target["title"] = request.Target.SHA, nil
		case "custom":
			target["instructions"] = request.Target.Instructions
		}
		params["target"], params["delivery"] = target, "inline"
	case "goal.clear":
		method = "thread/goal/clear"
	case "goal.pause":
		params["status"] = "paused"
	case "goal.resume":
		params["status"] = "active"
	case "goal.set":
		params["status"], params["objective"] = "active", request.Objective
		if request.TokenBudget != nil {
			params["tokenBudget"] = *request.TokenBudget
		}
	}
	var op *codexWorkflowReceipt
	callCtx, cancel := context.WithTimeout(ctx, codexWorkflowTimeout)
	defer cancel()
	reply, answered, err := c.callSessionActionGuarded(callCtx, method, params, func(observerID int64, pending <-chan rpcResult) error {
		if err := a.workflowGuard(callCtx, c, s, validate); err != nil {
			return err
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		current := a.codexWorkflowsLocked(t)
		if a.conn != c || a.threads[id] != t || current.Revision != request.ExpectedRevision || !workflowAvailable(current, request.Kind) {
			return errCodexWorkflowStale
		}
		if len(t.workflows.receipts) >= maxCodexWorkflowReceipts {
			return errors.New("session_workflow_limit: 当前会话的工作流记录已达上限")
		}
		op = &codexWorkflowReceipt{conn: c, signature: signature, request: request, requestHash: requestHash, pending: pending, observerID: observerID,
			baseTurnVersion: t.turnVersion, baseGoalVersion: t.workflows.goalVersion,
			baseGoal: cloneCodexGoal(t.workflows.goal),
			result: protocol.SessionWorkflowResult{SessionKey: t.info.SessionKey, OperationID: request.OperationID, Kind: request.Kind,
				State: "unconfirmed", Error: "原生工作流已发出，尚未确认结果；不会重发"}}
		if t.workflows.receipts == nil {
			t.workflows.receipts = map[string]*codexWorkflowReceipt{}
		}
		t.workflows.receipts[request.OperationID], t.workflows.busy = op, op
		t.workflows.change = config.NewUUID()
		t.historyReadVersion++
		return nil
	})
	if op == nil {
		return empty, err
	}
	a.mu.Lock()
	if answered {
		op.pending = nil
	}
	info := a.finishCodexWorkflowLocked(t, op, reply, err)
	result, resultErr := cloneCodexWorkflowResult(op.result), op.err
	a.mu.Unlock()
	if err := validateCodexContextCall(ctx, validate); err != nil {
		return empty, err
	}
	a.mu.Lock()
	current := !a.closed && a.conn == c && c.alive() && a.connSig == signature
	a.mu.Unlock()
	if !current {
		result.State, result.TurnID, result.Goal, result.Error = "unconfirmed", "", nil, "运行连接已改变，原操作不会重发"
		return result, resultErr
	}
	if info != nil {
		a.emit(protocol.Event{SessionKey: info.SessionKey, Type: "session.updated", Session: info})
	}
	a.emit(protocol.Event{SessionKey: result.SessionKey, Type: "workflows.changed"})
	return result, resultErr
}

func (a *codexAgent) pollCodexWorkflowLocked(t *codexThread, op *codexWorkflowReceipt) {
	if op == nil || op.terminal || op.pending == nil || op.conn != a.conn || !op.conn.alive() || t.workflows.conn != a.conn {
		return
	}
	select {
	case reply := <-op.pending:
		op.conn.releaseSessionActionObserver(op.observerID, false)
		op.pending = nil
		a.finishCodexWorkflowLocked(t, op, reply.result, reply.err)
	default:
	}
}

func (a *codexAgent) finishCodexWorkflowLocked(t *codexThread, op *codexWorkflowReceipt, raw json.RawMessage, err error) *protocol.SessionInfo {
	c := op.conn
	if a.closed || a.conn != c || !c.alive() || a.threads[t.id] != t || t.workflows.conn != c {
		op.result.State, op.result.TurnID, op.result.Goal = "unconfirmed", "", nil
		op.result.Error = "运行连接已改变，无法确认原生工作流；不会重发"
		op.terminal, op.pending = true, nil
		c.releaseSessionActionObserver(op.observerID, true)
		return nil
	}
	var rejection *rpcError
	if errors.As(err, &rejection) {
		op.err = errors.New("session_workflow_rejected: 原生运行时拒绝了工作流")
		op.terminal, t.workflows.busy = true, nil
		if rejection.Code == -32601 {
			op.err = errCodexWorkflowUnsupported
			if op.request.Kind == "review" {
				t.workflows.reviewUnsupported = true
			} else {
				t.workflows.goalUnsupported = true
			}
		}
		t.workflows.change = config.NewUUID()
		return nil
	}
	if err != nil {
		return nil
	}
	var info *protocol.SessionInfo
	if op.request.Kind == "review" {
		var reply struct {
			Turn           cxTurn `json:"turn"`
			ReviewThreadID string `json:"reviewThreadId"`
		}
		if json.Unmarshal(raw, &reply) != nil || reply.ReviewThreadID != t.id || !ValidTurnID(reply.Turn.ID) {
			return nil
		}
		switch reply.Turn.Status {
		case "inProgress", "completed", "failed", "interrupted":
		default:
			return nil
		}
		op.result.State, op.result.TurnID = "requested", reply.Turn.ID
		if t.turnVersion == op.baseTurnVersion && t.turnID == "" && t.info.ActiveTurnID == "" {
			t.turnVersion++
			if reply.Turn.Status == "inProgress" {
				t.turnID, t.info.ActiveTurnID, t.info.Status = reply.Turn.ID, reply.Turn.ID, "running"
			} else {
				t.info.LastTerminalTurnID = reply.Turn.ID
			}
			t.info.UpdatedAt = nowMs()
			value := t.info
			info = &value
		}
	} else if op.request.Kind == "goal.clear" {
		var reply struct {
			Cleared *bool `json:"cleared"`
		}
		if json.Unmarshal(raw, &reply) != nil || reply.Cleared == nil {
			return nil
		}
		if !*reply.Cleared {
			// This is a real terminal acknowledgement of no deletion. Do not
			// claim completion, but no delayed clear can collide with a new turn.
			op.terminal, t.workflows.busy = true, nil
			op.result.Error = "原生运行时未清除目标，请刷新核对；此操作不会重发"
			t.workflows.change = config.NewUUID()
			return nil
		}
		op.result.State, op.result.Goal = "completed", nil
		if t.workflows.goalVersion == op.baseGoalVersion {
			t.workflows.goal, t.workflows.goalKnown = nil, true
			t.workflows.goalVersion++
		}
	} else {
		goal, valid := decodeCodexGoal(raw, t.id)
		if !valid || goal == nil {
			return nil
		}
		status := "active"
		if op.request.Kind == "goal.pause" {
			status = "paused"
		}
		if goal.Status != status || op.request.Kind == "goal.set" && (goal.Objective != op.request.Objective ||
			op.request.TokenBudget != nil && (goal.TokenBudget == nil || *goal.TokenBudget != *op.request.TokenBudget)) {
			return nil
		}
		if op.request.Kind != "goal.set" && (op.baseGoal == nil || goal.Objective != op.baseGoal.Objective || goal.CreatedAt != op.baseGoal.CreatedAt || sessionActionHash(goal.TokenBudget) != sessionActionHash(op.baseGoal.TokenBudget)) {
			return nil
		}
		op.result.State, op.result.Goal = "completed", cloneCodexGoal(goal)
		if t.workflows.goalVersion == op.baseGoalVersion {
			t.workflows.goal, t.workflows.goalKnown = goal, true
			t.workflows.goalVersion++
		}
	}
	op.result.Error, op.terminal, t.workflows.busy = "", true, nil
	t.workflows.change = config.NewUUID()
	return info
}

func (a *codexAgent) onCodexWorkflowNotify(c *rpcConn, method string, params json.RawMessage) {
	if method != "thread/goal/updated" && method != "thread/goal/cleared" {
		return
	}
	var note struct {
		ThreadID string                `json:"threadId"`
		Goal     *protocol.SessionGoal `json:"goal"`
	}
	if json.Unmarshal(params, &note) != nil || !validChildID(note.ThreadID) {
		return
	}
	if method == "thread/goal/updated" {
		goal, valid := decodeCodexGoal(params, note.ThreadID)
		if !valid || goal == nil {
			return
		}
		note.Goal = goal
	}
	a.mu.Lock()
	t := a.threads[note.ThreadID]
	_, supported := c.sessionActionRuntime()
	if a.closed || a.conn != c || !supported || t == nil || !a.owned[note.ThreadID] {
		a.mu.Unlock()
		return
	}
	a.syncCodexWorkflowsLocked(t)
	if method == "thread/goal/updated" && t.workflows.goal != nil && note.Goal.CreatedAt == t.workflows.goal.CreatedAt && note.Goal.UpdatedAt < t.workflows.goal.UpdatedAt {
		a.mu.Unlock()
		return
	}
	t.workflows.goal = cloneCodexGoal(note.Goal)
	if method == "thread/goal/cleared" {
		t.workflows.goal = nil
	}
	t.workflows.goalKnown = true
	t.workflows.goalVersion++
	a.mu.Unlock()
	a.emit(protocol.Event{SessionKey: "codex:" + note.ThreadID, Type: "workflows.changed"})
}

var _ SessionWorkflowProvider = (*codexAgent)(nil)
