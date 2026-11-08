package agents

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"salcara/bridge/internal/protocol"
)

func TestClaudeNativeModeSwitchResumesOwnedSessionAndKeepsPromptsExplicit(t *testing.T) {
	f := newFixture(t, "auto_all") // synthetic test worker, never the installed Claude
	a := f.m.Get("claude").(*claudeAgent)
	id, err := a.StartWithOptions(context.Background(), f.workDir, "fixture first", "", TurnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	finish := func(wantMode string, turnCount int) {
		request := f.rec.wait(t, "explicit native request", func(e protocol.Event) bool {
			return e.Type == "approval.request" && e.SessionKey == "claude:"+id && !seen[e.ApprovalID]
		})
		seen[request.ApprovalID] = true
		if !a.Respond(request.ApprovalID, "allow", "") {
			t.Fatal("native request did not accept its exact answer")
		}
		f.rec.wait(t, "native completion", func(e protocol.Event) bool {
			return e.Type == "turn" && e.Status == "completed" && countTurns(f.rec) >= turnCount
		})
		log := readLog(t, f.logDir, "claude-args.jsonl")
		if len(log) != turnCount {
			t.Fatalf("worker count %d, want %d", len(log), turnCount)
		}
		args := strings.Join(toStrings(log[len(log)-1]["args"]), " ")
		if !strings.Contains(args, "--permission-mode "+wantMode) {
			t.Fatalf("wrong native mode: %s", args)
		}
		if turnCount > 1 && (!strings.Contains(args, "--resume "+id) || strings.Contains(args, "--session-id")) {
			t.Fatal("mode switch created a different thread")
		}
		a.mu.Lock()
		actual := a.live[id].ApprovalMode
		a.mu.Unlock()
		expected := map[string]string{"default": "ask", "acceptEdits": "auto_edits", "bypassPermissions": "auto_all"}[wantMode]
		if actual != expected {
			t.Fatalf("published policy %q, want %q", actual, expected)
		}
	}
	finish("default", 1) // global default auto_all cannot replace explicit phone consent
	dir := filepath.Join(f.home, "projects", "fixture")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte("{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":\"synthetic\"}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for i, policy := range []string{"auto_all", "auto_edits", "ask"} {
		if err := a.SendWithOptions(context.Background(), id, "fixture next", TurnOptions{ClaudeApproval: policy, ValidatePermission: func() error { return nil }}); err != nil {
			t.Fatal(err)
		}
		finish(map[string]string{"ask": "default", "auto_edits": "acceptEdits", "auto_all": "bypassPermissions"}[policy], i+2)
	}
}

func TestClaudeModeChangeCannotRetireBusyWorkerOrWriteWrongPolicy(t *testing.T) {
	for _, busy := range []bool{false, true} {
		a := newClaudeAgent(func(protocol.Event) {}, func() Settings { return Settings{ClaudeRoot: "http://127.0.0.1:1", ClaudeKey: "synthetic"} }, "not-launched", t.TempDir())
		p := claudeSyntheticPermissionWorker(t, a, "fixture")
		p.policy = "auto_all"
		p.workerSettingsSig = claudeSettingsSig(a.settings(), a.exeOverride)
		var writes atomic.Int32
		p.stdin = &claudeDeliveryFixtureWriter{write: func(b []byte) (int, error) { writes.Add(1); return len(b), nil }}
		if busy {
			p.turns = 1
			p.turnIDs = []string{"active-fixture"}
		}
		var err error
		if busy {
			err = a.SendWithOptions(context.Background(), p.id, "unsent", TurnOptions{ClaudeApproval: "ask"})
		} else {
			err = a.writeTurn(p, "unsent", nil, nil, TurnOptions{ClaudeApproval: "ask"})
		}
		if err == nil || p.stopped || writes.Load() != 0 {
			t.Fatal("mode change bypassed worker identity/admission fence")
		}
		a.Close()
	}
}

func TestClaudeNativeModeLaunchRechecksLiveConsentBeforeProcessStart(t *testing.T) {
	f := newFixture(t, "auto_all")
	a := f.m.Get("claude").(*claudeAgent)
	denied := errors.New("synthetic revoked before cmd.Start")
	checks := 0
	id, err := a.StartWithOptions(context.Background(), f.workDir, "unsent", "auto_all", TurnOptions{ValidatePermission: func() error {
		checks++
		if checks == 3 {
			return denied
		}
		return nil
	}})
	if id != "" || !errors.Is(err, denied) || checks != 3 {
		t.Fatalf("startup consent boundary missed: %q %v %d", id, err, checks)
	}
	if len(readLog(t, f.logDir, "claude-args.jsonl")) != 0 || a.ActiveRemoteTurns() {
		t.Fatal("revoked launch ran a worker")
	}
}
