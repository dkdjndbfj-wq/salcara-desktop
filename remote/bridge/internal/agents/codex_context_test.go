package agents

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"salcara/bridge/internal/protocol"
)

const contextOperation = "33333333-2222-4333-8444-555555555555"
const otherContextOperation = "44444444-2222-4333-8444-555555555555"

type codexContextTransport struct {
	mu        sync.Mutex
	conn      *rpcConn
	requests  []map[string]any
	hook      func(string)
	rejection *rpcError
	holdReply bool
	nullReply bool
}

func (f *codexContextTransport) Close() error { return nil }
func (f *codexContextTransport) Write(raw []byte) (int, error) {
	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil {
		return 0, err
	}
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	method, _ := req["method"].(string)
	if f.hook != nil {
		f.hook(method)
	}
	response := map[string]any{"id": req["id"]}
	switch method {
	case "config/read":
		cfg, _ := guardedProviderConfig(Settings{RelayRoot: "https://fixture.invalid"})
		response["result"] = map[string]any{"config": cfg}
	case "thread/compact/start":
		if f.holdReply {
			return len(raw), nil
		}
		if f.rejection != nil {
			response["error"] = f.rejection
		} else if f.nullReply {
			response["result"] = nil
		} else {
			response["result"] = map[string]any{}
		}
	default:
		return 0, errors.New("unexpected context fixture RPC: " + method)
	}
	encoded, _ := json.Marshal(response)
	f.conn.handleLine(encoded)
	return len(raw), nil
}

func (f *codexContextTransport) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, req := range f.requests {
		if req["method"] == method {
			n++
		}
	}
	return n
}

func newCodexContextFixture(t *testing.T) (*codexAgent, *codexContextTransport, *recorder) {
	t.Helper()
	a, _, rec := newResumeSafetyAgent(t, cxThread{}, false)
	thread := a.threadLocked("context-thread")
	thread.loaded = true
	thread.info.Cwd, thread.info.Model, thread.info.PermissionMode = t.TempDir(), "fixture-model", "ask"
	f := &codexContextTransport{conn: a.conn}
	a.conn.stdin = f
	a.installCodexNotificationHandler(a.conn)
	return a, f, rec
}

func emitCodexContextNote(c *rpcConn, method string, params any) {
	encoded, _ := json.Marshal(map[string]any{"method": method, "params": params})
	c.handleLine(encoded)
}

func emitCodexContextTurn(c *rpcConn, id, status string) {
	method := "turn/started"
	if status != "inProgress" {
		method = "turn/completed"
	}
	emitCodexContextNote(c, method, map[string]any{"threadId": "context-thread", "turn": map[string]any{"id": id, "status": status}})
}

func emitCodexCompactionItem(c *rpcConn, turn, item string, completed bool) {
	method := "item/started"
	if completed {
		method = "item/completed"
	}
	emitCodexContextNote(c, method, map[string]any{"threadId": "context-thread", "turnId": turn, "item": map[string]any{"id": item, "type": "contextCompaction"}})
}

func emitCodexContextUsage(c *rpcConn, turn string, used int64, window any) {
	emitCodexContextNote(c, "thread/tokenUsage/updated", map[string]any{
		"threadId": "context-thread", "turnId": turn,
		"tokenUsage": map[string]any{"last": map[string]any{"totalTokens": used}, "modelContextWindow": window},
	})
}

func readCodexContext(t *testing.T, a *codexAgent) protocol.SessionContext {
	t.Helper()
	out, err := a.SessionContext(context.Background(), "context-thread", nil)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCodexContextUsesLastNativeRequestWithoutRPC(t *testing.T) {
	a, f, _ := newCodexContextFixture(t)
	initial := readCodexContext(t, a)
	if initial.Snapshot != nil || !initial.CanCompact || initial.Revision == "" || f.count("thread/read") != 0 {
		t.Fatal(initial)
	}
	emitCodexContextTurn(a.conn, "ordinary-turn", "inProgress")
	emitCodexContextNote(a.conn, "thread/tokenUsage/updated", map[string]any{
		"threadId": "context-thread", "turnId": "ordinary-turn",
		"tokenUsage": map[string]any{"last": map[string]any{"totalTokens": 84, "inputTokens": 80, "outputTokens": 4},
			"total": map[string]any{"totalTokens": 9000000, "inputTokens": 8000000, "outputTokens": 1000000}, "modelContextWindow": 1000},
	})
	out := readCodexContext(t, a)
	if out.Snapshot == nil || out.Snapshot.UsedTokens != 84 || out.Snapshot.WindowTokens == nil || *out.Snapshot.WindowTokens != 1000 ||
		out.Snapshot.Source != "codex_token_usage" || out.Snapshot.Basis != "last_request" || out.Snapshot.WindowKind != "model" || out.Snapshot.MeasuredAt <= 0 || out.CanCompact {
		t.Fatal(out)
	}
	*out.Snapshot.WindowTokens = 1
	out.Snapshot.UsedTokens = 2
	if again := readCodexContext(t, a); again.Snapshot.UsedTokens != 84 || *again.Snapshot.WindowTokens != 1000 {
		t.Fatal("caller changed owned context snapshot", again)
	}
	emitCodexContextTurn(a.conn, "ordinary-turn", "completed")
	if out := readCodexContext(t, a); out.Snapshot == nil || !out.CanCompact {
		t.Fatal(out)
	}
	emitCodexContextNote(a.conn, "model/rerouted", map[string]any{"threadId": "context-thread", "turnId": "ordinary-turn", "fromModel": "fixture-model", "toModel": "different-model"})
	if out := readCodexContext(t, a); out.Snapshot != nil {
		t.Fatal("previous model's window was retained", out)
	}
	if len(f.requests) != 0 {
		t.Fatal("context read issued native requests", f.requests)
	}
}

func TestCodexContextRejectsMalformedForeignAndOldRuntimeUsage(t *testing.T) {
	a, f, _ := newCodexContextFixture(t)
	emitCodexContextTurn(a.conn, "current-turn", "inProgress")
	before := readCodexContext(t, a)
	for _, params := range []string{
		`{"threadId":"context-thread","turnId":"current-turn","tokenUsage":{"last":{"totalTokens":-1},"modelContextWindow":1000}}`,
		`{"threadId":"context-thread","turnId":"current-turn","tokenUsage":{"last":{"totalTokens":1.5},"modelContextWindow":1000}}`,
		`{"threadId":"context-thread","turnId":"current-turn","tokenUsage":{"last":{"totalTokens":9007199254740992},"modelContextWindow":1000}}`,
		`{"threadId":"context-thread","turnId":"current-turn","tokenUsage":{"last":{"inputTokens":55},"total":{"totalTokens":5555}}}`,
		`{"threadId":"context-thread","turnId":"old-turn","tokenUsage":{"last":{"totalTokens":50},"modelContextWindow":1000}}`,
		`{"threadId":"other-thread","turnId":"current-turn","tokenUsage":{"last":{"totalTokens":50},"modelContextWindow":1000}}`,
		`{"threadId":"context-thread","turnId":"current-turn","tokenUsage":{"last":{"totalTokens":50},"modelContextWindow":0}}`,
	} {
		a.conn.handleLine([]byte(`{"method":"thread/tokenUsage/updated","params":` + params + `}`))
		if out := readCodexContext(t, a); out.Snapshot != nil || out.Revision != before.Revision {
			t.Fatal("invalid context snapshot admitted", params, out)
		}
	}
	emitCodexContextNote(a.conn, "thread/tokenUsage/updated", map[string]any{"threadId": "context-thread", "turnId": "current-turn", "tokenUsage": map[string]any{"last": map[string]any{"totalTokens": 0}, "modelContextWindow": nil}})
	if out := readCodexContext(t, a); out.Snapshot == nil || out.Snapshot.UsedTokens != 0 || out.Snapshot.WindowTokens != nil || out.Snapshot.Model != "" {
		t.Fatal("known zero or unknown window was fabricated", out)
	}
	old := a.conn
	a.mu.Lock()
	a.conn = nil
	a.markUnloadedLocked()
	a.mu.Unlock()
	emitCodexContextTurn(old, "stale-turn", "inProgress")
	emitCodexContextNote(old, "thread/tokenUsage/updated", map[string]any{"threadId": "context-thread", "turnId": "stale-turn", "tokenUsage": map[string]any{"last": map[string]any{"totalTokens": 80}}})
	if out := readCodexContext(t, a); out.Snapshot != nil || out.CanCompact || a.threads["context-thread"].turnID != "" {
		t.Fatal("retired runtime altered context", out)
	}
	if len(f.requests) != 0 {
		t.Fatal(f.requests)
	}
}

func TestCodexCompactAcknowledgementCASAndConcurrentReceipts(t *testing.T) {
	a, f, rec := newCodexContextFixture(t)
	before := readCodexContext(t, a)
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := a.CompactContext(context.Background(), "context-thread", before.Revision, contextOperation, nil)
			if err != nil || out.Compaction == nil || out.Compaction.State != "requested" || !out.Compaction.Blocking || out.CanCompact || out.Revision == before.Revision {
				t.Errorf("compact acknowledgement: %+v %v", out, err)
			}
		}()
	}
	wg.Wait()
	if f.count("config/read") != 1 || f.count("thread/compact/start") != 1 {
		t.Fatal("concurrent compact duplicated native work", f.requests)
	}
	params := f.requests[1]["params"].(map[string]any)
	if !reflect.DeepEqual(params, map[string]any{"threadId": "context-thread"}) {
		t.Fatal("compact changed permissions/model or used text input", params)
	}
	for _, revision := range []string{before.Revision, readCodexContext(t, a).Revision} {
		if _, err := a.CompactContext(context.Background(), "context-thread", revision, otherContextOperation, nil); !errors.Is(err, errCodexContextStale) {
			t.Fatal("busy/stale compact accepted", err)
		}
	}
	if err := a.SendWithOptions(context.Background(), "context-thread", "must wait", TurnOptions{}); !errors.Is(err, errCodexCompactionBusy) {
		t.Fatal("send bypassed pending compaction", err)
	}
	a.mu.Lock()
	active := a.hasActiveTurnsLocked()
	a.mu.Unlock()
	if !active || f.count("thread/compact/start") != 1 || f.count("turn/start") != 0 {
		t.Fatal("pending compaction did not retain worker ownership")
	}
	emitCodexContextTurn(a.conn, "compact-turn", "inProgress")
	emitCodexCompactionItem(a.conn, "compact-turn", "compact-item", false)
	if out := readCodexContext(t, a); out.Compaction.State != "running" || !out.Compaction.Blocking || out.CanCompact {
		t.Fatal(out)
	}
	started := rec.all()
	var progress protocol.Event
	for _, event := range started {
		if event.Type == "notice" && event.NoticeKind == "context_compaction" && event.Progress {
			progress = event
		}
	}
	if progress.ID == "" || progress.Text != "正在压缩上下文" || rec.count(func(e protocol.Event) bool { return e.NoticeKind == "context_compaction" && e.Progress }) != 1 {
		t.Fatal("native Codex start was not represented by one progress row", started)
	}
	emitCodexCompactionItem(a.conn, "compact-turn", "compact-item", false)
	if rec.count(func(e protocol.Event) bool { return e.NoticeKind == "context_compaction" && e.Progress }) != 1 {
		t.Fatal("replayed item/started duplicated progress", rec.all())
	}
	emitCodexCompactionItem(a.conn, "compact-turn", "compact-item", true)
	if rec.count(func(e protocol.Event) bool {
		return e.NoticeKind == "context_compaction" && e.ID == progress.ID && !e.Progress && e.Text == "上下文已压缩"
	}) != 1 {
		t.Fatal("native Codex completion did not update its progress row", rec.all())
	}
	if out := readCodexContext(t, a); out.Compaction.State != "completed" || !out.Compaction.Blocking || out.CanCompact {
		t.Fatal("item completion released the native turn early", out)
	}
	if err := a.SendWithOptions(context.Background(), "context-thread", "must still wait", TurnOptions{}); !errors.Is(err, errCodexCompactionBusy) {
		t.Fatal(err)
	}
	emitCodexContextTurn(a.conn, "compact-turn", "completed")
	after := readCodexContext(t, a)
	if after.Compaction.State != "completed" || after.Compaction.Blocking || !after.CanCompact || after.Snapshot != nil {
		t.Fatal(after)
	}
	emitCodexContextNote(a.conn, "thread/compacted", map[string]any{"threadId": "context-thread", "turnId": "compact-turn"})
	if replay := readCodexContext(t, a); replay.Compaction.OperationID != contextOperation || replay.Compaction.State != "completed" {
		t.Fatal("deprecated duplicate replaced operation ownership", replay)
	}
	if _, err := a.CompactContext(context.Background(), "context-thread", before.Revision, otherContextOperation, nil); !errors.Is(err, errCodexContextStale) {
		t.Fatal("completed compact allowed a second click with old revision", err)
	}
}

func TestCodexCompactCorrelatesNativeTurnItemAndLateAcknowledgement(t *testing.T) {
	a, f, _ := newCodexContextFixture(t)
	before := readCodexContext(t, a)
	f.hook = func(method string) {
		if method != "thread/compact/start" {
			return
		}
		emitCodexContextTurn(a.conn, "compact-turn", "inProgress")
		emitCodexCompactionItem(a.conn, "foreign-turn", "foreign-item", true)
		if out := readCodexContext(t, a); out.Compaction.State != "requested" {
			t.Fatal("foreign turn satisfied compact", out)
		}
		emitCodexCompactionItem(a.conn, "compact-turn", "compact-item", false)
		emitCodexCompactionItem(a.conn, "compact-turn", "different-item", true)
		if out := readCodexContext(t, a); out.Compaction.State != "running" {
			t.Fatal("foreign item satisfied compact", out)
		}
		emitCodexCompactionItem(a.conn, "compact-turn", "compact-item", true)
		emitCodexContextTurn(a.conn, "compact-turn", "completed")
	}
	out, err := a.CompactContext(context.Background(), "context-thread", before.Revision, contextOperation, nil)
	if err != nil || out.Compaction.State != "completed" || !out.CanCompact {
		t.Fatal("late acknowledgement revived finished operation", out, err)
	}
	out, err = a.CompactContext(context.Background(), "context-thread", before.Revision, contextOperation, nil)
	if err != nil || out.Compaction.State != "completed" || out.Compaction.Blocking || f.count("thread/compact/start") != 1 {
		t.Fatal("completed receipt was sent again", out, err)
	}
}

func TestCodexCompactRechecksOwnershipStateAndConsentBeforeWrite(t *testing.T) {
	for _, change := range []string{"consent", "turn", "permission", "connection", "configuration"} {
		t.Run(change, func(t *testing.T) {
			a, f, _ := newCodexContextFixture(t)
			before := readCodexContext(t, a)
			revoked := false
			f.hook = func(method string) {
				if method != "config/read" {
					return
				}
				if change == "turn" {
					emitCodexContextTurn(a.conn, "new-turn", "inProgress")
					return
				}
				a.mu.Lock()
				defer a.mu.Unlock()
				switch change {
				case "consent":
					revoked = true
				case "permission":
					a.threads["context-thread"].permissionUnconfirmed = true
				case "connection":
					a.conn = nil
					a.markUnloadedLocked()
				case "configuration":
					a.connSig = "different-settings"
				}
			}
			_, err := a.CompactContext(context.Background(), "context-thread", before.Revision, contextOperation, func() error {
				if revoked {
					return errors.New("fixture permission revoked")
				}
				return nil
			})
			if err == nil || f.count("config/read") != 1 || f.count("thread/compact/start") != 0 {
				t.Fatal("late source/consent change was dispatched", f.requests, err)
			}
		})
	}
}

func TestCodexCompactRejectedAndUncertainRequestsNeverReplay(t *testing.T) {
	for _, failure := range []string{"unsupported", "timeout", "null-ack", "exit"} {
		t.Run(failure, func(t *testing.T) {
			a, f, _ := newCodexContextFixture(t)
			before := readCodexContext(t, a)
			ctx := context.Background()
			if failure == "unsupported" {
				f.rejection = &rpcError{Code: -32601, Message: "synthetic missing method"}
			} else if failure == "timeout" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 15*time.Millisecond)
				defer cancel()
				f.holdReply = true
			} else if failure == "null-ack" {
				f.nullReply = true
			} else {
				f.hook = func(method string) {
					if method == "thread/compact/start" {
						a.mu.Lock()
						a.conn = nil
						a.markUnloadedLocked()
						a.mu.Unlock()
					}
				}
			}
			out, err := a.CompactContext(ctx, "context-thread", before.Revision, contextOperation, nil)
			if failure == "exit" {
				out = readCodexContext(t, a)
			}
			want := "unconfirmed"
			if failure == "unsupported" {
				want = "failed"
			}
			if err == nil || out.Compaction == nil || out.Compaction.State != want || out.CanCompact {
				t.Fatal(out, err)
			}
			if failure != "unsupported" && protocol.ExecutionErrorCode(err) != "command_delivery_uncertain" {
				t.Fatal("uncertain delivery became a safe retry", err)
			}
			_, _ = a.CompactContext(context.Background(), "context-thread", before.Revision, contextOperation, nil)
			if f.count("thread/compact/start") != 1 {
				t.Fatal("unsafe replay", f.requests)
			}
			if failure == "timeout" {
				f.holdReply = false
				emitCodexContextTurn(a.conn, "compact-turn", "inProgress")
				emitCodexCompactionItem(a.conn, "compact-turn", "compact-item", false)
				emitCodexCompactionItem(a.conn, "compact-turn", "compact-item", true)
				emitCodexContextTurn(a.conn, "compact-turn", "completed")
				if resolved, replayErr := a.CompactContext(context.Background(), "context-thread", before.Revision, contextOperation, nil); replayErr != nil || resolved.Compaction.State != "completed" {
					t.Fatal("native completion did not resolve lost acknowledgement", resolved, replayErr)
				}
			}
		})
	}
}

func TestCodexCompactOldAcknowledgementCannotClearReplacementRuntimeContext(t *testing.T) {
	a, f, rec := newCodexContextFixture(t)
	before := readCodexContext(t, a)
	old := a.conn
	var replacement *rpcConn
	f.hook = func(method string) {
		if method != "thread/compact/start" {
			return
		}
		a.mu.Lock()
		a.conn = nil
		a.markUnloadedLocked()
		replacement = &rpcConn{pending: map[int64]chan rpcResult{}, done: make(chan struct{})}
		a.conn = replacement
		a.threads["context-thread"].loaded = true
		a.mu.Unlock()
		a.installCodexNotificationHandler(replacement)
		emitCodexContextTurn(replacement, "replacement-turn", "inProgress")
		emitCodexContextNote(replacement, "thread/tokenUsage/updated", map[string]any{"threadId": "context-thread", "turnId": "replacement-turn", "tokenUsage": map[string]any{"last": map[string]any{"totalTokens": 77}, "modelContextWindow": 700}})
	}
	_, err := a.CompactContext(context.Background(), "context-thread", before.Revision, contextOperation, nil)
	if protocol.ExecutionErrorCode(err) != "command_delivery_uncertain" {
		t.Fatal(err)
	}
	out := readCodexContext(t, a)
	if out.Snapshot == nil || out.Snapshot.UsedTokens != 77 || *out.Snapshot.WindowTokens != 700 {
		t.Fatal("old reply erased replacement context", out)
	}
	count := len(rec.all())
	emitCodexContextTurn(old, "old-compact-turn", "completed")
	emitCodexCompactionItem(old, "replacement-turn", "foreign-old-item", true)
	if after := readCodexContext(t, a); after.Revision != out.Revision || after.Snapshot == nil || after.Snapshot.UsedTokens != 77 || len(rec.all()) != count {
		t.Fatal("old runtime notification altered replacement", after)
	}
}

func TestCodexCompactRejectsUnloadedUnknownPermissionAndActiveChildren(t *testing.T) {
	for _, change := range []string{"unloaded", "permission", "unknown-status", "child", "approval", "closed"} {
		t.Run(change, func(t *testing.T) {
			a, f, _ := newCodexContextFixture(t)
			before := readCodexContext(t, a)
			a.mu.Lock()
			thread := a.threads["context-thread"]
			switch change {
			case "unloaded":
				thread.loaded = false
			case "permission":
				thread.info.PermissionMode = ""
			case "unknown-status":
				thread.statusUnconfirmed = true
			case "child":
				thread.childStates = map[string]string{"child": "pendingInit"}
			case "approval":
				thread.info.Status = "waiting_approval"
			case "closed":
				a.closed = true
			}
			a.mu.Unlock()
			_, err := a.CompactContext(context.Background(), "context-thread", before.Revision, contextOperation, nil)
			if !errors.Is(err, errCodexContextStale) || len(f.requests) != 0 {
				t.Fatal("unsafe compact issued a native request", f.requests, err)
			}
			if change == "closed" {
				a.mu.Lock()
				a.closed = false
				a.mu.Unlock()
			}
		})
	}
}

func TestCodexContextAutomaticCompactionAndNativeFailure(t *testing.T) {
	a, _, _ := newCodexContextFixture(t)
	emitCodexContextTurn(a.conn, "ordinary-turn", "inProgress")
	emitCodexCompactionItem(a.conn, "ordinary-turn", "automatic-item", false)
	if out := readCodexContext(t, a); out.Compaction.State != "running" || out.Compaction.OperationID != "" || out.CanCompact {
		t.Fatal(out)
	}
	emitCodexCompactionItem(a.conn, "ordinary-turn", "automatic-item", true)
	emitCodexContextTurn(a.conn, "ordinary-turn", "completed")
	before := readCodexContext(t, a)
	if !before.CanCompact || before.Compaction.State != "completed" {
		t.Fatal(before)
	}
	if _, err := a.CompactContext(context.Background(), "context-thread", before.Revision, contextOperation, nil); err != nil {
		t.Fatal(err)
	}
	emitCodexContextTurn(a.conn, "manual-turn", "inProgress")
	emitCodexCompactionItem(a.conn, "manual-turn", "manual-item", false)
	emitCodexContextNote(a.conn, "error", map[string]any{"threadId": "context-thread", "turnId": "manual-turn", "willRetry": false, "error": map[string]any{"message": "synthetic failure"}})
	if out := readCodexContext(t, a); out.Compaction.State != "failed" || out.CanCompact {
		t.Fatal("error prematurely released operation", out)
	}
	emitCodexContextTurn(a.conn, "manual-turn", "failed")
	if out := readCodexContext(t, a); out.Compaction.State != "failed" || !out.CanCompact {
		t.Fatal("native failure did not release operation", out)
	}
}

func TestCodexCompactStartedItemAndSuccessfulTurnDoNotInventCompletion(t *testing.T) {
	a, _, _ := newCodexContextFixture(t)
	before := readCodexContext(t, a)
	if _, err := a.CompactContext(context.Background(), "context-thread", before.Revision, contextOperation, nil); err != nil {
		t.Fatal(err)
	}
	emitCodexContextTurn(a.conn, "compact-turn", "inProgress")
	emitCodexCompactionItem(a.conn, "compact-turn", "compact-item", false)
	emitCodexContextTurn(a.conn, "compact-turn", "completed")
	if out := readCodexContext(t, a); out.Compaction.State != "unconfirmed" || out.Compaction.Blocking || !out.CanCompact {
		t.Fatal("a started item was treated as completed", out)
	}
}

func TestCodexContextCompletedTurnReplayCannotReplaceCurrentTurn(t *testing.T) {
	a, _, rec := newCodexContextFixture(t)
	for _, id := range []string{"older-turn", "previous-turn"} {
		emitCodexContextTurn(a.conn, id, "inProgress")
		emitCodexContextTurn(a.conn, id, "completed")
	}
	emitCodexContextTurn(a.conn, "current-turn", "inProgress")
	emitCodexContextNote(a.conn, "thread/tokenUsage/updated", map[string]any{"threadId": "context-thread", "turnId": "current-turn", "tokenUsage": map[string]any{"last": map[string]any{"totalTokens": 80}, "modelContextWindow": 1000}})
	before := readCodexContext(t, a)
	count := len(rec.all())
	for _, id := range []string{"older-turn", "previous-turn"} {
		emitCodexContextTurn(a.conn, id, "inProgress")
		emitCodexContextTurn(a.conn, id, "completed")
	}
	if out := readCodexContext(t, a); out.Revision != before.Revision || out.Snapshot == nil || out.Snapshot.UsedTokens != 80 || len(rec.all()) != count || a.threads["context-thread"].turnID != "current-turn" {
		t.Fatal("old turn replay replaced the active context", out)
	}
	emitCodexContextTurn(a.conn, "current-turn", "completed")
	before = readCodexContext(t, a)
	count = len(rec.all())
	emitCodexContextTurn(a.conn, "older-turn", "inProgress")
	emitCodexContextTurn(a.conn, "older-turn", "completed")
	if out := readCodexContext(t, a); out.Revision != before.Revision || len(rec.all()) != count || a.threads["context-thread"].turnID != "" || a.threads["context-thread"].info.LastTerminalTurnID != "current-turn" {
		t.Fatal("old turn replay revived after the successor finished", out)
	}
}

func TestCodexCompactDeadlineRetainsNativeBusyUntilRealCompletion(t *testing.T) {
	for _, progress := range []string{"requested", "running"} {
		t.Run(progress, func(t *testing.T) {
			a, f, rec := newCodexContextFixture(t)
			before := readCodexContext(t, a)
			if _, err := a.CompactContext(context.Background(), "context-thread", before.Revision, contextOperation, nil); err != nil {
				t.Fatal(err)
			}
			if progress == "running" {
				emitCodexContextTurn(a.conn, "compact-turn", "inProgress")
				emitCodexCompactionItem(a.conn, "compact-turn", "compact-item", false)
			}
			a.mu.Lock()
			thread := a.threads["context-thread"]
			op, conn := thread.context.compaction, a.conn
			done := op.done
			other := a.threadLocked("other-context-thread")
			other.loaded, other.turnID, other.info.Status = true, "other-turn", "running"
			a.mu.Unlock()
			pending := readCodexContext(t, a)
			count := len(rec.all())
			a.expireCodexCompaction(thread, op)
			out := readCodexContext(t, a)
			expectedDeadlineEvents := 1 // Context state changed.
			if progress == "running" {
				expectedDeadlineEvents++ // The existing progress row also receives its unconfirmed terminal update.
			}
			if out.Compaction.State != "unconfirmed" || !out.Compaction.Blocking || out.CanCompact || out.UnavailableReason != "context_compaction_busy" || out.Revision == pending.Revision || len(rec.all()) != count+expectedDeadlineEvents {
				t.Fatalf("deadline did not invalidate progress certainty: context=%+v events=%d want=%d", *out.Compaction, len(rec.all())-count, expectedDeadlineEvents)
			}
			select {
			case <-done:
			default:
				t.Fatal("expired timer was not released")
			}
			a.mu.Lock()
			retained := a.conn == conn && conn.alive() && !op.terminal && thread.codexCompactionBusyLocked() && other.turnID == "other-turn" && other.loaded
			a.mu.Unlock()
			if !retained {
				t.Fatal("deadline retired native work or another thread")
			}
			if replay, err := a.CompactContext(context.Background(), "context-thread", before.Revision, contextOperation, nil); err != nil || replay.Compaction.State != "unconfirmed" || !replay.Compaction.Blocking {
				t.Fatal("deadline lost the stable operation receipt", replay, err)
			}
			if _, err := a.CompactContext(context.Background(), "context-thread", out.Revision, otherContextOperation, nil); !errors.Is(err, errCodexContextStale) {
				t.Fatal("deadline enabled a second compact", err)
			}
			if err := a.SendWithOptions(context.Background(), "context-thread", "must wait", TurnOptions{}); !errors.Is(err, errCodexCompactionBusy) {
				t.Fatal("deadline enabled a send", err)
			}
			if f.count("thread/compact/start") != 1 || f.count("turn/start") != 0 {
				t.Fatal("deadline retried native work", f.requests)
			}
			if progress == "requested" {
				emitCodexContextTurn(conn, "compact-turn", "inProgress")
			}
			lateStartEventCount := len(rec.all())
			emitCodexCompactionItem(conn, "compact-turn", "compact-item", false)
			if out := readCodexContext(t, a); out.Compaction.State != "unconfirmed" || len(rec.all()) != lateStartEventCount {
				t.Fatalf("late start revived progress: context=%+v events=%d want=%d", *out.Compaction, len(rec.all())-lateStartEventCount, 0)
			}
			emitCodexCompactionItem(conn, "compact-turn", "compact-item", true)
			completed := readCodexContext(t, a)
			count = len(rec.all())
			a.expireCodexCompaction(thread, op)
			emitCodexCompactionItem(conn, "compact-turn", "compact-item", false)
			if out := readCodexContext(t, a); out.Compaction.State != "completed" || !out.Compaction.Blocking || out.CanCompact || out.Revision != completed.Revision || len(rec.all()) != count {
				t.Fatalf("late timer/start replaced native completion: context=%+v events=%d", *out.Compaction, len(rec.all())-count)
			}
			emitCodexContextTurn(conn, "compact-turn", "completed")
			if out := readCodexContext(t, a); out.Compaction.State != "completed" || out.Compaction.Blocking || !out.CanCompact {
				t.Fatal("native terminal did not resolve timed-out operation", out)
			}
		})
	}
}

func TestCodexCompactReplacementWorkerKeepsReceiptsWithoutBusyView(t *testing.T) {
	a, oldTransport, rec := newCodexContextFixture(t)
	before := readCodexContext(t, a)
	if _, err := a.CompactContext(context.Background(), "context-thread", before.Revision, contextOperation, nil); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	thread, old := a.threads["context-thread"], a.conn
	op := thread.context.compaction
	a.mu.Unlock()
	a.onExit(old)
	if out := readCodexContext(t, a); out.Compaction == nil || out.Compaction.State != "unconfirmed" || out.Compaction.Blocking || out.CanCompact {
		t.Fatal("worker exit discarded uncertainty", out)
	}
	replacement := &rpcConn{pending: map[int64]chan rpcResult{}, done: make(chan struct{})}
	f := &codexContextTransport{conn: replacement}
	replacement.stdin = f
	a.mu.Lock()
	a.conn, thread.loaded = replacement, true
	a.mu.Unlock()
	a.installCodexNotificationHandler(replacement)
	current := readCodexContext(t, a)
	if current.Compaction != nil || current.Snapshot != nil || !current.CanCompact || current.Revision == before.Revision {
		t.Fatal("old worker's uncertainty polluted the new worker", current)
	}
	if receipt, err := a.CompactContext(context.Background(), "context-thread", before.Revision, contextOperation, nil); !errors.Is(err, errRPCDeliveryInterrupted) || receipt.Compaction == nil || receipt.Compaction.State != "unconfirmed" || receipt.Compaction.Blocking {
		t.Fatal("old operation receipt was lost", receipt, err)
	}
	if f.count("thread/compact/start") != 0 || oldTransport.count("thread/compact/start") != 1 {
		t.Fatal("old operation was dispatched on the replacement worker")
	}
	if _, err := a.CompactContext(context.Background(), "context-thread", current.Revision, otherContextOperation, nil); err != nil {
		t.Fatal("new worker could not accept a new operation", err)
	}
	if receipt, err := a.CompactContext(context.Background(), "context-thread", before.Revision, contextOperation, nil); !errors.Is(err, errRPCDeliveryInterrupted) || receipt.Compaction == nil || receipt.Compaction.Blocking {
		t.Fatal("old receipt inherited the replacement operation's busy lease", receipt, err)
	}
	current = readCodexContext(t, a)
	count := len(rec.all())
	a.expireCodexCompaction(thread, op)
	emitCodexContextTurn(old, "old-compact-turn", "inProgress")
	emitCodexCompactionItem(old, "old-compact-turn", "old-compact-item", true)
	if out := readCodexContext(t, a); out.Revision != current.Revision || out.Compaction.OperationID != otherContextOperation || out.Compaction.State != "requested" || !out.Compaction.Blocking || len(rec.all()) != count || f.count("thread/compact/start") != 1 {
		t.Fatal("old worker's deadline/event altered the replacement operation", out)
	}
	a.mu.Lock()
	retained := thread.context.receipts[contextOperation] == op && len(thread.context.receipts) == 2
	a.mu.Unlock()
	if !retained {
		t.Fatal("replacement lost bounded operation receipts")
	}
}

func TestCodexContextExitInvalidatesSnapshotAndUnstartedCompaction(t *testing.T) {
	for _, compact := range []bool{false, true} {
		t.Run(map[bool]string{false: "snapshot", true: "requested"}[compact], func(t *testing.T) {
			a, _, rec := newCodexContextFixture(t)
			emitCodexContextTurn(a.conn, "previous-turn", "inProgress")
			emitCodexContextUsage(a.conn, "previous-turn", 80, 1000)
			emitCodexContextTurn(a.conn, "previous-turn", "completed")
			before := readCodexContext(t, a)
			if compact {
				var err error
				before, err = a.CompactContext(context.Background(), "context-thread", before.Revision, contextOperation, nil)
				if err != nil {
					t.Fatal(err)
				}
			}
			count := len(rec.all())
			a.onExit(a.conn)
			out := readCodexContext(t, a)
			events := rec.all()[count:]
			if out.Snapshot != nil || out.CanCompact || out.Revision == before.Revision || out.UnavailableReason != "runtime_not_loaded" || len(events) != 1 || events[0].Type != "context.updated" || events[0].Context == nil || events[0].Context.Revision != out.Revision {
				t.Fatal("idle worker exit did not emit context invalidation", out, events)
			}
			if compact && (out.Compaction == nil || out.Compaction.State != "unconfirmed" || out.Compaction.Blocking) {
				t.Fatal("exit invented an unstarted compaction result", out)
			}
		})
	}
}

func TestCodexContextCompactionSuppressesLateUsageUntilNextTurn(t *testing.T) {
	for _, itemObserved := range []bool{false, true} {
		t.Run(map[bool]string{false: "unconfirmed", true: "completed"}[itemObserved], func(t *testing.T) {
			a, _, _ := newCodexContextFixture(t)
			emitCodexContextTurn(a.conn, "previous-turn", "inProgress")
			emitCodexContextUsage(a.conn, "previous-turn", 900, 1000)
			emitCodexContextTurn(a.conn, "previous-turn", "completed")
			before := readCodexContext(t, a)
			if _, err := a.CompactContext(context.Background(), "context-thread", before.Revision, contextOperation, nil); err != nil {
				t.Fatal(err)
			}
			emitCodexContextUsage(a.conn, "previous-turn", 950, 1000)
			if out := readCodexContext(t, a); out.Snapshot != nil {
				t.Fatal("late pre-compaction usage repopulated the requested snapshot", out)
			}
			emitCodexContextTurn(a.conn, "compact-turn", "inProgress")
			if itemObserved {
				emitCodexCompactionItem(a.conn, "compact-turn", "compact-item", false)
				emitCodexCompactionItem(a.conn, "compact-turn", "compact-item", true)
			}
			emitCodexContextUsage(a.conn, "compact-turn", 960, 1000)
			emitCodexContextTurn(a.conn, "compact-turn", "completed")
			emitCodexContextUsage(a.conn, "compact-turn", 970, 1000)
			if out := readCodexContext(t, a); out.Snapshot != nil {
				t.Fatal("ambiguous compact-turn usage restored an old context value", out)
			}
			emitCodexContextTurn(a.conn, "next-turn", "inProgress")
			emitCodexContextUsage(a.conn, "next-turn", 40, nil)
			if out := readCodexContext(t, a); out.Snapshot == nil || out.Snapshot.UsedTokens != 40 || out.Snapshot.WindowTokens != nil || out.Snapshot.Model != "" {
				t.Fatal("fresh native observation was missing or guessed a model window", out)
			}
		})
	}
}
