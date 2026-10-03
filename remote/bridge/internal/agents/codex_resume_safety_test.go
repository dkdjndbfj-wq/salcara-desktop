package agents

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The transport is entirely in-process: safety failures must not spawn Codex,
// contact a model provider, resume a thread, or invoke a tool.
type resumeSafetyTransport struct {
	t       *testing.T
	conn    *rpcConn
	thread  cxThread
	readErr bool
	calls   []string
	ids     []string
}

func (s *resumeSafetyTransport) Close() error { return nil }
func (s *resumeSafetyTransport) Write(data []byte) (int, error) {
	var request struct {
		ID     int64  `json:"id"`
		Method string `json:"method"`
		Params struct {
			ThreadID     string `json:"threadId"`
			IncludeTurns bool   `json:"includeTurns"`
		} `json:"params"`
	}
	if err := json.Unmarshal(data, &request); err != nil {
		return 0, err
	}
	s.calls = append(s.calls, request.Method)
	s.ids = append(s.ids, request.Params.ThreadID)
	response := map[string]any{"id": request.ID}
	switch request.Method {
	case "thread/read":
		if !request.Params.IncludeTurns {
			s.t.Error("resume preflight did not request full turn history")
		}
		if s.readErr {
			response["error"] = map[string]any{"code": -32603, "message": "fixture read denied"}
		} else {
			response["result"] = map[string]any{"thread": s.thread}
		}
	case "thread/resume":
		response["result"] = map[string]any{"thread": s.thread}
	case "turn/start":
		response["result"] = map[string]any{"turn": cxTurn{ID: "fixture-next-turn", Status: "completed"}}
	default:
		return 0, errors.New("unexpected fixture RPC: " + request.Method)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return 0, err
	}
	s.conn.handleLine(encoded)
	return len(data), nil
}

func newResumeSafetyAgent(t *testing.T, thread cxThread, readErr bool) (*codexAgent, *resumeSafetyTransport, *recorder) {
	t.Helper()
	settings := Settings{RelayRoot: "https://fixture.invalid", CodexKey: "synthetic-fixture-key", Approval: "ask"}
	rec := &recorder{}
	a := newCodexAgent(rec.sink, func() Settings { return settings }, "", "")
	conn := &rpcConn{pending: map[int64]chan rpcResult{}, done: make(chan struct{})}
	transport := &resumeSafetyTransport{t: t, conn: conn, thread: thread, readErr: readErr}
	conn.stdin = transport
	a.conn, a.connSig = conn, codexSig(settings)
	t.Cleanup(func() {
		// This fixture has no OS process for rpcConn.Kill to stop.
		a.mu.Lock()
		a.conn = nil
		a.mu.Unlock()
		a.Close()
	})
	return a, transport, rec
}

func resumeSafetyRollout(t *testing.T, age time.Duration) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "fixture-rollout.jsonl")
	if err := os.WriteFile(file, []byte("fixture-only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	modified := time.Now().Add(-age)
	if err := os.Chtimes(file, modified, modified); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestCodexResumeReadFailureNeverResumesEvenWithCachedPath(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown-path", true: "cached-old-path"}[cached], func(t *testing.T) {
			a, transport, rec := newResumeSafetyAgent(t, cxThread{}, true)
			target := "fixture-original"
			if cached {
				a.threadLocked(target).path = resumeSafetyRollout(t, time.Hour)
			}
			err := a.Send(context.Background(), target, "must not be sent")
			if err == nil || !strings.Contains(err.Error(), "无法确认原 Codex 会话状态") {
				t.Fatalf("read failure allowed continuation: %v", err)
			}
			if !reflect.DeepEqual(transport.calls, []string{"thread/read"}) {
				t.Fatalf("read failure made mutating RPCs: %v", transport.calls)
			}
			if a.threads[target].info.Controllable {
				t.Fatal("unverified external thread remained controllable")
			}
			events := rec.all()
			if len(events) != 1 || events[0].Type != "session.updated" || events[0].Session.Controllable {
				t.Fatalf("phone was not notified of read-only state: %+v", events)
			}
		})
	}
}

func TestCodexResumeFreshActiveStateRefusesQuietExternalTurn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status cxThreadStatus
		turns  []cxTurn
		want   string
	}{
		{"active", cxThreadStatus{Type: "active"}, nil, "running"},
		{"approval", cxThreadStatus{Type: "active", ActiveFlags: []string{"waitingOnApproval"}}, nil, "waiting_approval"},
		{"not-loaded-in-progress", cxThreadStatus{Type: "notLoaded"}, []cxTurn{{ID: "long-tool", Status: "inProgress"}}, "running"},
		{"idle-in-progress", cxThreadStatus{Type: "idle"}, []cxTurn{{ID: "long-tool", Status: "inProgress"}}, "running"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := resumeSafetyRollout(t, time.Hour)
			thread := cxThread{ID: "fixture-original", Path: &old, Status: tc.status, Turns: tc.turns}
			a, transport, _ := newResumeSafetyAgent(t, thread, false)
			// Simulate a formerly idle cache. Sending must refresh even this path.
			a.threadLocked(thread.ID).path = old
			err := a.Send(context.Background(), thread.ID, "must not be sent")
			if err == nil || err.Error() != errExternalRunning {
				t.Fatalf("quiet external active turn resumed: %v", err)
			}
			if !reflect.DeepEqual(transport.calls, []string{"thread/read"}) {
				t.Fatalf("active preflight made mutating RPCs: %v", transport.calls)
			}
			info := a.threads[thread.ID].info
			if info.Status != tc.want || info.Controllable {
				t.Fatalf("external thread is not accurately read-only: %+v", info)
			}
		})
	}
}

func TestCodexResumeRecentFreshRolloutStillRefuses(t *testing.T) {
	old, recent := resumeSafetyRollout(t, time.Hour), resumeSafetyRollout(t, 0)
	thread := cxThread{ID: "fixture-original", Path: &recent, Status: cxThreadStatus{Type: "notLoaded"}, Turns: []cxTurn{{Status: "completed"}}}
	a, transport, _ := newResumeSafetyAgent(t, thread, false)
	a.threadLocked(thread.ID).path = old
	if err := a.Send(context.Background(), thread.ID, "must not be sent"); err == nil || err.Error() != errExternalRunning {
		t.Fatalf("recent rollout protection was lost: %v", err)
	}
	if !reflect.DeepEqual(transport.calls, []string{"thread/read"}) || a.threads[thread.ID].info.Controllable {
		t.Fatalf("recent fresh path allowed resume: %+v %v", a.threads[thread.ID].info, transport.calls)
	}
}

func TestCodexResumeRejectsMismatchedReadIdentity(t *testing.T) {
	a, transport, _ := newResumeSafetyAgent(t, cxThread{ID: "wrong-thread", Status: cxThreadStatus{Type: "idle"}}, false)
	if err := a.Send(context.Background(), "fixture-original", "must not be sent"); err == nil || !strings.Contains(err.Error(), "会话 ID 不匹配") {
		t.Fatalf("mismatched read identity allowed resume: %v", err)
	}
	if !reflect.DeepEqual(transport.calls, []string{"thread/read"}) {
		t.Fatalf("wrong identity made mutating RPCs: %v", transport.calls)
	}
}

func TestCodexResumeCompletedOriginalUsesSameIDAfterFreshRead(t *testing.T) {
	old := resumeSafetyRollout(t, time.Hour)
	thread := cxThread{ID: "fixture-original", Path: &old, Status: cxThreadStatus{Type: "notLoaded"}, Turns: []cxTurn{{ID: "completed-original", Status: "completed"}}}
	a, transport, _ := newResumeSafetyAgent(t, thread, false)
	if err := a.Send(context.Background(), thread.ID, "continue same thread"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(transport.calls, []string{"thread/read", "thread/resume", "turn/start"}) {
		t.Fatalf("wrong continuation sequence: %v", transport.calls)
	}
	for _, id := range transport.ids {
		if id != thread.ID {
			t.Fatalf("continuation changed thread identity: %v", transport.ids)
		}
	}
	if !a.threads[thread.ID].loaded {
		t.Fatal("completed original was not resumed on this bridge")
	}
}

func TestCodexApplyOwnedActiveTurnRemainsControllable(t *testing.T) {
	a, _, _ := newResumeSafetyAgent(t, cxThread{}, false)
	target := a.threadLocked("fixture-owned")
	target.loaded, target.turnID = true, "owned-turn"
	info := a.applyThreadLocked(cxThread{ID: target.id, Status: cxThreadStatus{Type: "active"}}).info
	if !info.Controllable || info.Status != "running" {
		t.Fatalf("own active turn became external read-only: %+v", info)
	}
}
