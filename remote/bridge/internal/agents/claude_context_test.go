package agents

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/protocol"
)

type claudeContextFixture struct {
	a           *claudeAgent
	p           *claudeProc
	mu          sync.Mutex
	version     string
	builtin     bool
	usage       json.RawMessage
	controls    []string
	users       []map[string]any
	onControl   func(string)
	onUser      func(map[string]any) (int, error)
	ignoreUsage bool
	key         atomic.Value
}

func newClaudeContextFixture(t *testing.T) (*claudeContextFixture, *recorder) {
	t.Helper()
	a, p, rec := newClaudeResultFixture(t)
	f := &claudeContextFixture{a: a, p: p, version: "2.1.300", builtin: true,
		usage: json.RawMessage(`{"totalTokens":1250,"rawMaxTokens":200000,"model":"claude-fixture"}`)}
	f.key.Store("synthetic-context-key")
	a.settings = func() Settings {
		return Settings{Approval: "ask", ClaudeRoot: "http://127.0.0.1:1", ClaudeKey: f.key.Load().(string)}
	}
	p.workerSettingsSig = claudeSettingsSig(a.settings(), a.exeOverride)
	p.stdin = &claudeDeliveryFixtureWriter{write: func(data []byte) (int, error) {
		var frame map[string]any
		if err := json.Unmarshal(data, &frame); err != nil {
			t.Fatal(err)
		}
		if frame["type"] == "user" {
			f.mu.Lock()
			f.users = append(f.users, frame)
			callback := f.onUser
			f.mu.Unlock()
			if callback != nil {
				n, err := callback(frame)
				if n >= 0 {
					return n, err
				}
			}
			return len(data), nil
		}
		request := frame["request"].(map[string]any)
		kind := request["subtype"].(string)
		f.mu.Lock()
		f.controls = append(f.controls, kind)
		callback, version, builtin, usage, ignore := f.onControl, f.version, f.builtin, append(json.RawMessage(nil), f.usage...), f.ignoreUsage
		f.mu.Unlock()
		if callback != nil {
			callback(kind)
		}
		var payload any
		switch kind {
		case "initialize":
			payload = map[string]any{"claude_code_version": version, "commands": []map[string]any{{"name": "compact", "builtin": builtin}}}
		case "get_binary_version":
			payload = map[string]any{"version": version}
		case "get_context_usage":
			if request["detail"] != "summary" {
				t.Error("context read requested a full token-count report")
			}
			if ignore {
				return len(data), nil
			}
			payload = usage
		default:
			t.Errorf("unexpected context control %q", kind)
			payload = map[string]any{}
		}
		response, _ := json.Marshal(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": frame["request_id"], "response": payload}})
		a.handleLine(p, response)
		return len(data), nil
	}}
	return f, rec
}

func (f *claudeContextFixture) context(t *testing.T) protocol.SessionContext {
	t.Helper()
	view, err := f.a.SessionContext(context.Background(), f.p.id, nil)
	if err != nil {
		t.Fatal(err)
	}
	return view
}

func (f *claudeContextFixture) compact(t *testing.T, view protocol.SessionContext, operation string) protocol.SessionContext {
	t.Helper()
	got, err := f.a.CompactContext(context.Background(), f.p.id, view.Revision, operation, nil)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func (f *claudeContextFixture) frame(value map[string]any) {
	value["session_id"] = f.p.id
	raw, _ := json.Marshal(value)
	f.a.handleLine(f.p, raw)
}

func (f *claudeContextFixture) result(uuid string, boundary bool, errorResult bool) {
	f.mu.Lock()
	clientUUID := f.users[len(f.users)-1]["uuid"]
	f.mu.Unlock()
	if boundary {
		f.frame(map[string]any{"type": "system", "subtype": "compact_boundary", "uuid": "boundary-" + uuid, "compact_metadata": map[string]any{"trigger": "manual", "pre_tokens": 1250, "post_tokens": 500}})
	}
	f.frame(map[string]any{"type": "result", "subtype": "success", "is_error": errorResult, "uuid": uuid, "user_message_uuid": clientUUID, "local_command": "compact"})
}

func TestClaudeContextUsesOnlyNativeSummaryAndKeepsUnknownWindow(t *testing.T) {
	f, _ := newClaudeContextFixture(t)
	view := f.context(t)
	if view.Snapshot == nil || view.Snapshot.UsedTokens != 1250 || view.Snapshot.WindowTokens == nil || *view.Snapshot.WindowTokens != 200000 || !view.Snapshot.Estimated || view.Snapshot.Basis != "runtime_estimate" || view.Snapshot.WindowKind != "effective" || view.Snapshot.Source != "claude_context_usage" || !view.CanCompact || view.Revision == "" {
		t.Fatalf("wrong native context snapshot: %+v", view)
	}
	view.Snapshot.UsedTokens = 999
	*view.Snapshot.WindowTokens = 1
	if current := f.context(t); current.Snapshot.UsedTokens != 1250 || *current.Snapshot.WindowTokens != 200000 {
		t.Fatal("snapshot aliases internal state")
	}
	f.mu.Lock()
	f.usage = json.RawMessage(`{"totalTokens":210000,"rawMaxTokens":200000,"model":"claude-fixture"}`)
	f.mu.Unlock()
	if current := f.context(t); current.Snapshot.UsedTokens != 210000 {
		t.Fatal("over-limit usage was clamped")
	}
	f.mu.Lock()
	f.usage = json.RawMessage(`{"totalTokens":500,"model":"claude-fixture"}`)
	f.mu.Unlock()
	if current := f.context(t); current.Snapshot.WindowTokens != nil {
		t.Fatal("missing native window was guessed")
	}
}

func TestClaudeContextOldVersionSkipsFullReadButMayCompact(t *testing.T) {
	for _, version := range []string{"", "2.1.220", "2.1.+300", "2.1.0300", "malformed", "3.0.0"} {
		t.Run(version, func(t *testing.T) {
			f, _ := newClaudeContextFixture(t)
			f.version = version
			view := f.context(t)
			if view.Snapshot != nil || !view.CanCompact || view.UnavailableReason != "context_summary_unavailable" {
				t.Fatal("unknown summary was fabricated or independently confirmed compact was hidden")
			}
			for _, kind := range f.controls {
				if kind == "get_context_usage" {
					t.Fatal("old/unknown CLI received a potentially full context read")
				}
			}
			if compact := f.compact(t, view, "11111111-1111-4111-8111-111111111111"); compact.Compaction == nil || compact.Compaction.State != "running" || !compact.Compaction.Blocking {
				t.Fatal("confirmed builtin compact depends on summary support")
			}
			f.result("old-result", true, false)
		})
	}
}

func TestClaudeContextNeverUsesResultBillingAsOccupancy(t *testing.T) {
	f, _ := newClaudeContextFixture(t)
	f.version = "2.1.220"
	_ = f.context(t)
	f.frame(map[string]any{"type": "result", "subtype": "success", "uuid": "billing-only", "usage": map[string]any{"input_tokens": 40000, "output_tokens": 20000}, "modelUsage": map[string]any{"claude-fixture": map[string]any{"inputTokens": 999999, "contextWindow": 200000}}})
	if view := f.context(t); view.Snapshot != nil {
		t.Fatal("cumulative result usage became a context snapshot")
	}
}

func TestClaudeContextReadRechecksSourceAndCancellationWithoutStoppingWorker(t *testing.T) {
	for _, change := range []string{"source", "cancel", "timeout"} {
		t.Run(change, func(t *testing.T) {
			f, _ := newClaudeContextFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
			defer cancel()
			f.onControl = func(kind string) {
				if kind != "get_context_usage" {
					return
				}
				if change == "source" {
					f.key.Store("synthetic-other-source")
				}
				if change == "cancel" {
					cancel()
				}
			}
			f.ignoreUsage = change == "timeout"
			view, err := f.a.SessionContext(ctx, f.p.id, nil)
			if err == nil || view.Snapshot != nil {
				t.Fatal("late native snapshot crossed cancellation/source boundary")
			}
			if !f.a.writeWorkerActive(f.p) {
				t.Fatal("read cancellation/timeout killed the worker")
			}
		})
	}
}

func TestClaudeCompactRequiresBuiltinCASAndIdleWorker(t *testing.T) {
	for _, condition := range []string{"skill-named-compact", "stale", "running", "subtask", "permission"} {
		t.Run(condition, func(t *testing.T) {
			f, _ := newClaudeContextFixture(t)
			view := f.context(t)
			var guard func() error
			switch condition {
			case "skill-named-compact":
				f.builtin = false
			case "stale":
				view.Revision = "stale-revision"
			case "running":
				f.p.mu.Lock()
				f.p.turns = 1
				f.p.mu.Unlock()
			case "subtask":
				f.p.activeSubtasks.Store(true)
			case "permission":
				guard = func() error { return errors.New("synthetic permission revoked") }
			}
			if _, err := f.a.CompactContext(context.Background(), f.p.id, view.Revision, "11111111-1111-4111-8111-111111111111", guard); err == nil {
				t.Fatal("invalid compact was admitted")
			}
			if len(f.users) != 0 {
				t.Fatal("invalid compact wrote a user prompt")
			}
		})
	}
}

func TestClaudeCompactNativeBoundaryAndTerminalOwnCompletion(t *testing.T) {
	f, rec := newClaudeContextFixture(t)
	view := f.context(t)
	var requested protocol.SessionContext
	f.onUser = func(map[string]any) (int, error) { requested = f.context(t); return -1, nil }
	accepted := f.compact(t, view, "11111111-1111-4111-8111-111111111111")
	if requested.Compaction == nil || requested.Compaction.State != "requested" || !requested.Compaction.Blocking {
		t.Fatal("reserved compact did not expose its runtime occupancy")
	}
	if accepted.Compaction == nil || accepted.Compaction.State != "running" || !accepted.Compaction.Blocking || accepted.CanCompact || accepted.Snapshot != nil {
		t.Fatal("dispatch claimed completion or retained old snapshot")
	}
	if got := f.users[0]["message"].(map[string]any)["content"]; got != "/compact" {
		t.Fatal("wrong native command dispatch")
	}
	started := rec.wait(t, "manual compaction progress notice", func(e protocol.Event) bool {
		return e.Type == "notice" && e.NoticeKind == "context_compaction" && e.Progress && e.Text == "正在压缩上下文"
	})
	for range 2 {
		if duplicate := f.compact(t, view, "11111111-1111-4111-8111-111111111111"); duplicate.Compaction.State != "running" || !duplicate.Compaction.Blocking {
			t.Fatal("duplicate receipt changed")
		}
	}
	if len(f.users) != 1 {
		t.Fatal("duplicate compact was resent")
	}
	if err := f.a.Send(context.Background(), f.p.id, "must not queue during compact"); !errors.Is(err, errClaudeContextBusy) {
		t.Fatalf("compact did not exclude new turns: %v", err)
	}
	if err := f.a.retireIdleClaudeWorker(f.p); err == nil {
		t.Fatal("compacting worker was treated as idle")
	}
	f.frame(map[string]any{"type": "system", "subtype": "compact_boundary", "uuid": "actual-boundary", "compact_metadata": map[string]any{"trigger": "manual"}})
	if state := f.a.claudeContextView(f.p); state.Compaction.State != "running" || !state.Compaction.Blocking {
		t.Fatal("boundary released busy before the command terminal")
	}
	f.result("compact-result", false, false)
	completed := f.a.claudeContextView(f.p)
	if completed.Compaction.State != "completed" || completed.Compaction.Blocking || !completed.CanCompact {
		t.Fatal("actual compact boundary/result did not complete operation")
	}
	if duplicate := f.compact(t, view, "11111111-1111-4111-8111-111111111111"); duplicate.Compaction.State != "completed" || duplicate.Compaction.Blocking || len(f.users) != 1 {
		t.Fatal("completed duplicate was replayed")
	}
	if count := rec.count(func(e protocol.Event) bool {
		return e.Type == "notice" && e.NoticeKind == "context_compaction" && e.ID == started.ID && !e.Progress && e.Text == "上下文已压缩"
	}); count != 1 {
		t.Fatalf("manual compaction progress was not updated in place: %+v", rec.all())
	}
	rec.wait(t, "context refresh after compact", func(e protocol.Event) bool {
		return e.Type == "context.updated" && e.Context != nil && e.Context.Compaction != nil && e.Context.Compaction.State == "completed" && !e.Context.Compaction.Blocking && e.Context.Snapshot != nil
	})
}

func TestClaudeAutomaticCompactionUsesOnlyNativeLifecycleFrames(t *testing.T) {
	f, rec := newClaudeContextFixture(t)
	initial := f.context(t)
	if initial.Compaction != nil {
		t.Fatal("context read fabricated compaction state", initial)
	}
	// Ordinary busy/status and assistant output are not compaction evidence.
	f.frame(map[string]any{"type": "system", "subtype": "status", "status": "busy", "uuid": "ordinary-status"})
	f.frame(map[string]any{"type": "assistant", "uuid": "ordinary-assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "long answer"}}}})
	if view := f.a.claudeContextView(f.p); view.Compaction != nil {
		t.Fatal("ordinary long work was classified as compaction", view)
	}
	start := map[string]any{"type": "system", "subtype": "informational", "content": "Compacting conversation…", "uuid": "auto-compact-start"}
	f.frame(start)
	running := f.a.claudeContextView(f.p)
	if running.Compaction == nil || running.Compaction.State != "running" || !running.Compaction.Blocking || running.CanCompact || running.Snapshot != nil {
		t.Fatal("native automatic compaction start was not admitted", running)
	}
	f.frame(start) // native replay must not create a second operation or notice
	progressID := ""
	for _, e := range rec.all() {
		if e.Type == "notice" && e.NoticeKind == "context_compaction" && e.Progress && e.Text == "正在压缩上下文" {
			progressID = e.ID
		}
	}
	if progressID == "" || rec.count(func(e protocol.Event) bool {
		return e.Type == "notice" && e.NoticeKind == "context_compaction" && e.Progress
	}) != 1 {
		t.Fatal("automatic start notice was missing or duplicated", rec.all())
	}
	f.frame(map[string]any{"type": "system", "subtype": "compact_boundary", "uuid": "auto-compact-boundary", "compact_metadata": map[string]any{"trigger": "auto"}})
	completed := f.a.claudeContextView(f.p)
	if completed.Compaction == nil || completed.Compaction.State != "completed" || completed.Compaction.Blocking || completed.Snapshot != nil {
		t.Fatal("native compaction boundary did not complete the operation", completed)
	}
	if rec.count(func(e protocol.Event) bool {
		return e.Type == "notice" && e.NoticeKind == "context_compaction" && e.ID == progressID && !e.Progress && e.Text == "上下文已压缩"
	}) != 1 {
		t.Fatal("automatic lifecycle notice did not replace its progress row", rec.all())
	}
}

func TestClaudeCompactBoundaryWithoutStartKeepsStartTimeUnknown(t *testing.T) {
	f, rec := newClaudeContextFixture(t)
	f.frame(map[string]any{"type": "system", "subtype": "compact_boundary", "uuid": "boundary-without-start", "compact_metadata": map[string]any{"trigger": "auto"}})

	view := f.a.claudeContextView(f.p)
	if view.Compaction == nil || view.Compaction.State != "completed" || view.Compaction.StartedAt != 0 || view.Compaction.UpdatedAt <= 0 {
		t.Fatal("a boundary without a start frame fabricated a start timestamp", view)
	}
	notices := rec.all()
	count := 0
	for _, event := range notices {
		if event.Type == "notice" && event.NoticeKind == "context_compaction" && !event.Progress && event.Text == "上下文已压缩" {
			count++
		}
	}
	if count != 1 {
		t.Fatal("the boundary did not produce one typed completion notice", notices)
	}
}

func TestClaudeForeignCompactionBoundaryDoesNotEmitCompletionNotice(t *testing.T) {
	f, rec := newClaudeContextFixture(t)
	f.frame(map[string]any{"type": "system", "subtype": "compact_boundary", "uuid": "foreign-boundary", "agent_id": "child", "compact_metadata": map[string]any{"trigger": "auto"}})

	for _, event := range rec.all() {
		if event.Type == "notice" && event.Text == "上下文已压缩" {
			t.Fatal("a foreign child boundary leaked a completion notice", event)
		}
	}
}

func TestClaudeAutomaticCompactionWithoutBoundaryBecomesUnconfirmed(t *testing.T) {
	f, rec := newClaudeContextFixture(t)
	f.frame(map[string]any{"type": "system", "subtype": "status", "status": "compacting", "uuid": "auto-start"})
	if view := f.a.claudeContextView(f.p); view.Compaction == nil || view.Compaction.State != "running" {
		t.Fatal("native compacting status was not recognized", view)
	}
	f.frame(map[string]any{"type": "result", "subtype": "success", "uuid": "ordinary-result"})
	view := f.a.claudeContextView(f.p)
	if view.Compaction == nil || view.Compaction.State != "unconfirmed" || view.Compaction.Blocking {
		t.Fatal("missing completion boundary was claimed as success or left blocking", view)
	}
	if rec.count(func(e protocol.Event) bool {
		return e.Type == "notice" && e.NoticeKind == "context_compaction" && !e.Progress && e.Text == "压缩结果未确认"
	}) != 1 {
		t.Fatal("unconfirmed automatic state did not retire the spinner", rec.all())
	}
}

func TestClaudeCompactSuccessWithoutBoundaryIsNotCompletion(t *testing.T) {
	for _, nativeError := range []bool{false, true} {
		f, _ := newClaudeContextFixture(t)
		view := f.context(t)
		f.compact(t, view, "11111111-1111-4111-8111-111111111111")
		f.result("no-boundary-result", false, nativeError)
		if got := f.a.claudeContextView(f.p); got.Compaction.State != "failed" || got.Compaction.Blocking {
			t.Fatal("ordinary success/error fabricated compaction")
		}
	}
}

func TestClaudeCompactSourceChangeDuringPreflightAndAfterDispatch(t *testing.T) {
	for _, afterDispatch := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-write", true: "after-write"}[afterDispatch], func(t *testing.T) {
			f, _ := newClaudeContextFixture(t)
			view := f.context(t)
			if !afterDispatch {
				f.onControl = func(kind string) {
					if kind == "initialize" {
						f.key.Store("synthetic-new-source")
					}
				}
			}
			_, err := f.a.CompactContext(context.Background(), f.p.id, view.Revision, "11111111-1111-4111-8111-111111111111", nil)
			if !afterDispatch {
				if err == nil || len(f.users) != 0 {
					t.Fatal("source changed during preflight but compact was written")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			f.key.Store("synthetic-new-source")
			f.result("source-result", true, false)
			if got := f.a.claudeContextView(f.p); got.Compaction.State != "unconfirmed" || got.Compaction.Blocking {
				t.Fatal("source change was reported as confirmed compact")
			}
		})
	}
}

func TestClaudeCompactCancellationAfterAdmissionDoesNotResend(t *testing.T) {
	f, _ := newClaudeContextFixture(t)
	view := f.context(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.onUser = func(map[string]any) (int, error) { cancel(); return -1, nil }
	got, err := f.a.CompactContext(ctx, f.p.id, view.Revision, "11111111-1111-4111-8111-111111111111", nil)
	if err != nil || got.Compaction.State != "running" || !got.Compaction.Blocking {
		t.Fatal("admitted compact was reclassified as unsent")
	}
	f.result("cancelled-caller-result", true, false)
	if duplicate := f.compact(t, view, "11111111-1111-4111-8111-111111111111"); duplicate.Compaction.State != "completed" || len(f.users) != 1 {
		t.Fatal("cancelled caller caused duplicate compaction")
	}
}

func TestClaudeCompactPartialWriteAndWorkerExitRemainUnconfirmed(t *testing.T) {
	for _, partial := range []bool{false, true} {
		f, rec := newClaudeContextFixture(t)
		view := f.context(t)
		if partial {
			f.onUser = func(map[string]any) (int, error) { return 3, io.ErrShortWrite }
		}
		got := f.compact(t, view, "11111111-1111-4111-8111-111111111111")
		if !partial {
			f.a.stop(f.p)
			// The observer event may be suppressed for a retired generation; wait
			// on the operation's completion receipt, not a transient UI callback.
			f.a.contextState.mu.Lock()
			done := f.a.contextState.sessions[f.p.id].operations["11111111-1111-4111-8111-111111111111"].done
			f.a.contextState.mu.Unlock()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("worker exit left compact pending")
			}
			got = f.a.claudeContextView(f.p)
		}
		if got.Compaction == nil || got.Compaction.State != "unconfirmed" || got.Compaction.Blocking || got.CanCompact {
			t.Fatal("uncertain delivery/exit was reported as safe completion")
		}
		_ = rec
	}
}

func TestClaudeContextValidationCallbackMayInspectAgentLocks(t *testing.T) {
	f, _ := newClaudeContextFixture(t)
	guard := func() error { f.a.mu.Lock(); f.p.mu.Lock(); f.p.mu.Unlock(); f.a.mu.Unlock(); return nil }
	view, err := f.a.SessionContext(context.Background(), f.p.id, guard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.CompactContext(context.Background(), f.p.id, view.Revision, "11111111-1111-4111-8111-111111111111", guard); err != nil {
		t.Fatal(err)
	}
	f.result("reentrant-result", true, false)
}

func TestClaudeContextWorkerReplacementCannotPublishOldSnapshot(t *testing.T) {
	f, _ := newClaudeContextFixture(t)
	f.onControl = func(kind string) {
		if kind != "get_context_usage" {
			return
		}
		next := claudeSyntheticPermissionWorker(t, f.a, f.p.id)
		next.mapper = newClaudeMapper("claude:"+next.id, next.cwd)
		next.workerSettingsSig = f.p.workerSettingsSig
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	view, err := f.a.SessionContext(ctx, f.p.id, nil)
	if err == nil || view.Snapshot != nil {
		t.Fatal("replaced worker published an old context snapshot")
	}
}

func TestClaudeContextLateCapabilitiesCannotResetReplacement(t *testing.T) {
	f, _ := newClaudeContextFixture(t)
	_ = f.context(t)
	next := claudeSyntheticPermissionWorker(t, f.a, f.p.id)
	next.workerSettingsSig = f.p.workerSettingsSig
	s, unlock, err := f.a.lockActiveClaudeContext(next)
	if err != nil {
		t.Fatal(err)
	}
	s.view.Snapshot = &protocol.ContextSnapshot{UsedTokens: 71, Source: "replacement"}
	revision := s.view.Revision
	unlock()
	if err := f.a.claudeContextCapabilities(context.Background(), f.p, nil, false); !errors.Is(err, errClaudeContextChanged) {
		t.Fatalf("late old-worker preflight was accepted: %v", err)
	}
	if current := f.a.claudeContextView(next); current.Revision != revision || current.Snapshot == nil || current.Snapshot.UsedTokens != 71 {
		t.Fatal("late preflight erased the replacement worker's context")
	}
}

func TestClaudeCompactReplacementReceiptStaysBoundToSource(t *testing.T) {
	for _, changedSource := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-source", true: "other-source"}[changedSource], func(t *testing.T) {
			f, _ := newClaudeContextFixture(t)
			view := f.context(t)
			f.compact(t, view, "11111111-1111-4111-8111-111111111111")
			if changedSource {
				f.key.Store("synthetic-replacement-source")
			}
			next := claudeSyntheticPermissionWorker(t, f.a, f.p.id)
			next.workerSettingsSig = claudeSettingsSig(f.a.settings(), f.a.exeOverride)
			duplicate, err := f.a.CompactContext(context.Background(), f.p.id, view.Revision, "11111111-1111-4111-8111-111111111111", nil)
			if changedSource {
				if !errors.Is(err, errClaudeContextChanged) || duplicate.Compaction != nil {
					t.Fatal("replacement exposed a receipt from a different source")
				}
			} else if err != nil || duplicate.Compaction == nil || duplicate.Compaction.State != "unconfirmed" || duplicate.Compaction.Blocking {
				t.Fatalf("replacement erased the uncertain operation receipt: %v", err)
			}
			if len(f.users) != 1 {
				t.Fatal("replacement replayed the original native compact command")
			}
		})
	}
}

func TestClaudeCompactReplacementDuringWriteKeepsOperationReceipt(t *testing.T) {
	f, _ := newClaudeContextFixture(t)
	view := f.context(t)
	f.onUser = func(map[string]any) (int, error) {
		next := claudeSyntheticPermissionWorker(t, f.a, f.p.id)
		next.workerSettingsSig = f.p.workerSettingsSig
		s, unlock, err := f.a.lockActiveClaudeContext(next)
		if err != nil {
			t.Fatal(err)
		}
		s.view.Snapshot = &protocol.ContextSnapshot{UsedTokens: 71, Source: "replacement"}
		unlock()
		return -1, nil
	}
	got := f.compact(t, view, "11111111-1111-4111-8111-111111111111")
	if got.Compaction == nil || got.Compaction.State != "unconfirmed" || got.Compaction.Blocking || got.Snapshot != nil || got.CanCompact {
		t.Fatal("replacement hid the dispatched operation or returned its successor's snapshot")
	}
	if len(f.users) != 1 {
		t.Fatal("replacement replayed the native command")
	}
}

func TestClaudeCompactIgnoresForeignChildAndReplayedNativeFrames(t *testing.T) {
	f, _ := newClaudeContextFixture(t)
	f.version = "2.1.220" // no background summary read between operations
	view := f.context(t)
	f.compact(t, view, "11111111-1111-4111-8111-111111111111")
	for _, child := range []map[string]any{
		{"type": "system", "subtype": "compact_boundary", "uuid": "child-boundary", "agent_id": "child", "compact_metadata": map[string]any{"trigger": "manual"}},
		{"type": "result", "subtype": "success", "uuid": "child-result", "agent_id": "child"},
		{"type": "result", "subtype": "success", "uuid": "foreign-result", "user_message_uuid": "different-user"},
		{"type": "result", "subtype": "success", "uuid": "background-result", "origin": map[string]any{"kind": "task-notification"}},
	} {
		f.frame(child)
	}
	if current := f.a.claudeContextView(f.p); current.Compaction.State != "running" {
		t.Fatal("foreign/child result completed compact")
	}
	f.result("first-result", true, false)
	second := f.context(t)
	f.compact(t, second, "22222222-2222-4222-8222-222222222222")
	f.frame(map[string]any{"type": "system", "subtype": "compact_boundary", "uuid": "boundary-first-result", "compact_metadata": map[string]any{"trigger": "manual"}})
	f.frame(map[string]any{"type": "result", "subtype": "success", "uuid": "first-result", "local_command": "compact"})
	if current := f.a.claudeContextView(f.p); current.Compaction.State != "running" {
		t.Fatal("replayed result completed a newer operation")
	}
	f.result("second-no-boundary", false, false)
	if current := f.a.claudeContextView(f.p); current.Compaction.State != "failed" {
		t.Fatal("replayed boundary counted as a new compaction")
	}
}

func TestClaudeContextReadDoesNotStartUnknownRuntime(t *testing.T) {
	f, _ := newClaudeContextFixture(t)
	view, err := f.a.SessionContext(context.Background(), "not-owned", nil)
	if err != nil || view.Snapshot != nil || view.CanCompact || view.UnavailableReason != "runtime_not_owned" || len(f.controls) != 0 {
		t.Fatal("context read started or adopted a runtime")
	}
}

func TestClaudeCompactLostWorkerRetainsUnconfirmedReceipt(t *testing.T) {
	f, _ := newClaudeContextFixture(t)
	view := f.context(t)
	f.compact(t, view, "11111111-1111-4111-8111-111111111111")
	f.a.mu.Lock()
	delete(f.a.procs, f.p.id)
	f.a.mu.Unlock()
	current := f.context(t)
	if current.Compaction == nil || current.Compaction.State != "unconfirmed" || current.Compaction.Blocking || current.Snapshot != nil || current.CanCompact {
		t.Fatal("lost runtime erased or completed pending compaction")
	}
	duplicate := f.compact(t, view, "11111111-1111-4111-8111-111111111111")
	if duplicate.Compaction.State != "unconfirmed" || duplicate.Compaction.Blocking || len(f.users) != 1 {
		t.Fatal("lost runtime retry replayed the original compact")
	}
	f.key.Store("synthetic-other-source")
	if changed := f.context(t); changed.Compaction != nil {
		t.Fatal("detached receipt crossed into another source")
	}
}

func TestClaudeContextOrdinaryBusyDoesNotBlockCompletedCompaction(t *testing.T) {
	f, _ := newClaudeContextFixture(t)
	f.version = "2.1.220"
	view := f.context(t)
	f.compact(t, view, "11111111-1111-4111-8111-111111111111")
	f.result("finished-before-ordinary-turn", true, false)
	if err := f.a.Send(context.Background(), f.p.id, "ordinary work after compact"); err != nil {
		t.Fatal(err)
	}
	current := f.context(t)
	if current.Compaction == nil || current.Compaction.State != "completed" || current.Compaction.Blocking || current.UnavailableReason != "runtime_busy" || current.CanCompact {
		t.Fatal("ordinary task occupancy was attributed to an old compaction")
	}
}

func TestClaudeCompactPriorReceiptDoesNotBlockAnotherOperation(t *testing.T) {
	for _, replaced := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-worker", true: "replacement-worker"}[replaced], func(t *testing.T) {
			f, _ := newClaudeContextFixture(t)
			f.version = "2.1.220"
			view := f.context(t)
			f.compact(t, view, "11111111-1111-4111-8111-111111111111")
			p := f.p
			if replaced {
				p = claudeSyntheticPermissionWorker(t, f.a, p.id)
				p.workerSettingsSig = f.p.workerSettingsSig
			} else {
				f.result("prior-operation-completed", true, false)
			}
			s, unlock, err := f.a.lockActiveClaudeContext(p)
			if err != nil {
				t.Fatal(err)
			}
			// Model an already admitted second native operation. Retrying the
			// first receipt must never inherit this operation's occupancy.
			second := &claudeContextOperation{worker: p, done: make(chan struct{}),
				receipt: protocol.ContextCompaction{OperationID: "22222222-2222-4222-8222-222222222222", State: "running"}}
			s.pending, s.operations[second.receipt.OperationID] = second, second
			s.view.Compaction, s.view.CanCompact, p.contextCompacting = &second.receipt, false, true
			unlock()
			current, err := f.a.SessionContext(context.Background(), p.id, nil)
			if err != nil || current.Compaction == nil || current.Compaction.OperationID != second.receipt.OperationID || !current.Compaction.Blocking {
				t.Fatalf("current compact occupancy was lost: %v", err)
			}
			duplicate, err := f.a.CompactContext(context.Background(), p.id, view.Revision, "11111111-1111-4111-8111-111111111111", nil)
			if err != nil || duplicate.Compaction == nil || duplicate.Compaction.OperationID != "11111111-1111-4111-8111-111111111111" || duplicate.Compaction.Blocking || len(f.users) != 1 {
				t.Fatalf("old receipt inherited another operation's occupancy or replayed: %v", err)
			}
		})
	}
}

func TestClaudeCompactRetiredBeforeAcknowledgementIsNotBlocking(t *testing.T) {
	f, _ := newClaudeContextFixture(t)
	view := f.context(t)
	var retired protocol.SessionContext
	f.onUser = func(map[string]any) (int, error) {
		f.a.stop(f.p)
		retired = f.a.claudeContextView(f.p)
		return -1, nil
	}
	got := f.compact(t, view, "11111111-1111-4111-8111-111111111111")
	if retired.Compaction == nil || retired.Compaction.State != "requested" || retired.Compaction.Blocking || got.Compaction == nil || got.Compaction.Blocking {
		t.Fatal("retired runtime stayed blocking before its receipt reached a terminal state")
	}
}

func TestClaudeCompactUnconfirmedBlocksUntilRuntimeRetired(t *testing.T) {
	f, rec := newClaudeContextFixture(t)
	view := f.context(t)
	f.compact(t, view, "11111111-1111-4111-8111-111111111111")
	f.a.contextState.mu.Lock()
	op := f.a.contextState.sessions[f.p.id].operations["11111111-1111-4111-8111-111111111111"]
	f.a.contextState.mu.Unlock()
	// Deliberately separate the result transition from the following stop.
	// No OS process is needed to exercise this admission-fence interval.
	f.a.finishClaudeCompact(f.p, op, "unconfirmed", "synthetic unconfirmed result")
	current := f.context(t)
	if current.Compaction == nil || current.Compaction.State != "unconfirmed" || !current.Compaction.Blocking || current.CanCompact {
		t.Fatal("unconfirmed result unlocked the projection before worker retirement")
	}
	duplicate := f.compact(t, view, "11111111-1111-4111-8111-111111111111")
	if duplicate.Compaction == nil || !duplicate.Compaction.Blocking || len(f.users) != 1 {
		t.Fatal("same-operation retry lost the still-active admission fence")
	}
	if err := f.a.Send(context.Background(), f.p.id, "must wait for retirement"); !errors.Is(err, errClaudeContextBusy) {
		t.Fatalf("unconfirmed result reopened actual turn admission: %v", err)
	}
	rec.wait(t, "unconfirmed compact still occupies runtime", func(e protocol.Event) bool {
		return e.Type == "context.updated" && e.Context != nil && e.Context.Compaction != nil &&
			e.Context.Compaction.OperationID == op.receipt.OperationID && e.Context.Compaction.State == "unconfirmed" && e.Context.Compaction.Blocking
	})
	f.a.stop(f.p)
	retired := f.a.claudeCompactReceiptView(f.p, op)
	if retired.Compaction == nil || retired.Compaction.State != "unconfirmed" || retired.Compaction.Blocking {
		t.Fatal("worker retirement did not release the projection")
	}
}
