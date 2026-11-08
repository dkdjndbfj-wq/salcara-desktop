package agents

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"salcara/bridge/internal/protocol"
)

func TestClaudeStoppedHistoryPreservesTerminalOnlyWithOwnedCompletion(t *testing.T) {
	for _, name := range []string{"confirmed_owned_exit", "external_write_revokes_completion", "missing_transcript_revokes_completion"} {
		t.Run(name, func(t *testing.T) {
			a := newClaudeAgent(func(protocol.Event) {}, func() Settings { return Settings{} }, "not-launched", t.TempDir())
			t.Cleanup(a.Close)
			id := "11111111-2222-4333-8444-555555555555"
			dir := filepath.Join(a.hist.dir, "-work")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, id+".jsonl")
			transcript := []byte(`{"type":"user","message":{"role":"user","content":"fixture prompt"},"cwd":"/work"}` + "\n")
			if err := os.WriteFile(path, transcript, 0o600); err != nil {
				t.Fatal(err)
			}
			st, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			a.hist.ownedLeases = map[string]*claudeOwnedLease{id: {exited: true, terminal: "interrupted", path: path, file: st}}
			// The stopped worker has been removed from procs; only its confirmed
			// completion receipt and Bridge turn metadata remain.
			terminal := "turn_stopped_fixture"
			liveRevision := nowMs() + 1000
			a.live[id] = protocol.SessionInfo{SessionKey: "claude:" + id, Tool: "claude", Status: "idle", LastTerminalTurnID: terminal, UpdatedAt: liveRevision}
			if name == "external_write_revokes_completion" {
				if err := os.WriteFile(path, append(transcript, []byte(`{"type":"user","message":{"role":"user","content":"external update"}}`+"\n")...), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if name == "missing_transcript_revokes_completion" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			check := func(info protocol.SessionInfo) {
				t.Helper()
				if name != "confirmed_owned_exit" {
					if info.Controllable || info.LastTerminalTurnID != "" || info.ActiveTurnID != "" {
						t.Fatalf("external transcript inherited stale Bridge turn control: %+v", info)
					}
					return
				}
				if !info.Controllable || info.Status != "idle" || info.ActiveTurnID != "" || info.LastTerminalTurnID != terminal {
					t.Fatalf("confirmed stopped history lost terminal identity: %+v", info)
				}
				if info.UpdatedAt < liveRevision {
					t.Fatal("reopened terminal snapshot regressed behind the live revision")
				}
			}
			info, _, err := a.Open(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			check(info)
			for _, open := range []func(context.Context, string, string, int) (protocol.SessionInfo, []protocol.Event, string, error){a.OpenPage, a.OpenMessagesPage} {
				info, _, _, err := open(context.Background(), id, "", 10)
				if err != nil {
					t.Fatal(err)
				}
				check(info)
			}
			list, err := a.Sessions(context.Background())
			if err != nil || len(list) != 1 {
				t.Fatalf("missing fixture history: %v, %d sessions", err, len(list))
			}
			check(list[0])
			list, _, err = a.SessionsPage(context.Background(), "", 10, "code")
			if err != nil || len(list) != 1 {
				t.Fatalf("missing paged fixture history: %v, %d sessions", err, len(list))
			}
			check(list[0])
		})
	}
}
