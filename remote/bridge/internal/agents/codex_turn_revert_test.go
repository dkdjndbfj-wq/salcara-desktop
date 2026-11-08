package agents

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"salcara/bridge/internal/protocol"
)

type revertFixtureTransport struct {
	mu                       sync.Mutex
	conn                     *rpcConn
	thread                   cxThread
	requests                 []map[string]any
	goal                     any
	hold                     bool
	badAck                   bool
	noMutation               bool
	reject                   bool
	internalErrorAfterCommit bool
	hook                     func(string)
}

func (f *revertFixtureTransport) Close() error { return nil }
func (f *revertFixtureTransport) Write(raw []byte) (int, error) {
	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil {
		return 0, err
	}
	method := req["method"].(string)
	params, _ := req["params"].(map[string]any)
	f.mu.Lock()
	f.requests = append(f.requests, req)
	hook := f.hook
	f.mu.Unlock()
	if hook != nil {
		hook(method)
	}
	f.mu.Lock()
	reply := map[string]any{"id": req["id"]}
	switch method {
	case "config/read":
		cfg, _ := guardedProviderConfig(Settings{RelayRoot: "https://fixture.invalid"})
		reply["result"] = map[string]any{"config": cfg}
	case "thread/goal/get":
		reply["result"] = map[string]any{"goal": f.goal}
	case "thread/read":
		th := f.thread
		if params["includeTurns"] == false {
			th.Turns = []cxTurn{}
		}
		reply["result"] = map[string]any{"thread": th}
	case "thread/turns/list":
		limit := int(params["limit"].(float64))
		data := []cxTurn{}
		for i := len(f.thread.Turns) - 1; i >= 0 && len(data) < limit; i-- {
			data = append(data, f.thread.Turns[i])
		}
		reply["result"] = map[string]any{"data": data, "nextCursor": nil}
	case "thread/rollback", "thread/revert":
		if params["threadId"] == revertProbeThread {
			if method == "thread/rollback" {
				reply["error"] = map[string]any{"code": -32600, "message": "invalid thread id: invalid character"}
			} else {
				reply["error"] = map[string]any{"code": -32601, "message": "unknown method"}
			}
			break
		}
		if f.reject {
			reply["error"] = map[string]any{"code": -32602, "message": "native rejection"}
			break
		}
		if !f.noMutation && len(f.thread.Turns) > 0 {
			f.thread.Turns = f.thread.Turns[:len(f.thread.Turns)-1]
		}
		if f.internalErrorAfterCommit {
			reply["error"] = map[string]any{"code": -32603, "message": "native internal error after persistence"}
			break
		}
		if f.hold {
			f.mu.Unlock()
			return len(raw), nil
		}
		th := f.thread
		if f.badAck {
			th.ID = "wrong"
		}
		if method == "thread/revert" {
			th.Turns = []cxTurn{}
			reply["result"] = map[string]any{"thread": th, "turnsBackwardsCursor": nil, "itemsBackwardsCursor": nil}
		} else {
			reply["result"] = map[string]any{"thread": th}
		}
	default:
		f.mu.Unlock()
		return 0, errors.New("unexpected native method: " + method)
	}
	encoded, err := json.Marshal(reply)
	f.mu.Unlock()
	if err != nil {
		return 0, err
	}
	f.conn.handleLine(encoded)
	return len(raw), nil
}
func (f *revertFixtureTransport) calls(method string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []map[string]any{}
	for _, req := range f.requests {
		if req["method"] == method {
			out = append(out, req["params"].(map[string]any))
		}
	}
	return out
}
func revertUser(id string) json.RawMessage {
	return json.RawMessage(`{"type":"userMessage","id":"` + id + `","content":[{"type":"text","text":"hello"}]}`)
}

const revertOp = "91000000-0000-4000-8000-000000000001"

func newRevertFixture(t *testing.T, mode string) (*codexAgent, *revertFixtureTransport, *recorder) {
	t.Helper()
	a, _, rec := newResumeSafetyAgent(t, cxThread{}, false)
	a.stateFile = filepath.Join(t.TempDir(), "codex-owned.json")
	a.turnReverts = map[string][]*codexTurnRevertReceipt{}
	th := cxThread{ID: "revert-thread", Cwd: t.TempDir(), HistoryMode: mode, Status: cxThreadStatus{Type: "idle"}, Source: json.RawMessage(`"appServer"`), Turns: []cxTurn{
		{ID: "prior-turn", Status: "completed", ItemsView: "full", Items: []json.RawMessage{revertUser("prior-user")}},
		{ID: "last-turn", Status: "completed", ItemsView: "full", Items: []json.RawMessage{revertUser("last-user"), json.RawMessage(`{"type":"agentMessage","id":"answer","text":"response"}`)}},
	}}
	f := &revertFixtureTransport{conn: a.conn, thread: th}
	a.conn.stdin = f
	a.conn.mu.Lock()
	a.conn.latestRevertSupported = true
	a.conn.legacyRollbackSupported = true
	a.conn.mu.Unlock()
	a.mu.Lock()
	a.owned[th.ID] = true
	item := a.applyThreadLocked(th)
	item.loaded = true
	item.info.PermissionMode = "ask"
	item.info.LastTerminalTurnID = "last-turn"
	a.applyThreadLocked(th)
	a.mu.Unlock()
	return a, f, rec
}
func runRevert(a *codexAgent) (protocol.LatestTurnRevertResult, error) {
	return a.RevertLatestTurn(context.Background(), "revert-thread", "last-turn", "last-user", revertOp, nil)
}

func TestCodexLatestTurnRevertUsesNativeMethodAndPersistsTombstone(t *testing.T) {
	for _, mode := range []string{"legacy", "paginated"} {
		t.Run(mode, func(t *testing.T) {
			a, f, rec := newRevertFixture(t, mode)
			out, err := runRevert(a)
			if err != nil || out.Status != "completed" {
				t.Fatalf("result %#v %v", out, err)
			}
			method := "thread/rollback"
			params := map[string]any{"threadId": "revert-thread", "numTurns": float64(1)}
			if mode == "paginated" {
				method = "thread/revert"
				params = map[string]any{"threadId": "revert-thread", "beforeTurnId": "last-turn"}
			}
			if calls := f.calls(method); len(calls) != 1 || !reflect.DeepEqual(calls[0], params) {
				t.Fatalf("native calls %#v", calls)
			}
			if len(f.thread.Turns) != 1 || f.thread.Turns[0].ID != "prior-turn" {
				t.Fatal("retained native history changed")
			}
			again, err := runRevert(a)
			if err != nil || again.Status != "completed" || len(f.calls(method)) != 1 {
				t.Fatalf("replay %#v %v", again, err)
			}
			if len(f.calls("turn/start")) != 0 {
				t.Fatal("revert must never auto-send")
			}
			found := false
			for _, e := range rec.all() {
				if e.Type == "session.reverted" {
					found = e.ID == "last-user" && e.TurnID == "last-turn" && e.OperationID == revertOp
				}
			}
			if !found {
				t.Fatal("missing native-confirmed history event")
			}
			loaded := newCodexAgent(func(protocol.Event) {}, a.settings, "", a.stateFile)
			events := loaded.revertedHistoryEvents("revert-thread")
			if len(events) != 1 || events[0].TurnID != "last-turn" {
				t.Fatalf("offline tombstones %#v", events)
			}
			_, events, _, err = a.OpenMessagesPage(context.Background(), "revert-thread", "", 20)
			if err != nil {
				t.Fatal(err)
			}
			found = false
			for _, e := range events {
				if e.Type == "session.reverted" && e.TurnID == "last-turn" {
					found = true
				}
			}
			if !found {
				t.Fatal("fresh history omitted persisted tombstone")
			}
		})
	}
}
func TestCodexLatestTurnRevertRejectsUnsafeTargets(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*codexAgent, *revertFixtureTransport)
	}{
		{"running", func(a *codexAgent, f *revertFixtureTransport) {
			a.threads[f.thread.ID].turnID = "last-turn"
			a.threads[f.thread.ID].info.Status = "running"
		}},
		{"external", func(a *codexAgent, f *revertFixtureTransport) { a.owned[f.thread.ID] = false }},
		{"unloaded", func(a *codexAgent, f *revertFixtureTransport) { a.threads[f.thread.ID].loaded = false }},
		{"newer-turn", func(a *codexAgent, f *revertFixtureTransport) {
			f.thread.Turns = append(f.thread.Turns, cxTurn{ID: "newer", Status: "completed", Items: []json.RawMessage{revertUser("newer-user")}})
		}},
		{"multiple-user-steer", func(a *codexAgent, f *revertFixtureTransport) {
			f.thread.Turns[1].Items = append(f.thread.Turns[1].Items, revertUser("steer"))
		}},
		{"wrong-user", func(a *codexAgent, f *revertFixtureTransport) {
			f.thread.Turns[1].Items = []json.RawMessage{revertUser("other")}
		}},
		{"summary-items", func(a *codexAgent, f *revertFixtureTransport) { f.thread.Turns[1].ItemsView = "summary" }},
		{"unknown-history", func(a *codexAgent, f *revertFixtureTransport) { f.thread.HistoryMode = "future" }},
		{"native-active", func(a *codexAgent, f *revertFixtureTransport) { f.thread.Status.Type = "active" }},
		{"ephemeral", func(a *codexAgent, f *revertFixtureTransport) { f.thread.Ephemeral = true }},
		{"live-child", func(a *codexAgent, f *revertFixtureTransport) {
			a.threads[f.thread.ID].childStates = map[string]string{"child": "running"}
		}},
		{"goal-unknown", func(a *codexAgent, f *revertFixtureTransport) { f.goal = map[string]any{"status": "active"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			a, f, _ := newRevertFixture(t, "legacy")
			test.change(a, f)
			_, err := runRevert(a)
			if err == nil {
				t.Fatal("unsafe revert accepted")
			}
			if len(f.calls("thread/rollback"))+len(f.calls("thread/revert")) != 0 {
				t.Fatal("native mutation dispatched")
			}
		})
	}
}
func TestCodexLatestTurnRevertUncertaintyNeverReissues(t *testing.T) {
	for _, kind := range []string{"timeout", "bad-ack", "unchanged-history", "internal-error-after-commit"} {
		t.Run(kind, func(t *testing.T) {
			a, f, rec := newRevertFixture(t, "legacy")
			f.hold = kind == "timeout"
			f.badAck = kind == "bad-ack"
			f.noMutation = kind == "unchanged-history"
			f.internalErrorAfterCommit = kind == "internal-error-after-commit"
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			out, err := a.RevertLatestTurn(ctx, "revert-thread", "last-turn", "last-user", revertOp, nil)
			if err != nil || out.Status != "unconfirmed" {
				t.Fatalf("%#v %v", out, err)
			}
			if !a.threads["revert-thread"].codexSessionActionBusyLocked() {
				t.Fatal("uncertain native write did not fence later sends")
			}
			loaded := newCodexAgent(func(protocol.Event) {}, a.settings, "", a.stateFile)
			if !loaded.threads["revert-thread"].revertPending {
				t.Fatal("restart lost uncertainty fence")
			}
			for _, e := range rec.all() {
				if e.Type == "session.reverted" {
					t.Fatal("unconfirmed write advertised success")
				}
			}
			out, err = runRevert(a)
			want := "completed"
			if kind == "unchanged-history" {
				want = "unconfirmed"
			}
			if err != nil || out.Status != want || len(f.calls("thread/rollback")) != 1 {
				t.Fatal("uncertain native write was replayed")
			}
			if a.threads["revert-thread"].revertPending != (want == "unconfirmed") {
				t.Fatal("read-only recovery did not release exactly the confirmed fence")
			}
		})
	}
}

func TestCodexLatestTurnRevertHistoryReadReconcilesLostReply(t *testing.T) {
	a, f, _ := newRevertFixture(t, "legacy")
	f.hold = true
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if out, err := a.RevertLatestTurn(ctx, "revert-thread", "last-turn", "last-user", revertOp, nil); err != nil || out.Status != "unconfirmed" {
		t.Fatalf("%#v %v", out, err)
	}
	_, events, _, err := a.OpenMessagesPage(context.Background(), "revert-thread", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.Type == "session.reverted" && e.OperationID == revertOp {
			found = true
		}
	}
	if !found || a.threads["revert-thread"].revertPending || len(f.calls("thread/rollback")) != 1 {
		t.Fatal("opening history did not safely reconcile without another native write")
	}
}
func TestCodexLatestTurnRevertInvalidatesStaleWriteAndConflictingOperation(t *testing.T) {
	a, f, _ := newRevertFixture(t, "legacy")
	f.hook = func(method string) {
		if method == "thread/goal/get" {
			a.mu.Lock()
			a.threads["revert-thread"].turnVersion++
			a.mu.Unlock()
		}
	}
	if _, err := runRevert(a); err == nil || len(f.calls("thread/rollback")) != 0 {
		t.Fatal("stale snapshot admitted")
	}
	a, f, _ = newRevertFixture(t, "legacy")
	if _, err := runRevert(a); err != nil {
		t.Fatal(err)
	}
	if _, err := a.RevertLatestTurn(context.Background(), "revert-thread", "prior-turn", "prior-user", revertOp, nil); err == nil || len(f.calls("thread/rollback")) != 1 {
		t.Fatal("operation identity reused")
	}
}
func TestCodexTurnRevertProbeAndCapabilityFailClosed(t *testing.T) {
	a, f, _ := newRevertFixture(t, "legacy")
	a.conn.probeTurnRevert(context.Background())
	if a.conn.turnRevertMethod("paginated") != "" || a.conn.turnRevertMethod("legacy") != "thread/rollback" {
		t.Fatal("probe inferred unsupported mutation")
	}
	if !a.Descriptor().Capabilities.LatestTurnRevert {
		t.Fatal("native supported legacy capability missing")
	}
	a.mu.Lock()
	info := a.applyThreadLocked(f.thread).info
	a.owned[f.thread.ID] = false
	external := a.applyThreadLocked(f.thread).info
	a.mu.Unlock()
	if !info.LatestTurnRevert || external.LatestTurnRevert {
		t.Fatal("session capability ignored ownership")
	}
}
func TestCodexLatestTurnRevertRequiresDurableIntentBeforeNativeWrite(t *testing.T) {
	a, f, _ := newRevertFixture(t, "legacy")
	a.stateFile = ""
	if _, err := runRevert(a); err == nil || len(f.calls("thread/rollback")) != 0 {
		t.Fatal("native write occurred without durable intent")
	}
}

func TestCodexLatestTurnRevertRecoveryOnlyNeverCreatesAnIntent(t *testing.T) {
	a, f, _ := newRevertFixture(t, "legacy")
	if _, err := a.ReconcileLatestTurnRevert(context.Background(), "revert-thread", "last-turn", "last-user", revertOp, nil); err == nil {
		t.Fatal("missing receipt admitted recovery")
	}
	if len(f.calls("thread/rollback")) != 0 || len(a.turnReverts["revert-thread"]) != 0 {
		t.Fatal("recovery-only operation wrote native history")
	}
}

func TestCodexLatestTurnRevertFirstTurnCanLeaveEmptyNativeHistory(t *testing.T) {
	a, f, _ := newRevertFixture(t, "paginated")
	f.thread.Turns = f.thread.Turns[1:]
	out, err := runRevert(a)
	if err != nil || out.Status != "completed" || len(f.thread.Turns) != 0 || out.Session.LastTerminalTurnID != "" {
		t.Fatalf("first turn %#v %v", out, err)
	}
}

func TestCodexLatestTurnRevertCannotClearANewerNativeTurn(t *testing.T) {
	a, f, _ := newRevertFixture(t, "legacy")
	reads := 0
	f.hook = func(method string) {
		if method == "thread/turns/list" {
			reads++
			if reads == 2 {
				a.mu.Lock()
				item := a.threads["revert-thread"]
				item.turnVersion++
				item.turnID = "newer-active"
				item.info.ActiveTurnID = "newer-active"
				item.info.Status = "running"
				a.mu.Unlock()
			}
		}
	}
	out, err := runRevert(a)
	if err != nil || out.Status != "unconfirmed" {
		t.Fatalf("late turn %#v %v", out, err)
	}
	if a.threads["revert-thread"].turnID != "newer-active" || a.threads["revert-thread"].info.Status != "running" {
		t.Fatal("revert erased the newer native turn")
	}
}
