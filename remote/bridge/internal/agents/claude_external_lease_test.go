package agents

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"salcara/bridge/internal/protocol"
)

func TestExternalClaudeHistoryAgeNeverEstablishesControlOwnership(t *testing.T) {
	for _, age := range []time.Duration{0, time.Minute, time.Hour, 48 * time.Hour} {
		a := &claudeAgent{hist: newClaudeHistory(t.TempDir()), procs: map[string]*claudeProc{}, recentOwned: map[string]time.Time{}}
		info := protocol.SessionInfo{SessionKey: "claude:external", UpdatedAt: time.Now().Add(-age).UnixMilli()}
		a.mu.Lock()
		a.decorateLocked("external", &info)
		a.mu.Unlock()
		if info.Controllable || info.Status == "idle" {
			t.Fatalf("age %s became proof of idle ownership: %+v", age, info)
		}
	}
}

func TestClaudeExternalStateFutureClockIsUnknown(t *testing.T) {
	h := newClaudeHistory(t.TempDir())
	state, controllable := h.remoteHistoryState("external", time.Now().Add(10*time.Minute).UnixMilli())
	if state != "unknown" || controllable {
		t.Fatalf("future external timestamp became active: state=%q controllable=%v", state, controllable)
	}
}

func TestClaudeOwnedCompletionRevokedWhenTranscriptChanges(t *testing.T) {
	home := t.TempDir()
	h := newClaudeHistory(filepath.Join(home, "projects"))
	dir := filepath.Join(h.dir, "-work")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	id := "11111111-2222-4333-8444-555555555555"
	path := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user","message":{"role":"user","content":"hello"},"cwd":"/work"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	h.ownedLeases = map[string]*claudeOwnedLease{id: &claudeOwnedLease{exited: true, path: path, file: st}}
	if !h.ownedCompletion(id) {
		t.Fatal("unchanged transcript should retain the process-local completion receipt")
	}
	if err := os.WriteFile(path, append(mustReadFile(t, path), []byte(`{"type":"assistant"}`+"\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if h.ownedCompletion(id) {
		t.Fatal("transcript mutation revived controllable ownership")
	}
	if _, ok := h.ownedLeases[id]; ok {
		t.Fatal("invalidated lease was retained")
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
