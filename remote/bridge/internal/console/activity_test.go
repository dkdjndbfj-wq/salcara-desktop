package console

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"salcara/bridge/internal/protocol"
)

type activityFixtureSnapshot struct {
	Revision    uint64            `json:"revision"`
	Sessions    []activitySession `json:"sessions"`
	ApprovalIDs []string          `json:"approvalIds"`
}

func readActivityFixture(t *testing.T, s *Server, handler http.Handler) activityFixtureSnapshot {
	t.Helper()
	w := do(handler, "GET", "/api/activity", "127.0.0.1:47831", map[string]string{"Cookie": CookieName + "=" + s.Token()}, "")
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("activity: %d %s", w.Code, w.Body.String())
	}
	var snapshot activityFixtureSnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestActivitySnapshotRestoresOnlyObservedLiveState(t *testing.T) {
	s, handler := newTestServer(t)
	// The status projection is available before any agent manager exists. It
	// cannot implicitly launch Codex or scan Claude histories while showing a ball.
	empty := readActivityFixture(t, s, handler)
	if empty.Sessions == nil || empty.ApprovalIDs == nil || empty.Revision != 0 {
		t.Fatalf("empty snapshot: %+v", empty)
	}
	s.Broadcast(protocol.Event{Type: "session.updated", Session: &protocol.SessionInfo{SessionKey: "codex:active", Status: "running", Title: "private-title", Cwd: "private-path"}})
	s.Broadcast(protocol.Event{Type: "session.updated", Session: &protocol.SessionInfo{SessionKey: "claude:waiting", Status: "waiting_approval"}})
	s.Broadcast(protocol.Event{Type: "approval.request", ApprovalID: "question", Text: "private-prompt"})
	snapshot := readActivityFixture(t, s, handler)
	if snapshot.Revision != 3 || len(snapshot.Sessions) != 2 || len(snapshot.ApprovalIDs) != 1 || snapshot.ApprovalIDs[0] != "question" {
		t.Fatalf("live snapshot: %+v", snapshot)
	}
	w := do(handler, "GET", "/api/activity", "127.0.0.1:47831", map[string]string{"Cookie": CookieName + "=" + s.Token()}, "")
	if strings.Contains(w.Body.String(), "private-") {
		t.Fatalf("activity exposes history: %s", w.Body.String())
	}
	s.Broadcast(protocol.Event{Type: "approval.resolved", ApprovalID: "question"})
	s.Broadcast(protocol.Event{Type: "session.updated", Session: &protocol.SessionInfo{SessionKey: "codex:active", Status: "idle"}})
	s.Broadcast(protocol.Event{Type: "session.updated", Session: &protocol.SessionInfo{SessionKey: "claude:waiting", Status: "failed"}})
	snapshot = readActivityFixture(t, s, handler)
	if snapshot.Revision != 6 || len(snapshot.Sessions) != 0 || len(snapshot.ApprovalIDs) != 0 {
		t.Fatalf("terminal state retained: %+v", snapshot)
	}
	other, otherHandler := newTestServer(t)
	if snapshot := readActivityFixture(t, other, otherHandler); snapshot.Revision != 0 || len(snapshot.Sessions) != 0 {
		t.Fatalf("another process inherited activity: %+v", snapshot)
	}
}

func TestActivitySnapshotRequiresConsoleCookieAndOrigin(t *testing.T) {
	s, handler := newTestServer(t)
	for _, tc := range []struct {
		headers map[string]string
		code    int
	}{
		{nil, http.StatusUnauthorized},
		{map[string]string{"Cookie": CookieName + "=" + s.Token(), "Origin": "https://another-site.example"}, http.StatusForbidden},
	} {
		if w := do(handler, "GET", "/api/activity", "localhost:47831", tc.headers, ""); w.Code != tc.code {
			t.Fatalf("unprotected activity: %d", w.Code)
		}
	}
}

func TestActivitySnapshotAndConcurrentSSEHaveOneMonotonicOrder(t *testing.T) {
	s, handler := newTestServer(t)
	const workers = 24
	ch := make(chan sseMsg, workers*4)
	s.subMu.Lock()
	s.subs[ch] = struct{}{}
	s.subMu.Unlock()
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("codex:%d", i)
			s.Broadcast(protocol.Event{Type: "session.updated", Session: &protocol.SessionInfo{SessionKey: id, Status: "running"}})
			s.Broadcast(protocol.Event{Type: "approval.request", ApprovalID: id})
			s.Broadcast(protocol.Event{Type: "approval.resolved", ApprovalID: id})
			s.Broadcast(protocol.Event{Type: "session.updated", Session: &protocol.SessionInfo{SessionKey: id, Status: "idle"}})
		}(i)
	}
	var previous uint64
	for i := 0; i < workers; i++ {
		snapshot := readActivityFixture(t, s, handler)
		if snapshot.Revision < previous {
			t.Fatal("snapshot revision moved backwards")
		}
		previous = snapshot.Revision
	}
	wg.Wait()
	for revision := uint64(1); revision <= workers*4; revision++ {
		select {
		case message := <-ch:
			var event struct {
				ActivityRevision uint64 `json:"activityRevision"`
			}
			if err := json.Unmarshal(message.data, &event); err != nil || event.ActivityRevision != revision {
				t.Fatalf("SSE order at %d: %s, %v", revision, message.data, err)
			}
		default:
			t.Fatalf("missing event at %d", revision)
		}
	}
	snapshot := readActivityFixture(t, s, handler)
	if snapshot.Revision != workers*4 || len(snapshot.Sessions) != 0 || len(snapshot.ApprovalIDs) != 0 {
		t.Fatalf("concurrent final snapshot: %+v", snapshot)
	}
}
