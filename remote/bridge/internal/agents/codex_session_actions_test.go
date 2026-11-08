package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/protocol"
)

// Recorded from an isolated, empty CODEX_HOME initialize-only handshake with
// the local 0.145 binary. The client version suffix is not a runtime version.
const sessionActionNativeUA = "Codex Desktop/0.145.0 (Windows 10.0.26200; x86_64) dumb (salcara_bridge; 1.0.0)"
const sessionActionThread = "action-original"

type codexSessionActionTransport struct {
	mu             sync.Mutex
	conn           *rpcConn
	threads        map[string]cxThread
	archived       map[string]bool
	requests       []map[string]any
	hook           func(string)
	hold           string
	deferArchive   bool
	overrideMethod string
	override       json.RawMessage
	rejectMethod   string
	delayed        []byte
	forks          int
}

func (f *codexSessionActionTransport) Close() error { return nil }
func (f *codexSessionActionTransport) Write(raw []byte) (int, error) {
	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil {
		return 0, err
	}
	method, _ := req["method"].(string)
	params, _ := req["params"].(map[string]any)
	id, _ := params["threadId"].(string)
	f.mu.Lock()
	f.requests = append(f.requests, req)
	hook := f.hook
	f.mu.Unlock()
	if hook != nil {
		hook(method)
	}
	f.mu.Lock()
	reply := map[string]any{"id": req["id"]}
	if method == f.rejectMethod {
		reply["error"] = map[string]any{"code": -32601, "message": "fixture unsupported"}
	} else {
		switch method {
		case "config/read":
			cfg, _ := guardedProviderConfig(Settings{RelayRoot: "https://fixture.invalid"})
			reply["result"] = map[string]any{"config": cfg}
		case "thread/list":
			archived, _ := params["archived"].(bool)
			ids := []string{}
			for key := range f.threads {
				if f.archived[key] == archived {
					ids = append(ids, key)
				}
			}
			sort.Strings(ids)
			offset := 0
			if cursor, ok := params["cursor"].(string); ok {
				offset, _ = strconv.Atoi(strings.TrimPrefix(cursor, "native-page-"))
			}
			limit, _ := params["limit"].(float64)
			if limit < 1 {
				limit = 100
			}
			data := []cxThread{}
			for offset < len(ids) && len(data) < int(limit) {
				data = append(data, f.threads[ids[offset]])
				offset++
			}
			next := ""
			if offset < len(ids) {
				next = fmt.Sprintf("native-page-%d", offset)
			}
			reply["result"] = map[string]any{"data": data, "nextCursor": next}
		case "thread/read":
			reply["result"] = map[string]any{"thread": f.threads[id]}
		case "thread/name/set":
			th := f.threads[id]
			name, _ := params["name"].(string)
			th.Name, th.UpdatedAt = &name, th.UpdatedAt+1
			f.threads[id] = th
			reply["result"] = map[string]any{}
		case "thread/archive":
			if !f.deferArchive {
				f.archived[id] = true
			}
			reply["result"] = map[string]any{}
		case "thread/unarchive":
			f.archived[id] = false
			reply["result"] = map[string]any{"thread": f.threads[id]}
		case "thread/fork":
			f.forks++
			th := f.threads[id]
			th.ID, th.ForkedFromID, th.Turns = fmt.Sprintf("native-fork-%d", f.forks), &id, []cxTurn{}
			f.threads[th.ID] = th
			reply["result"] = map[string]any{"thread": th, "model": "native-fork-model", "approvalPolicy": "on-request", "approvalsReviewer": "user", "sandbox": map[string]any{"type": "workspaceWrite"}}
		default:
			f.mu.Unlock()
			return 0, errors.New("unexpected session action RPC: " + method)
		}
	}
	if method == f.overrideMethod {
		reply["result"] = f.override
	}
	encoded, err := json.Marshal(reply)
	if method == f.hold {
		f.delayed = append([]byte(nil), encoded...)
		f.mu.Unlock()
		return len(raw), nil
	}
	f.mu.Unlock()
	if err != nil {
		return 0, err
	}
	f.conn.handleLine(encoded)
	return len(raw), nil
}

func (f *codexSessionActionTransport) calls(method string) []map[string]any {
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

func newCodexSessionActionFixture(t *testing.T) (*codexAgent, *codexSessionActionTransport, *recorder) {
	t.Helper()
	a, _, rec := newResumeSafetyAgent(t, cxThread{}, false)
	name := "Native original"
	th := cxThread{ID: sessionActionThread, Name: &name, Cwd: t.TempDir(), UpdatedAt: 100, Status: cxThreadStatus{Type: "idle"}, Turns: []cxTurn{}, Source: json.RawMessage(`"appServer"`)}
	f := &codexSessionActionTransport{conn: a.conn, threads: map[string]cxThread{th.ID: th}, archived: map[string]bool{}}
	a.conn.stdin = f
	a.conn.recordSessionInitialization(codexSessionInitialize{UserAgent: sessionActionNativeUA})
	a.mu.Lock()
	thread := a.applyThreadLocked(th)
	thread.loaded, thread.info.PermissionMode = true, "ask"
	a.mu.Unlock()
	return a, f, rec
}

func readSessionActionFixture(t *testing.T, a *codexAgent) protocol.SessionActions {
	t.Helper()
	out, err := a.SessionActions(context.Background(), sessionActionThread, nil)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sessionActionRequest(action, revision string, index int) protocol.SessionActionRequest {
	req := protocol.SessionActionRequest{Action: action, ExpectedRevision: revision, OperationID: fmt.Sprintf("51000000-0000-4000-8000-%012x", index)}
	if action == "rename" {
		req.Name = "Native renamed"
	}
	return req
}

func assertNoSessionModelCalls(t *testing.T, f *codexSessionActionTransport) {
	t.Helper()
	for _, method := range []string{"turn/start", "turn/steer", "thread/start", "thread/resume", "review/start", "thread/goal/set"} {
		if len(f.calls(method)) != 0 {
			t.Fatalf("session management invoked %s", method)
		}
	}
}

func TestCodexSessionActionsVersionBoundToInitializeConnection(t *testing.T) {
	for _, ua := range []string{"", "Codex Desktop/0.144.9 (Windows)", "Codex Desktop/0.146.0 (Windows)", "Codex Desktop/0.145.0-beta (Windows)", "unknown/0.145.0", "Codex Desktop/0.144.0 (salcara_bridge; 0.145.0)", sessionActionNativeUA, "codex_cli_rs/0.145.3 (linux)"} {
		t.Run(ua, func(t *testing.T) {
			a, f, _ := newCodexSessionActionFixture(t)
			a.conn.recordSessionInitialization(codexSessionInitialize{UserAgent: ua})
			out := readSessionActionFixture(t, a)
			supported := ua == sessionActionNativeUA || ua == "codex_cli_rs/0.145.3 (linux)"
			if supported != (len(out.Actions) == 3) {
				t.Fatalf("version capability: %q %+v", ua, out)
			}
			if !supported && (out.UnavailableReason != "当前 Codex 版本不支持" || len(f.calls("thread/list")) != 0) {
				t.Fatal(out)
			}
			if !supported {
				_, _, err := a.ArchivedSessions(context.Background(), "", 10, nil)
				if !errors.Is(err, errCodexSessionActionUnsupported) {
					t.Fatal(err)
				}
			}
		})
	}
	a, _, _ := newCodexSessionActionFixture(t)
	before := readSessionActionFixture(t, a)
	a.mu.Lock()
	a.conn = &rpcConn{pending: map[int64]chan rpcResult{}, done: make(chan struct{})}
	a.mu.Unlock()
	after := readSessionActionFixture(t, a)
	if after.Revision == before.Revision || len(after.Actions) != 0 {
		t.Fatalf("replacement inherited initialize proof: %+v", after)
	}
}

func TestCodexSessionActionsNativeRenameArchiveRestoreAndFork(t *testing.T) {
	a, f, _ := newCodexSessionActionFixture(t)
	for index, action := range []string{"rename", "archive", "unarchive", "fork"} {
		state := readSessionActionFixture(t, a)
		if !hasSessionAction(state.Actions, action) {
			t.Fatalf("missing %s: %+v", action, state)
		}
		req := sessionActionRequest(action, state.Revision, index+1)
		out, err := a.ApplySessionAction(context.Background(), sessionActionThread, req, nil)
		if err != nil || out.State != "completed" || out.Session == nil {
			t.Fatalf("%s: %+v %v", action, out, err)
		}
		if action == "rename" && out.Session.Title != req.Name {
			t.Fatal(out)
		}
		if action == "archive" {
			if !out.Session.Archived {
				t.Fatal("archive ack lost native state")
			}
			active, err := a.Sessions(context.Background())
			if err != nil || len(active) != 0 {
				t.Fatalf("archived thread reappeared from live cache: %+v %v", active, err)
			}
			archived, next, err := a.ArchivedSessions(context.Background(), "", 10, nil)
			if err != nil || len(archived) != 1 || !archived[0].Archived || next != "" {
				t.Fatalf("archive directory: %+v %s %v", archived, next, err)
			}
		}
		if action == "unarchive" {
			if out.Session.Archived || out.Session.SessionKey != "codex:"+sessionActionThread {
				t.Fatal(out)
			}
			archived, _, err := a.ArchivedSessions(context.Background(), "", 10, nil)
			if err != nil || len(archived) != 0 {
				t.Fatalf("restored thread remained archived: %+v %v", archived, err)
			}
			active, _, err := a.SessionsPage(context.Background(), "", 10, "")
			if err != nil || len(active) != 1 || active[0].Archived {
				t.Fatalf("restored thread missing from native active list: %+v %v", active, err)
			}
		}
		if action == "fork" {
			if out.Session.SessionKey == "codex:"+sessionActionThread || out.Session.ForkedFromSessionKey != "codex:"+sessionActionThread || out.Session.Model != "native-fork-model" || out.Session.PermissionMode != "ask" {
				t.Fatal(out)
			}
			want := map[string]any{"threadId": sessionActionThread, "deferGoalContinuation": true, "excludeTurns": true}
			if calls := f.calls("thread/fork"); len(calls) != 1 || !reflect.DeepEqual(calls[0], want) {
				t.Fatalf("unsafe fork params: %+v", calls)
			}
		} else {
			method := map[string]string{"rename": "thread/name/set", "archive": "thread/archive", "unarchive": "thread/unarchive"}[action]
			want := map[string]any{"threadId": sessionActionThread}
			if action == "rename" {
				want["name"] = req.Name
			}
			if calls := f.calls(method); len(calls) != 1 || !reflect.DeepEqual(calls[0], want) {
				t.Fatalf("unsafe %s params: %+v", action, calls)
			}
		}
		replay, err := a.ApplySessionAction(context.Background(), sessionActionThread, req, nil)
		if err != nil || replay.State != "completed" || replay.Session.SessionKey != out.Session.SessionKey {
			t.Fatalf("receipt replay: %+v %v", replay, err)
		}
	}
	assertNoSessionModelCalls(t, f)
}

func TestCodexSessionActionsCASAndBusyRefuseMutation(t *testing.T) {
	for _, scenario := range []string{"native-title", "native-active", "turn", "approval", "context", "child", "permission", "write-boundary"} {
		t.Run(scenario, func(t *testing.T) {
			a, f, _ := newCodexSessionActionFixture(t)
			state := readSessionActionFixture(t, a)
			switch scenario {
			case "native-title", "native-active":
				f.mu.Lock()
				th := f.threads[sessionActionThread]
				if scenario == "native-title" {
					name := "Changed elsewhere"
					th.Name = &name
				} else {
					th.Status.Type = "active"
				}
				f.threads[sessionActionThread] = th
				f.mu.Unlock()
			case "approval":
				a.aps.addRequest("codex:"+sessionActionThread, "codex", "fixture-approval", protocol.Event{Kind: "command"})
			case "write-boundary":
				f.hook = func(method string) {
					if method == "config/read" {
						a.aps.addRequest("codex:"+sessionActionThread, "codex", "late-approval", protocol.Event{Kind: "command"})
					}
				}
			default:
				a.mu.Lock()
				th := a.threads[sessionActionThread]
				switch scenario {
				case "turn":
					th.turnID = "new-native-turn"
				case "context":
					th.context.compaction = &codexCompaction{conn: a.conn}
					th.context.conn = a.conn
				case "child":
					th.childStates = map[string]string{"child": "running"}
				case "permission":
					th.permissionUnconfirmed = true
				}
				a.mu.Unlock()
			}
			_, err := a.ApplySessionAction(context.Background(), sessionActionThread, sessionActionRequest("rename", state.Revision, 1), nil)
			if err == nil || len(f.calls("thread/name/set")) != 0 {
				t.Fatalf("busy/stale mutation dispatched: %v", err)
			}
			assertNoSessionModelCalls(t, f)
		})
	}
}

func TestCodexSessionActionInvalidAndUnconfirmedRepliesNeverReplay(t *testing.T) {
	for _, action := range []string{"rename", "archive", "unarchive", "fork"} {
		t.Run(action, func(t *testing.T) {
			a, f, _ := newCodexSessionActionFixture(t)
			if action == "unarchive" {
				f.archived[sessionActionThread] = true
			}
			state := readSessionActionFixture(t, a)
			req := sessionActionRequest(action, state.Revision, 1)
			method := map[string]string{"rename": "thread/name/set", "archive": "thread/archive", "unarchive": "thread/unarchive", "fork": "thread/fork"}[action]
			f.overrideMethod, f.override = method, json.RawMessage(`null`)
			out, err := a.ApplySessionAction(context.Background(), sessionActionThread, req, nil)
			if err != nil || out.State != "unconfirmed" || out.Session != nil {
				t.Fatalf("malformed acknowledgement claimed completion: %+v %v", out, err)
			}
			replay, err := a.ApplySessionAction(context.Background(), sessionActionThread, req, nil)
			if err != nil || replay.State != "unconfirmed" || len(f.calls(method)) != 1 {
				t.Fatalf("uncertain operation redispatched: %+v %v", replay, err)
			}
			req.Action = "fork"
			if action == "fork" {
				req.Action = "archive"
			}
			req.Name = ""
			if _, err := a.ApplySessionAction(context.Background(), sessionActionThread, req, nil); err == nil {
				t.Fatal("operation ID accepted different content")
			}
			assertNoSessionModelCalls(t, f)
		})
	}
}

func TestCodexSessionActionCancellationKeepsReceiptAndBusyFence(t *testing.T) {
	a, f, _ := newCodexSessionActionFixture(t)
	state := readSessionActionFixture(t, a)
	req := sessionActionRequest("rename", state.Revision, 1)
	f.hold = "thread/name/set"
	ctx, cancel := context.WithCancel(context.Background())
	f.hook = func(method string) {
		if method == f.hold {
			cancel()
		}
	}
	_, err := a.ApplySessionAction(ctx, sessionActionThread, req, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller returned native content: %v", err)
	}
	out, err := a.ApplySessionAction(context.Background(), sessionActionThread, req, nil)
	if err != nil || out.State != "unconfirmed" || out.Session != nil || len(f.calls("thread/name/set")) != 1 {
		t.Fatalf("missing response escaped uncertainty: %+v %v", out, err)
	}
	contextState, err := a.SessionContext(context.Background(), sessionActionThread, nil)
	if err != nil || contextState.CanCompact || contextState.UnavailableReason != "session_action_busy" {
		t.Fatalf("compaction escaped busy fence: %+v %v", contextState, err)
	}
	if err := a.Send(context.Background(), sessionActionThread, "must not execute"); !errors.Is(err, errCodexSessionActionBusy) {
		t.Fatal(err)
	}
	state = readSessionActionFixture(t, a)
	if len(state.Actions) != 0 || state.UnavailableReason != "上次操作结果未确认" {
		t.Fatal(state)
	}
	if _, err := a.ApplySessionAction(context.Background(), sessionActionThread, sessionActionRequest("rename", state.Revision, 2), nil); err == nil {
		t.Fatal("new operation bypassed unresolved mutation")
	}
	f.mu.Lock()
	late := append([]byte(nil), f.delayed...)
	f.mu.Unlock()
	f.conn.handleLine(late)
	state = readSessionActionFixture(t, a)
	if !hasSessionAction(state.Actions, "rename") {
		t.Fatalf("real late acknowledgement did not release the fence: %+v", state)
	}
	out, err = a.ApplySessionAction(context.Background(), sessionActionThread, req, nil)
	if err != nil || out.State != "completed" || out.Session == nil || out.Session.Title != req.Name || len(f.calls("thread/name/set")) != 1 {
		t.Fatalf("late acknowledgement was not recovered exactly once: %+v %v", out, err)
	}
	assertNoSessionModelCalls(t, f)
}

func TestCodexSessionActionConcurrentRetriesShareNativeAcknowledgement(t *testing.T) {
	a, f, _ := newCodexSessionActionFixture(t)
	req := sessionActionRequest("rename", readSessionActionFixture(t, a).Revision, 1)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	f.hook = func(method string) {
		if method == "thread/name/set" {
			close(entered)
			<-release
		}
	}
	type response struct {
		result protocol.SessionActionResult
		err    error
	}
	done := make(chan response, 2)
	call := func() {
		r, e := a.ApplySessionAction(context.Background(), sessionActionThread, req, nil)
		done <- response{r, e}
	}
	go call()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("native request did not start")
	}
	go call()
	unblock()
	for i := 0; i < 2; i++ {
		select {
		case out := <-done:
			if out.err != nil || out.result.State != "completed" {
				t.Fatalf("%+v", out)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("duplicate did not finish")
		}
	}
	if len(f.calls("thread/name/set")) != 1 {
		t.Fatal("concurrent operation replayed")
	}
}

func TestCodexSessionActionSourceGuardAtWriteAndReturn(t *testing.T) {
	for _, afterWrite := range []bool{false, true} {
		t.Run(fmt.Sprintf("after-write=%t", afterWrite), func(t *testing.T) {
			a, f, rec := newCodexSessionActionFixture(t)
			req := sessionActionRequest("rename", readSessionActionFixture(t, a).Revision, 1)
			var revoked atomic.Bool
			denied := errors.New("fixture source revoked")
			validate := func() error {
				if revoked.Load() {
					return denied
				}
				return nil
			}
			f.hook = func(method string) {
				if method == "config/read" && !afterWrite || method == "thread/name/set" && afterWrite {
					revoked.Store(true)
				}
			}
			out, err := a.ApplySessionAction(context.Background(), sessionActionThread, req, validate)
			if !errors.Is(err, denied) || out.Session != nil || out.State != "" {
				t.Fatalf("revoked source received result: %+v %v", out, err)
			}
			if !afterWrite && len(f.calls("thread/name/set")) != 0 {
				t.Fatal("revoked source reached native write")
			}
			if afterWrite {
				revoked.Store(false)
				replay, err := a.ApplySessionAction(context.Background(), sessionActionThread, req, validate)
				if err != nil || replay.State != "completed" || len(f.calls("thread/name/set")) != 1 {
					t.Fatalf("completed receipt lost after source denial: %+v %v", replay, err)
				}
			}
			if len(rec.all()) != 0 {
				t.Fatal("source denial leaked session result through event")
			}
		})
	}
}

func TestCodexSessionActionLateOldConnectionCannotProjectResult(t *testing.T) {
	a, f, _ := newCodexSessionActionFixture(t)
	req := sessionActionRequest("fork", readSessionActionFixture(t, a).Revision, 1)
	f.hook = func(method string) {
		if method != "thread/fork" {
			return
		}
		replacement := &rpcConn{pending: map[int64]chan rpcResult{}, done: make(chan struct{})}
		replacement.recordSessionInitialization(codexSessionInitialize{UserAgent: sessionActionNativeUA})
		a.mu.Lock()
		a.conn = replacement
		a.syncCodexSessionActionsLocked(a.threads[sessionActionThread])
		a.threads[sessionActionThread].info.Title = "Successor state"
		a.mu.Unlock()
	}
	out, err := a.ApplySessionAction(context.Background(), sessionActionThread, req, nil)
	if err != nil || out.State != "unconfirmed" || out.Session != nil {
		t.Fatalf("old connection reply escaped: %+v %v", out, err)
	}
	a.mu.Lock()
	title := a.threads[sessionActionThread].info.Title
	child := a.threads["native-fork-1"]
	a.mu.Unlock()
	if title != "Successor state" || child != nil {
		t.Fatal("old connection updated current session cache")
	}
	replay, err := a.ApplySessionAction(context.Background(), sessionActionThread, req, nil)
	if err != nil || replay.State != "unconfirmed" || len(f.calls("thread/fork")) != 1 {
		t.Fatalf("old connection receipt replayed: %+v %v", replay, err)
	}
}

func TestCodexArchivedSessionsNativePaginationAndConnectionScope(t *testing.T) {
	a, f, _ := newCodexSessionActionFixture(t)
	f.mu.Lock()
	for i := 0; i < 3; i++ {
		th := f.threads[sessionActionThread]
		th.ID = fmt.Sprintf("archived-%d", i)
		f.threads[th.ID] = th
		f.archived[th.ID] = true
	}
	f.mu.Unlock()
	cursor := ""
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		rows, next, err := a.ArchivedSessions(context.Background(), cursor, 1, nil)
		if err != nil || len(rows) != 1 || !rows[0].Archived || seen[rows[0].SessionKey] {
			t.Fatalf("native archived page: %+v %s %v", rows, next, err)
		}
		seen[rows[0].SessionKey] = true
		cursor = next
	}
	if cursor != "" || len(seen) != 3 {
		t.Fatal("archive paging did not finish")
	}
	_, cursor, err := a.ArchivedSessions(context.Background(), "", 1, nil)
	if err != nil || cursor == "" {
		t.Fatal(err)
	}
	a.conn.recordSessionInitialization(codexSessionInitialize{UserAgent: sessionActionNativeUA})
	if _, _, err := a.ArchivedSessions(context.Background(), cursor, 1, nil); !errors.Is(err, errCodexSessionActionStale) {
		t.Fatal("cursor crossed connection epoch", err)
	}
	for _, params := range f.calls("thread/list") {
		if params["archived"] != true {
			t.Fatal("archive request dropped its native filter")
		}
	}
	assertNoSessionModelCalls(t, f)
}

func TestCodexArchivedSessionsNeverFallBackOrGuessFromPath(t *testing.T) {
	a, f, _ := newCodexSessionActionFixture(t)
	path := "C:/archive/looks-archived.jsonl"
	f.mu.Lock()
	th := f.threads[sessionActionThread]
	th.Path = &path
	f.threads[sessionActionThread] = th
	f.mu.Unlock()
	state := readSessionActionFixture(t, a)
	if state.Archived || !hasSessionAction(state.Actions, "archive") {
		t.Fatal("path was treated as native archive evidence", state)
	}
	before := len(f.calls("thread/list"))
	f.rejectMethod = "thread/list"
	if rows, _, err := a.ArchivedSessions(context.Background(), "", 10, nil); err == nil || rows != nil {
		t.Fatal("unsupported archive filter succeeded")
	}
	if len(f.calls("thread/list")) != before+1 {
		t.Fatal("archive list retried without its filter")
	}
}

func TestCodexSessionActionAcknowledgementMustMatchNativeContract(t *testing.T) {
	for _, scenario := range []struct{ action, reply string }{
		{"rename", `{"accepted":false}`},
		{"archive", `{"pending":true}`},
		{"unarchive", `{"thread":{"id":"another-thread","status":{"type":"idle"}}}`},
		{"fork", `{"thread":{"id":"action-original","forkedFromId":"action-original","status":{"type":"idle"}},"model":"native"}`},
		{"fork", `{"thread":{"id":"new-thread","forkedFromId":"other-parent","status":{"type":"idle"}},"model":"native"}`},
		{"fork", `{"thread":{"id":"new-thread","forkedFromId":"action-original","status":{"type":"active"}},"model":"native"}`},
		{"fork", `{"thread":{"id":"new-thread","forkedFromId":"action-original","status":{"type":"idle"}}}`},
	} {
		t.Run(scenario.action+scenario.reply, func(t *testing.T) {
			a, f, _ := newCodexSessionActionFixture(t)
			if scenario.action == "unarchive" {
				f.archived[sessionActionThread] = true
			}
			state := readSessionActionFixture(t, a)
			f.overrideMethod = map[string]string{"rename": "thread/name/set", "archive": "thread/archive", "unarchive": "thread/unarchive", "fork": "thread/fork"}[scenario.action]
			f.override = json.RawMessage(scenario.reply)
			out, err := a.ApplySessionAction(context.Background(), sessionActionThread, sessionActionRequest(scenario.action, state.Revision, 1), nil)
			if err != nil || out.State != "unconfirmed" || out.Session != nil {
				t.Fatalf("invalid ack became success: %+v %v", out, err)
			}
		})
	}
}

func TestCodexSessionActionNativeMethodRejectionIsRemembered(t *testing.T) {
	a, f, _ := newCodexSessionActionFixture(t)
	request := sessionActionRequest("fork", readSessionActionFixture(t, a).Revision, 1)
	f.rejectMethod = "thread/fork"
	for i := 0; i < 2; i++ {
		_, err := a.ApplySessionAction(context.Background(), sessionActionThread, request, nil)
		if !errors.Is(err, errCodexSessionActionUnsupported) {
			t.Fatal(err)
		}
	}
	if len(f.calls("thread/fork")) != 1 {
		t.Fatal("rejected native method was retried")
	}
	state := readSessionActionFixture(t, a)
	if hasSessionAction(state.Actions, "fork") || !hasSessionAction(state.Actions, "rename") {
		t.Fatal(state)
	}
}

func TestCodexSessionActionReturnAndReceiptCannotRefreshSuccessorState(t *testing.T) {
	a, f, rec := newCodexSessionActionFixture(t)
	req := sessionActionRequest("rename", readSessionActionFixture(t, a).Revision, 1)
	var acknowledged atomic.Bool
	f.hook = func(method string) {
		if method == "thread/name/set" {
			acknowledged.Store(true)
		}
	}
	validate := func() error {
		if acknowledged.Swap(false) {
			replacement := &rpcConn{pending: map[int64]chan rpcResult{}, done: make(chan struct{})}
			a.mu.Lock()
			a.conn = replacement
			a.syncCodexSessionActionsLocked(a.threads[sessionActionThread])
			a.threads[sessionActionThread].info.Title = "New worker title"
			a.mu.Unlock()
		}
		return nil
	}
	out, err := a.ApplySessionAction(context.Background(), sessionActionThread, req, validate)
	if err != nil || out.State != "unconfirmed" || out.Session != nil || len(rec.all()) != 0 {
		t.Fatalf("old ack returned into successor: %+v %v", out, err)
	}
	out, err = a.ApplySessionAction(context.Background(), sessionActionThread, req, nil)
	if err != nil || out.State != "unconfirmed" || out.Session != nil || len(f.calls("thread/name/set")) != 1 {
		t.Fatalf("old receipt projected into successor: %+v %v", out, err)
	}
}

func TestCodexArchivedSessionsSourceGuardDoesNotReleaseRows(t *testing.T) {
	a, f, _ := newCodexSessionActionFixture(t)
	f.archived[sessionActionThread] = true
	var revoked atomic.Bool
	f.hook = func(method string) {
		if method == "thread/list" {
			revoked.Store(true)
		}
	}
	denied := errors.New("fixture directory source revoked")
	validate := func() error {
		if revoked.Load() {
			return denied
		}
		return nil
	}
	rows, next, err := a.ArchivedSessions(context.Background(), "", 10, validate)
	if !errors.Is(err, denied) || rows != nil || next != "" {
		t.Fatalf("revoked directory released content: %+v %s %v", rows, next, err)
	}
}

func TestCodexArchivedMembershipAllowsRestoreAfterArchiveFileWrite(t *testing.T) {
	a, f, _ := newCodexSessionActionFixture(t)
	path := resumeSafetyRollout(t, 0)
	f.mu.Lock()
	th := f.threads[sessionActionThread]
	th.Path, th.Status.Type = &path, "notLoaded"
	f.threads[sessionActionThread], f.archived[sessionActionThread] = th, true
	f.mu.Unlock()
	a.mu.Lock()
	a.threads[sessionActionThread].loaded = false
	a.mu.Unlock()
	state := readSessionActionFixture(t, a)
	if !state.Archived || !reflect.DeepEqual(state.Actions, []string{"unarchive"}) {
		t.Fatalf("archive write mistaken for a running turn: %+v", state)
	}
	if result, err := a.ApplySessionAction(context.Background(), sessionActionThread, sessionActionRequest("unarchive", state.Revision, 1), nil); err != nil || result.State != "completed" {
		t.Fatalf("native restore failed: %+v %v", result, err)
	}
}

func TestCodexSessionActionLateArchiveNeverUnlocksFromIdleRead(t *testing.T) {
	a, f, _ := newCodexSessionActionFixture(t)
	state := readSessionActionFixture(t, a)
	req := sessionActionRequest("archive", state.Revision, 1)
	f.hold, f.deferArchive = "thread/archive", true
	ctx, cancel := context.WithCancel(context.Background())
	f.hook = func(method string) {
		if method == "thread/archive" {
			cancel()
		}
	}
	if _, err := a.ApplySessionAction(ctx, sessionActionThread, req, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// The server has received the archive request but has not applied it yet.
	// Its concurrent read RPCs honestly report an active-directory idle thread.
	state = readSessionActionFixture(t, a)
	if state.Archived || len(state.Actions) != 0 || state.UnavailableReason != "上次操作结果未确认" {
		t.Fatalf("idle native read incorrectly resolved the pending archive: %+v", state)
	}
	if err := a.Send(context.Background(), sessionActionThread, "must stay fenced"); !errors.Is(err, errCodexSessionActionBusy) {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.archived[sessionActionThread] = true
	late := append([]byte(nil), f.delayed...)
	f.mu.Unlock()
	f.conn.handleLine(late)
	const readers = 8
	results := make(chan protocol.SessionActionResult, readers)
	failures := make(chan error, readers)
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := a.ApplySessionAction(context.Background(), sessionActionThread, req, nil)
			results <- out
			failures <- err
		}()
	}
	wg.Wait()
	for i := 0; i < readers; i++ {
		out, err := <-results, <-failures
		if err != nil || out.State != "completed" || out.Session == nil || !out.Session.Archived {
			t.Fatalf("late archive receipt: %+v %v", out, err)
		}
	}
	if err := a.Send(context.Background(), sessionActionThread, "must remain archived"); !errors.Is(err, errCodexSessionActionBusy) {
		t.Fatal(err)
	}
	a.mu.Lock()
	thread := a.threads[sessionActionThread]
	busy, archived, pending := thread.codexSessionActionBusyLocked(), thread.info.Archived, thread.sessionActions.receipts[req.OperationID].pending
	a.mu.Unlock()
	f.conn.mu.Lock()
	listeners := len(f.conn.pending)
	observers := len(f.conn.sessionActionObservers)
	f.conn.mu.Unlock()
	if busy || !archived || pending != nil || listeners != 0 || observers != 0 || len(f.calls("thread/archive")) != 1 {
		t.Fatalf("late archive was not consumed once: busy=%v archived=%v pending=%v listeners=%d observers=%d", busy, archived, pending != nil, listeners, observers)
	}
	assertNoSessionModelCalls(t, f)
}

func TestCodexSessionActionUnknownForkSurvivesConnectionRetirement(t *testing.T) {
	a, f, _ := newCodexSessionActionFixture(t)
	state := readSessionActionFixture(t, a)
	req := sessionActionRequest("fork", state.Revision, 1)
	f.hold = "thread/fork"
	ctx, cancel := context.WithCancel(context.Background())
	f.hook = func(method string) {
		if method == "thread/fork" {
			cancel()
		}
	}
	if _, err := a.ApplySessionAction(ctx, sessionActionThread, req, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	old := f.conn
	old.shutdown(errors.New("fixture confirmed exit"))
	old.mu.Lock()
	remaining := len(old.pending)
	observers := len(old.sessionActionObservers)
	old.mu.Unlock()
	if remaining != 0 || observers != 0 {
		t.Fatalf("connection exit retained %d response channels and %d observers", remaining, observers)
	}
	next := &rpcConn{stdin: f, pending: map[int64]chan rpcResult{}, done: make(chan struct{})}
	next.recordSessionInitialization(codexSessionInitialize{UserAgent: sessionActionNativeUA})
	f.mu.Lock()
	f.conn, f.hook, f.hold = next, nil, ""
	f.mu.Unlock()
	a.mu.Lock()
	a.conn = next
	a.markUnloadedLocked()
	thread := a.threads[sessionActionThread]
	retained := thread.sessionActions.receipts[req.OperationID].pending != nil
	a.mu.Unlock()
	if retained {
		t.Fatal("retired receipt retained an old connection observer")
	}
	state = readSessionActionFixture(t, a)
	if hasSessionAction(state.Actions, "fork") || !hasSessionAction(state.Actions, "rename") || !hasSessionAction(state.Actions, "archive") {
		t.Fatalf("unknown fork could duplicate, or blocked unrelated actions: %+v", state)
	}
	if _, err := a.ApplySessionAction(context.Background(), sessionActionThread, sessionActionRequest("fork", state.Revision, 2), nil); err == nil {
		t.Fatal("a new operation ID duplicated an unknown fork")
	}
	if out, err := a.ApplySessionAction(context.Background(), sessionActionThread, req, nil); err != nil || out.State != "unconfirmed" || out.Session != nil {
		t.Fatalf("old fork receipt was adopted after connection retirement: %+v %v", out, err)
	}
	if out, err := a.ApplySessionAction(context.Background(), sessionActionThread, sessionActionRequest("rename", state.Revision, 3), nil); err != nil || out.State != "completed" {
		t.Fatalf("unrelated parent action did not recover: %+v %v", out, err)
	}
	if len(f.calls("thread/fork")) != 1 {
		t.Fatal("unknown fork was sent again")
	}
	assertNoSessionModelCalls(t, f)
}

func TestCodexSessionActionObserverAndReceiptCapacityNeverDispatch(t *testing.T) {
	for _, capacity := range []string{"observers", "received-observers", "receipts"} {
		t.Run(capacity, func(t *testing.T) {
			a, f, _ := newCodexSessionActionFixture(t)
			state := readSessionActionFixture(t, a)
			if capacity != "receipts" {
				f.conn.mu.Lock()
				f.conn.sessionActionObservers = map[int64]struct{}{}
				for i := 0; i < maxCodexSessionActionReceipts; i++ {
					id := int64(10000 + i)
					f.conn.pending[id], f.conn.pendingMethods[id] = make(chan rpcResult, 1), "thread/fork"
					f.conn.sessionActionObservers[id] = struct{}{}
				}
				f.conn.mu.Unlock()
				if capacity == "received-observers" {
					for i := 0; i < maxCodexSessionActionReceipts; i++ {
						f.conn.handleLine([]byte(fmt.Sprintf(`{"id":%d,"result":{}}`, 10000+i)))
					}
				}
			} else {
				a.mu.Lock()
				thread := a.threads[sessionActionThread]
				thread.sessionActions.receipts = map[string]*codexSessionActionReceipt{}
				for i := 0; i < maxCodexSessionActionReceipts; i++ {
					thread.sessionActions.receipts[fmt.Sprint(i)] = &codexSessionActionReceipt{terminal: true}
				}
				a.mu.Unlock()
			}
			if _, err := a.ApplySessionAction(context.Background(), sessionActionThread, sessionActionRequest("rename", state.Revision, 1), nil); err == nil || !strings.HasPrefix(err.Error(), "session_action_limit:") {
				t.Fatalf("capacity returned %v", err)
			}
			if len(f.calls("thread/name/set")) != 0 {
				t.Fatal("capacity error followed a native mutation")
			}
			f.conn.mu.Lock()
			pending := len(f.conn.pending)
			observers := len(f.conn.sessionActionObservers)
			f.conn.mu.Unlock()
			want := 0
			if capacity == "observers" {
				want = maxCodexSessionActionReceipts
			}
			if pending != want {
				t.Fatalf("capacity failure leaked observers: got %d want %d", pending, want)
			}
			want = 0
			if capacity != "receipts" {
				want = maxCodexSessionActionReceipts
			}
			if observers != want {
				t.Fatalf("capacity failure leaked retained acknowledgements: got %d want %d", observers, want)
			}
		})
	}
}

func TestCodexSessionActionLateReceiptIsSourceGuarded(t *testing.T) {
	a, f, rec := newCodexSessionActionFixture(t)
	state := readSessionActionFixture(t, a)
	req := sessionActionRequest("rename", state.Revision, 1)
	f.hold = "thread/name/set"
	ctx, cancel := context.WithCancel(context.Background())
	f.hook = func(method string) {
		if method == "thread/name/set" {
			cancel()
		}
	}
	if _, err := a.ApplySessionAction(ctx, sessionActionThread, req, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	f.mu.Lock()
	late := append([]byte(nil), f.delayed...)
	f.mu.Unlock()
	f.conn.handleLine(late)
	denied := errors.New("fixture source denied")
	if _, err := a.ApplySessionAction(context.Background(), sessionActionThread, req, func() error { return denied }); !errors.Is(err, denied) {
		t.Fatal(err)
	}
	if rec.count(func(event protocol.Event) bool { return event.Type == "session.updated" }) != 0 {
		t.Fatal("late receipt escaped a denied source through an event")
	}
	out, err := a.ApplySessionAction(context.Background(), sessionActionThread, req, nil)
	if err != nil || out.State != "completed" || out.Session == nil || len(f.calls("thread/name/set")) != 1 {
		t.Fatalf("authorized late receipt failed: %+v %v", out, err)
	}
}
