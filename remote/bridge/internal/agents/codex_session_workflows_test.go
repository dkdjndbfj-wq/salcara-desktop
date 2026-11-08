package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"salcara/bridge/internal/protocol"
)

type codexWorkflowTransport struct {
	mu             sync.Mutex
	base           *codexSessionActionTransport
	goal           *protocol.SessionGoal
	requests       []map[string]any
	hook           func(string)
	hold           string
	delayed        []byte
	overrideMethod string
	override       json.RawMessage
	reject         string
}

func (f *codexWorkflowTransport) Close() error { return nil }
func (f *codexWorkflowTransport) Write(raw []byte) (int, error) {
	var request map[string]any
	if err := json.Unmarshal(raw, &request); err != nil {
		return 0, err
	}
	method, _ := request["method"].(string)
	if method != "review/start" && method != "thread/goal/get" && method != "thread/goal/set" && method != "thread/goal/clear" && method != "turn/interrupt" {
		return f.base.Write(raw)
	}
	f.mu.Lock()
	f.requests = append(f.requests, request)
	hook := f.hook
	f.mu.Unlock()
	if hook != nil {
		hook(method)
	}
	f.mu.Lock()
	params, _ := request["params"].(map[string]any)
	id, _ := params["threadId"].(string)
	response := map[string]any{"id": request["id"]}
	if method == f.reject {
		response["error"] = map[string]any{"code": -32601, "message": "fixture method unavailable"}
	} else {
		switch method {
		case "thread/goal/get":
			response["result"] = map[string]any{"goal": cloneCodexGoal(f.goal)}
		case "thread/goal/set":
			if f.goal == nil {
				f.goal = &protocol.SessionGoal{ThreadID: id, CreatedAt: 100, UpdatedAt: 100}
			}
			if objective, ok := params["objective"].(string); ok {
				f.goal.Objective = objective
			}
			if status, ok := params["status"].(string); ok {
				f.goal.Status = status
			}
			if budget, ok := params["tokenBudget"].(float64); ok {
				value := int64(budget)
				f.goal.TokenBudget = &value
			}
			f.goal.UpdatedAt++
			response["result"] = map[string]any{"goal": cloneCodexGoal(f.goal)}
		case "thread/goal/clear":
			cleared := f.goal != nil
			f.goal = nil
			response["result"] = map[string]any{"cleared": cleared}
		case "review/start":
			response["result"] = map[string]any{"reviewThreadId": id, "turn": cxTurn{ID: "native-review-turn", Status: "inProgress"}}
		case "turn/interrupt":
			response["result"] = map[string]any{}
		}
	}
	if method == f.overrideMethod {
		response["result"] = f.override
	}
	encoded, err := json.Marshal(response)
	if method == f.hold {
		f.delayed = append([]byte(nil), encoded...)
		f.mu.Unlock()
		return len(raw), err
	}
	f.mu.Unlock()
	if err != nil {
		return 0, err
	}
	f.base.conn.handleLine(encoded)
	return len(raw), nil
}

func (f *codexWorkflowTransport) calls(method string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []map[string]any{}
	for _, request := range f.requests {
		if request["method"] == method {
			out = append(out, request["params"].(map[string]any))
		}
	}
	return out
}

func newCodexWorkflowFixture(t *testing.T) (*codexAgent, *codexWorkflowTransport, *recorder) {
	t.Helper()
	a, base, rec := newCodexSessionActionFixture(t)
	f := &codexWorkflowTransport{base: base}
	a.conn.stdin = f
	a.mu.Lock()
	a.owned[sessionActionThread] = true
	a.threads[sessionActionThread].info.Model = "native-model"
	a.mu.Unlock()
	a.installCodexNotificationHandler(a.conn)
	return a, f, rec
}

func workflowFixtureGoal(status string) *protocol.SessionGoal {
	budget := int64(7000)
	return &protocol.SessionGoal{ThreadID: sessionActionThread, Objective: "Existing goal", Status: status, TokenBudget: &budget, TokensUsed: 100, TimeUsedSeconds: 5, CreatedAt: 100, UpdatedAt: 105}
}

func workflowView(t *testing.T, a *codexAgent) protocol.SessionWorkflows {
	t.Helper()
	view, err := a.SessionWorkflows(context.Background(), sessionActionThread, nil)
	if err != nil {
		t.Fatal(err)
	}
	return view
}

func workflowRequest(kind, revision string, index int) protocol.SessionWorkflowRequest {
	request := protocol.SessionWorkflowRequest{Kind: kind, ExpectedRevision: revision, OperationID: fmt.Sprintf("61000000-0000-4000-8000-%012x", index)}
	if kind == "review" {
		request.Target = &protocol.SessionReviewTarget{Type: "uncommittedChanges"}
	}
	if kind == "goal.set" {
		request.Objective = "Implement the requested change"
	}
	return request
}

func applyWorkflow(t *testing.T, a *codexAgent, request protocol.SessionWorkflowRequest) protocol.SessionWorkflowResult {
	t.Helper()
	out, err := a.ApplySessionWorkflow(context.Background(), sessionActionThread, request, nil)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCodexWorkflowsReviewUsesNativeInlineTargetAndTurn(t *testing.T) {
	for _, target := range []protocol.SessionReviewTarget{{Type: "uncommittedChanges"}, {Type: "baseBranch", Branch: "main"}, {Type: "commit", SHA: "abcdef0123456789"}, {Type: "custom", Instructions: "Review race safety"}} {
		t.Run(target.Type, func(t *testing.T) {
			a, f, _ := newCodexWorkflowFixture(t)
			view := workflowView(t, a)
			if !view.CanReview || !view.GoalSupported || view.Goal != nil {
				t.Fatal(view)
			}
			request := workflowRequest("review", view.Revision, 1)
			request.Target = &target
			out := applyWorkflow(t, a, request)
			if out.State != "requested" || out.TurnID != "native-review-turn" || out.Goal != nil {
				t.Fatal(out)
			}
			params := f.calls("review/start")
			if len(params) != 1 || params[0]["delivery"] != "inline" || params[0]["threadId"] != sessionActionThread || len(params[0]) != 3 {
				t.Fatal(params)
			}
			native := params[0]["target"].(map[string]any)
			if native["type"] != target.Type {
				t.Fatal(native)
			}
			if target.Type == "commit" {
				if value, ok := native["title"]; !ok || value != nil {
					t.Fatal(native)
				}
			}
			copy := cloneCodexWorkflowRequest(request)
			if again := applyWorkflow(t, a, copy); again.State != "requested" || len(f.calls("review/start")) != 1 {
				t.Fatal(again)
			}
			a.mu.Lock()
			active := a.threads[sessionActionThread].info.ActiveTurnID
			a.mu.Unlock()
			if active != out.TurnID {
				t.Fatal(active)
			}
			assertNoSessionModelCalls(t, f.base)
		})
	}
}

func TestCodexWorkflowsGoalSetPauseResumeClearAndBudgetOmission(t *testing.T) {
	for _, explicitBudget := range []bool{false, true} {
		t.Run(fmt.Sprint(explicitBudget), func(t *testing.T) {
			a, f, _ := newCodexWorkflowFixture(t)
			f.goal = workflowFixtureGoal("paused")
			view := workflowView(t, a)
			request := workflowRequest("goal.set", view.Revision, 1)
			if explicitBudget {
				budget := int64(9000)
				request.TokenBudget = &budget
			}
			out := applyWorkflow(t, a, request)
			if out.State != "completed" || out.Goal == nil || out.Goal.ThreadID != sessionActionThread || out.Goal.Status != "active" || out.Goal.Objective != request.Objective || out.TurnID != "" {
				t.Fatal(out)
			}
			params := f.calls("thread/goal/set")[0]
			if _, present := params["tokenBudget"]; present != explicitBudget {
				t.Fatalf("budget omission changed: %+v", params)
			}
			if !explicitBudget && *out.Goal.TokenBudget != 7000 {
				t.Fatal(out.Goal)
			}
			view = workflowView(t, a)
			if view.CanReview || !hasSessionAction(view.GoalActions, "pause") || !hasSessionAction(view.GoalActions, "clear") {
				t.Fatal(view)
			}
			if err := a.Send(context.Background(), sessionActionThread, "must not race scheduled continuation"); !errors.Is(err, errCodexWorkflowBusy) {
				t.Fatal(err)
			}
			contextView, err := a.SessionContext(context.Background(), sessionActionThread, nil)
			if err != nil || contextView.CanCompact || contextView.UnavailableReason != "session_workflow_busy" {
				t.Fatalf("context: %+v %v", contextView, err)
			}
			if actions := readSessionActionFixture(t, a); len(actions.Actions) != 0 {
				t.Fatal(actions)
			}
			for index, kind := range []string{"goal.pause", "goal.resume", "goal.pause", "goal.clear"} {
				view = workflowView(t, a)
				out = applyWorkflow(t, a, workflowRequest(kind, view.Revision, index+2))
				if out.State != "completed" {
					t.Fatal(out)
				}
				if kind == "goal.clear" {
					if out.Goal != nil {
						t.Fatal(out)
					}
					continue
				}
				calls := f.calls("thread/goal/set")
				params = calls[len(calls)-1]
				if len(params) != 2 || params["threadId"] != sessionActionThread {
					t.Fatalf("status patch changed other fields: %+v", params)
				}
			}
			view = workflowView(t, a)
			if !view.CanReview || view.Goal != nil || !reflect.DeepEqual(view.GoalActions, []string{"set"}) {
				t.Fatal(view)
			}
			assertNoSessionModelCalls(t, f.base)
		})
	}
}

func TestCodexWorkflowsPauseDoesNotInterruptCurrentTurn(t *testing.T) {
	a, f, _ := newCodexWorkflowFixture(t)
	f.goal = workflowFixtureGoal("active")
	emitCodexContextNote(f.base.conn, "turn/started", map[string]any{"threadId": sessionActionThread, "turn": cxTurn{ID: "active-goal-turn", Status: "inProgress"}})
	view := workflowView(t, a)
	if view.CanReview || !reflect.DeepEqual(view.GoalActions, []string{"pause"}) {
		t.Fatal(view)
	}
	out := applyWorkflow(t, a, workflowRequest("goal.pause", view.Revision, 1))
	if out.State != "completed" || out.Goal == nil || out.Goal.Status != "paused" || len(f.calls("turn/interrupt")) != 0 {
		t.Fatal(out)
	}
	a.mu.Lock()
	active := a.threads[sessionActionThread].turnID
	a.mu.Unlock()
	if active != "active-goal-turn" {
		t.Fatal("pause retired the current turn")
	}
	stop, err := a.InterruptTurn(context.Background(), sessionActionThread, active, "71000000-0000-4000-8000-000000000001")
	if err != nil || stop.Status != "requested" || len(f.calls("turn/interrupt")) != 1 {
		t.Fatalf("precise interrupt stopped working: %+v %v", stop, err)
	}
	assertNoSessionModelCalls(t, f.base)
}

func TestCodexWorkflowsCASOwnershipBusyAndPermissionExclusion(t *testing.T) {
	for _, kind := range []string{"unowned", "unloaded", "permission", "approval", "child", "context", "management", "native-goal", "native-active", "native-notLoaded", "native-unknown", "local-turn"} {
		t.Run(kind, func(t *testing.T) {
			a, f, _ := newCodexWorkflowFixture(t)
			view := workflowView(t, a)
			a.mu.Lock()
			thread := a.threads[sessionActionThread]
			switch kind {
			case "unowned":
				delete(a.owned, sessionActionThread)
			case "unloaded":
				thread.loaded = false
			case "permission":
				thread.permissionUnconfirmed = true
			case "child":
				thread.childStates = map[string]string{"child": "running"}
			case "context":
				thread.context.compaction = &codexCompaction{conn: a.conn}
			case "management":
				thread.sessionActions.busy = &codexSessionActionReceipt{conn: a.conn}
			case "local-turn":
				thread.turnID, thread.info.ActiveTurnID, thread.info.Status = "other", "other", "running"
			}
			a.mu.Unlock()
			if kind == "approval" {
				a.aps.addRequest("codex:"+sessionActionThread, "codex", "workflow-question", protocol.Event{Kind: "command"})
			}
			if kind == "native-goal" {
				f.goal = workflowFixtureGoal("active")
			}
			if kind == "native-active" || kind == "native-notLoaded" || kind == "native-unknown" {
				f.base.mu.Lock()
				th := f.base.threads[sessionActionThread]
				th.Status.Type = strings.TrimPrefix(kind, "native-")
				f.base.threads[sessionActionThread] = th
				f.base.mu.Unlock()
			}
			if _, err := a.ApplySessionWorkflow(context.Background(), sessionActionThread, workflowRequest("review", view.Revision, 1), nil); err == nil {
				t.Fatal("stale or busy state admitted review")
			}
			if len(f.calls("review/start")) != 0 {
				t.Fatal("review executed")
			}
			assertNoSessionModelCalls(t, f.base)
		})
	}
}

func TestCodexWorkflowsProgressMeterDoesNotInvalidatePause(t *testing.T) {
	a, f, _ := newCodexWorkflowFixture(t)
	f.goal = workflowFixtureGoal("active")
	view := workflowView(t, a)
	f.goal.TokensUsed += 500
	f.goal.TimeUsedSeconds += 20
	f.goal.UpdatedAt++
	emitCodexContextNote(f.base.conn, "thread/goal/updated", map[string]any{"threadId": sessionActionThread, "goal": f.goal})
	out := applyWorkflow(t, a, workflowRequest("goal.pause", view.Revision, 1))
	if out.State != "completed" || out.Goal == nil || out.Goal.Status != "paused" {
		t.Fatal(out)
	}
}

func TestCodexWorkflowsLostResultNeverReplaysAndRealLateAckRecovers(t *testing.T) {
	for _, kind := range []string{"review", "goal.set"} {
		t.Run(kind, func(t *testing.T) {
			a, f, _ := newCodexWorkflowFixture(t)
			view := workflowView(t, a)
			request := workflowRequest(kind, view.Revision, 1)
			method := "review/start"
			if kind == "goal.set" {
				method = "thread/goal/set"
			}
			f.hold = method
			ctx, cancel := context.WithCancel(context.Background())
			f.hook = func(value string) {
				if value == method {
					cancel()
				}
			}
			if _, err := a.ApplySessionWorkflow(ctx, sessionActionThread, request, nil); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			out := applyWorkflow(t, a, request)
			if out.State != "unconfirmed" || out.TurnID != "" || out.Goal != nil || len(f.calls(method)) != 1 {
				t.Fatal(out)
			}
			view = workflowView(t, a)
			if view.CanReview || len(view.GoalActions) != 0 {
				t.Fatal("read released an unacknowledged workflow")
			}
			if err := a.Send(context.Background(), sessionActionThread, "must not start"); !errors.Is(err, errCodexWorkflowBusy) {
				t.Fatal(err)
			}
			f.mu.Lock()
			late := append([]byte(nil), f.delayed...)
			f.mu.Unlock()
			f.base.conn.handleLine(late)
			out = applyWorkflow(t, a, request)
			want := "requested"
			if kind == "goal.set" {
				want = "completed"
			}
			if out.State != want || len(f.calls(method)) != 1 {
				t.Fatal(out)
			}
			assertNoSessionModelCalls(t, f.base)
		})
	}
}

func TestCodexWorkflowsMalformedAcknowledgementsRemainUnconfirmed(t *testing.T) {
	for _, fixture := range []struct{ kind, raw string }{
		{"review", `null`}, {"review", `{"reviewThreadId":"wrong","turn":{"id":"turn","status":"inProgress"}}`},
		{"review", `{"reviewThreadId":"action-original","turn":{"id":"turn","status":"future"}}`},
		{"goal.set", `{"goal":null}`}, {"goal.set", `{"goal":{"threadId":"wrong","objective":"Implement the requested change","status":"active"}}`},
		{"goal.clear", `{"cleared":false}`}, {"goal.clear", `{}`},
	} {
		t.Run(fixture.kind+fixture.raw, func(t *testing.T) {
			a, f, _ := newCodexWorkflowFixture(t)
			if fixture.kind == "goal.clear" {
				f.goal = workflowFixtureGoal("paused")
			}
			view := workflowView(t, a)
			method := map[string]string{"review": "review/start", "goal.set": "thread/goal/set", "goal.clear": "thread/goal/clear"}[fixture.kind]
			f.overrideMethod, f.override = method, json.RawMessage(fixture.raw)
			request := workflowRequest(fixture.kind, view.Revision, 1)
			out := applyWorkflow(t, a, request)
			if out.State != "unconfirmed" || out.Goal != nil || out.TurnID != "" {
				t.Fatal(out)
			}
			if again := applyWorkflow(t, a, request); again.State != "unconfirmed" || len(f.calls(method)) != 1 {
				t.Fatal(again)
			}
		})
	}
}

func TestCodexWorkflowsSourceGuardAtWriteAndReturn(t *testing.T) {
	for _, boundary := range []string{"write", "return"} {
		t.Run(boundary, func(t *testing.T) {
			a, f, rec := newCodexWorkflowFixture(t)
			view := workflowView(t, a)
			request := workflowRequest("goal.set", view.Revision, 1)
			var deny atomic.Bool
			denied := errors.New("fixture source denied")
			guard := func() error {
				if deny.Load() {
					return denied
				}
				return nil
			}
			if boundary == "write" {
				f.base.hook = func(method string) {
					if method == "config/read" {
						deny.Store(true)
					}
				}
			} else {
				f.hook = func(method string) {
					if method == "thread/goal/set" {
						deny.Store(true)
					}
				}
			}
			out, err := a.ApplySessionWorkflow(context.Background(), sessionActionThread, request, guard)
			if !errors.Is(err, denied) || out.State != "" || out.Goal != nil {
				t.Fatalf("result escaped source guard: %+v %v", out, err)
			}
			if rec.count(func(event protocol.Event) bool {
				return event.Type == "workflows.changed" || event.Type == "session.updated"
			}) != 0 {
				t.Fatal("event escaped source guard")
			}
			want := 0
			if boundary == "return" {
				want = 1
			}
			if len(f.calls("thread/goal/set")) != want {
				t.Fatal("incorrect dispatch boundary")
			}
			if boundary == "return" {
				deny.Store(false)
				if again := applyWorkflow(t, a, request); again.State != "completed" || len(f.calls("thread/goal/set")) != 1 {
					t.Fatal(again)
				}
			}
		})
	}
}

func TestCodexWorkflowsNotificationsFenceOldReadsAndConnections(t *testing.T) {
	a, f, _ := newCodexWorkflowFixture(t)
	f.goal = workflowFixtureGoal("paused")
	workflowView(t, a)
	active := workflowFixtureGoal("active")
	f.hook = func(method string) {
		if method == "thread/goal/get" {
			emitCodexContextNote(f.base.conn, "thread/goal/updated", map[string]any{"threadId": sessionActionThread, "goal": active})
		}
	}
	if _, err := a.SessionWorkflows(context.Background(), sessionActionThread, nil); !errors.Is(err, errCodexWorkflowStale) {
		t.Fatal(err)
	}
	a.mu.Lock()
	observed := cloneCodexGoal(a.threads[sessionActionThread].workflows.goal)
	a.mu.Unlock()
	if observed == nil || observed.Status != "active" {
		t.Fatalf("stale read overwrote notification: %+v", observed)
	}
	old := f.base.conn
	next := &rpcConn{pending: map[int64]chan rpcResult{}, done: make(chan struct{}), stdin: f}
	next.recordSessionInitialization(codexSessionInitialize{UserAgent: sessionActionNativeUA})
	a.mu.Lock()
	a.conn = next
	a.markUnloadedLocked()
	a.mu.Unlock()
	emitCodexContextNote(old, "thread/goal/updated", map[string]any{"threadId": sessionActionThread, "goal": active})
	a.mu.Lock()
	observed = cloneCodexGoal(a.threads[sessionActionThread].workflows.goal)
	a.mu.Unlock()
	if observed != nil {
		t.Fatal("old connection revived goal metadata")
	}
}

func TestCodexWorkflowsVersionAndUnsupportedMethodsNarrowCapabilities(t *testing.T) {
	for _, reason := range []string{"version", "goal-get", "review"} {
		t.Run(reason, func(t *testing.T) {
			a, f, _ := newCodexWorkflowFixture(t)
			if reason == "version" {
				a.conn.recordSessionInitialization(codexSessionInitialize{UserAgent: "codex_cli_rs/0.144.0"})
			}
			if reason == "goal-get" {
				f.reject = "thread/goal/get"
			}
			view := workflowView(t, a)
			if reason != "review" {
				if view.CanReview || view.GoalSupported || len(view.GoalActions) != 0 {
					t.Fatal(view)
				}
				workflowView(t, a)
				want := 0
				if reason == "goal-get" {
					want = 1
				}
				if len(f.calls("thread/goal/get")) != want {
					t.Fatal("unsupported method retried")
				}
				return
			}
			f.reject = "review/start"
			request := workflowRequest("review", view.Revision, 1)
			if _, err := a.ApplySessionWorkflow(context.Background(), sessionActionThread, request, nil); !errors.Is(err, errCodexWorkflowUnsupported) {
				t.Fatal(err)
			}
			view = workflowView(t, a)
			if view.CanReview || !view.GoalSupported {
				t.Fatal(view)
			}
			if _, err := a.ApplySessionWorkflow(context.Background(), sessionActionThread, request, nil); !errors.Is(err, errCodexWorkflowUnsupported) || len(f.calls("review/start")) != 1 {
				t.Fatal(err)
			}
		})
	}
}

func TestCodexWorkflowsReceiptConflictAndCapacityNeverDispatch(t *testing.T) {
	a, f, _ := newCodexWorkflowFixture(t)
	view := workflowView(t, a)
	request := workflowRequest("goal.set", view.Revision, 1)
	applyWorkflow(t, a, request)
	request.Objective = "Another request"
	if _, err := a.ApplySessionWorkflow(context.Background(), sessionActionThread, request, nil); err == nil {
		t.Fatal("receipt ID accepted changed content")
	}
	if len(f.calls("thread/goal/set")) != 1 {
		t.Fatal("changed receipt dispatched")
	}
	b, g, _ := newCodexWorkflowFixture(t)
	view = workflowView(t, b)
	b.mu.Lock()
	thread := b.threads[sessionActionThread]
	thread.workflows.receipts = map[string]*codexWorkflowReceipt{}
	for i := 0; i < maxCodexWorkflowReceipts; i++ {
		thread.workflows.receipts[fmt.Sprint(i)] = &codexWorkflowReceipt{terminal: true}
	}
	b.mu.Unlock()
	if _, err := b.ApplySessionWorkflow(context.Background(), sessionActionThread, workflowRequest("review", view.Revision, 1), nil); err == nil || !strings.HasPrefix(err.Error(), "session_workflow_limit:") {
		t.Fatal(err)
	}
	if len(g.calls("review/start")) != 0 {
		t.Fatal("receipt capacity dispatched review")
	}
}

func TestCodexWorkflowsOldConnectionAndCachedReturnCannotProjectIntoSuccessor(t *testing.T) {
	for _, boundary := range []string{"native-ack", "receipt-return"} {
		t.Run(boundary, func(t *testing.T) {
			a, f, rec := newCodexWorkflowFixture(t)
			view := workflowView(t, a)
			request := workflowRequest("goal.set", view.Revision, 1)
			next := &rpcConn{pending: map[int64]chan rpcResult{}, done: make(chan struct{}), stdin: f}
			next.recordSessionInitialization(codexSessionInitialize{UserAgent: sessionActionNativeUA})
			replace := func() {
				a.mu.Lock()
				a.conn = next
				a.markUnloadedLocked()
				a.mu.Unlock()
			}
			var validate func() error
			if boundary == "native-ack" {
				f.hook = func(method string) {
					if method == "thread/goal/set" {
						replace()
					}
				}
			} else {
				applyWorkflow(t, a, request)
				var validations int
				validate = func() error {
					validations++
					if validations == 3 {
						replace()
					}
					return nil
				}
			}
			before := rec.count(func(event protocol.Event) bool { return event.Type == "workflows.changed" })
			out, err := a.ApplySessionWorkflow(context.Background(), sessionActionThread, request, validate)
			if err != nil || out.State != "unconfirmed" || out.Goal != nil || out.TurnID != "" || len(f.calls("thread/goal/set")) != 1 {
				t.Fatalf("old result escaped: %+v %v", out, err)
			}
			a.mu.Lock()
			goal := a.threads[sessionActionThread].workflows.goal
			a.mu.Unlock()
			if goal != nil || rec.count(func(event protocol.Event) bool { return event.Type == "workflows.changed" }) != before {
				t.Fatal("old reply changed successor goal or emitted a refresh event")
			}
		})
	}
}

func TestCodexWorkflowsLateReviewAckCannotReviveTerminalOrReplaceNextTurn(t *testing.T) {
	a, f, _ := newCodexWorkflowFixture(t)
	view := workflowView(t, a)
	f.hook = func(method string) {
		if method != "review/start" {
			return
		}
		for _, turn := range []cxTurn{{ID: "native-review-turn", Status: "inProgress"}, {ID: "native-review-turn", Status: "completed"}, {ID: "next-native-turn", Status: "inProgress"}} {
			method := "turn/started"
			if turn.Status != "inProgress" {
				method = "turn/completed"
			}
			emitCodexContextNote(f.base.conn, method, map[string]any{"threadId": sessionActionThread, "turn": turn})
		}
	}
	out := applyWorkflow(t, a, workflowRequest("review", view.Revision, 1))
	if out.State != "requested" || out.TurnID != "native-review-turn" {
		t.Fatal(out)
	}
	a.mu.Lock()
	thread := a.threads[sessionActionThread]
	active, terminal := thread.info.ActiveTurnID, thread.info.LastTerminalTurnID
	a.mu.Unlock()
	if active != "next-native-turn" || terminal != "native-review-turn" {
		t.Fatalf("late review ack replaced newer state: %q %q", active, terminal)
	}
}

func TestCodexWorkflowsGoalStatusPatchMustPreserveExactGoal(t *testing.T) {
	for _, mismatch := range []string{"objective", "budget", "thread", "status", "created", "missing-counter"} {
		t.Run(mismatch, func(t *testing.T) {
			a, f, _ := newCodexWorkflowFixture(t)
			f.goal = workflowFixtureGoal("active")
			view := workflowView(t, a)
			goal := workflowFixtureGoal("paused")
			switch mismatch {
			case "objective":
				goal.Objective = "Another goal"
			case "budget":
				value := int64(5000)
				goal.TokenBudget = &value
			case "thread":
				goal.ThreadID = "another-thread"
			case "status":
				goal.Status = "active"
			case "created":
				goal.CreatedAt++
			}
			raw, _ := json.Marshal(map[string]any{"goal": goal})
			if mismatch == "missing-counter" {
				var body map[string]any
				_ = json.Unmarshal(raw, &body)
				delete(body["goal"].(map[string]any), "tokensUsed")
				raw, _ = json.Marshal(body)
			}
			f.overrideMethod, f.override = "thread/goal/set", raw
			out := applyWorkflow(t, a, workflowRequest("goal.pause", view.Revision, 1))
			if out.State != "unconfirmed" || out.Goal != nil {
				t.Fatal(out)
			}
		})
	}
}

func TestCodexWorkflowsMalformedGoalNotificationCannotReleaseActiveFence(t *testing.T) {
	a, f, _ := newCodexWorkflowFixture(t)
	f.goal = workflowFixtureGoal("active")
	workflowView(t, a)
	emitCodexContextNote(f.base.conn, "thread/goal/updated", map[string]any{"threadId": sessionActionThread, "goal": map[string]any{"threadId": sessionActionThread, "objective": "Existing goal", "status": "paused"}})
	a.mu.Lock()
	active := a.threads[sessionActionThread].codexGoalActiveLocked()
	a.mu.Unlock()
	if !active {
		t.Fatal("malformed notification released goal scheduling fence")
	}
}

func TestCodexWorkflowsConcurrentLateReceiptConsumptionAndResultIsolation(t *testing.T) {
	a, f, _ := newCodexWorkflowFixture(t)
	view := workflowView(t, a)
	request := workflowRequest("goal.set", view.Revision, 1)
	budget := int64(9000)
	request.TokenBudget = &budget
	f.hold = "thread/goal/set"
	ctx, cancel := context.WithCancel(context.Background())
	f.hook = func(method string) {
		if method == "thread/goal/set" {
			cancel()
		}
	}
	if _, err := a.ApplySessionWorkflow(ctx, sessionActionThread, request, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	f.mu.Lock()
	late := append([]byte(nil), f.delayed...)
	f.mu.Unlock()
	f.base.conn.handleLine(late)
	const readers = 8
	results := make(chan protocol.SessionWorkflowResult, readers)
	failures := make(chan error, readers)
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := a.ApplySessionWorkflow(context.Background(), sessionActionThread, request, nil)
			results <- out
			failures <- err
		}()
	}
	wg.Wait()
	for i := 0; i < readers; i++ {
		out, err := <-results, <-failures
		if err != nil || out.State != "completed" || out.Goal == nil || *out.Goal.TokenBudget != budget {
			t.Fatalf("receipt: %+v %v", out, err)
		}
		out.Goal.Objective = "mutated by caller"
		*out.Goal.TokenBudget = 1
	}
	out := applyWorkflow(t, a, request)
	if out.Goal.Objective != request.Objective || *out.Goal.TokenBudget != budget || len(f.calls("thread/goal/set")) != 1 {
		t.Fatal(out)
	}
	f.base.conn.mu.Lock()
	observers := len(f.base.conn.sessionActionObservers)
	f.base.conn.mu.Unlock()
	if observers != 0 {
		t.Fatal("late workflow observer was not released")
	}
}
