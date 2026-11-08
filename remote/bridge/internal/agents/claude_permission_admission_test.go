package agents

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/protocol"
)

func TestClaudeFullAccessConsentUsesActualPolicyAndResumeDefault(t *testing.T) {
	for _, tc := range []struct {
		name, saved, worker, fallback string
		want                          bool
	}{
		{"saved full access", "auto_all", "auto_all", "ask", true},
		{"worker full access", "", "auto_all", "ask", true},
		{"unknown resume never inherits bypass", "", "", "auto_all", false},
		{"explicit ask preserved", "ask", "ask", "auto_all", false},
		{"edits are not full access", "auto_edits", "auto_edits", "auto_all", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newClaudeAgent(func(protocol.Event) {}, func() Settings { return Settings{Approval: tc.fallback} }, "not-launched", t.TempDir())
			t.Cleanup(a.Close)
			a.policies["synthetic"] = tc.saved
			if tc.worker != "" {
				p := claudeSyntheticPermissionWorker(t, a, "synthetic")
				p.policy = tc.worker
			}
			if a.SessionRequiresFullAccessConsent("synthetic") != tc.want {
				t.Fatal("consent did not follow the effective worker/resume policy")
			}
			if a.SessionRequiresFullAccessConsent("") {
				t.Fatal("new session must use its separately resolved explicit/default policy")
			}
		})
	}
}

func TestClaudePermissionGuardRejectsStartBeforeLaunching(t *testing.T) {
	a := newClaudeAgent(func(protocol.Event) {}, func() Settings { return Settings{ClaudeRoot: "http://127.0.0.1:1", ClaudeKey: "synthetic-api-key"} }, "never-launched", t.TempDir())
	t.Cleanup(a.Close)
	denied := errors.New("synthetic consent revoked")
	id, err := a.StartWithOptions(context.Background(), t.TempDir(), "unsent", "auto_all", TurnOptions{ValidatePermission: func() error {
		_ = a.ActiveRemoteTurns() // a callback may inspect the agent without deadlocking
		return denied
	}})
	if id != "" || !errors.Is(err, denied) || a.ActiveRemoteTurns() {
		t.Fatal("permission refusal did not precede startup")
	}
}

func TestClaudePermissionGuardRechecksNewWorkerBeforeFirstTurn(t *testing.T) {
	f := newFixture(t, "auto_all") // test executable only, temporary history, no provider
	a := f.m.Get("claude").(*claudeAgent)
	denied := errors.New("synthetic consent revoked after spawn")
	checks := 0
	id, err := a.StartWithOptions(context.Background(), f.workDir, "unsent", "auto_all", TurnOptions{ValidatePermission: func() error {
		_ = a.ActiveRemoteTurns()
		checks++
		if checks > 1 {
			return denied
		}
		return nil
	}})
	if id != "" || !errors.Is(err, denied) || checks != 2 {
		t.Fatal("new worker silently dropped the late permission guard")
	}
	for _, e := range f.rec.all() {
		if e.Type == "message" && e.Role == "user" || e.Type == "turn" && e.Status == "started" {
			t.Fatal("denied first turn was published as accepted")
		}
	}
}

func TestClaudePermissionRevocationWhileWaitingForWriterPreservesAcceptedWork(t *testing.T) {
	for _, continuation := range []bool{false, true} {
		t.Run(map[bool]string{false: "send", true: "continue"}[continuation], func(t *testing.T) {
			rec := &recorder{}
			a := newClaudeAgent(rec.sink, func() Settings {
				return Settings{Approval: "auto_all", ClaudeRoot: "http://127.0.0.1:1", ClaudeKey: "synthetic-api-key"}
			}, "not-launched", t.TempDir())
			t.Cleanup(a.Close)
			p := claudeSyntheticPermissionWorker(t, a, "synthetic")
			p.policy = "auto_all"
			p.workerSettingsSig = claudeSettingsSig(a.settings(), a.exeOverride)
			a.policies[p.id] = "auto_all"
			a.live[p.id] = protocol.SessionInfo{SessionKey: "claude:" + p.id, Status: "idle", LastTerminalTurnID: "terminal-fixture"}
			if !continuation {
				p.turns, p.turnIDs = 1, []string{"accepted-fixture"}
				a.live[p.id] = protocol.SessionInfo{SessionKey: "claude:" + p.id, Status: "running", ActiveTurnID: "accepted-fixture"}
			}
			var writes atomic.Int32
			p.stdin = &claudeDeliveryFixtureWriter{write: func(data []byte) (int, error) { writes.Add(1); return len(data), nil }}
			var allowed atomic.Bool
			allowed.Store(true)
			denied := errors.New("synthetic live consent revoked")
			entered := make(chan struct{}, 1)
			opts := TurnOptions{ValidatePermission: func() error {
				_ = a.ActiveRemoteTurns()
				if a.SessionRequiresFullAccessConsent(p.id) && !allowed.Load() {
					return denied
				}
				select {
				case entered <- struct{}{}:
				default:
				}
				return nil
			}}
			p.wmu.Lock()
			result := make(chan error, 1)
			go func() {
				if continuation {
					_, err := a.ContinueWithOptions(context.Background(), p.id, "unsent", "terminal-fixture", "10000000-0000-4000-8000-000000000001", opts)
					result <- err
				} else {
					result <- a.SendWithOptions(context.Background(), p.id, "unsent", opts)
				}
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				p.wmu.Unlock()
				t.Fatal("first consent check did not reach the synthetic barrier")
			}
			allowed.Store(false)
			p.wmu.Unlock()
			select {
			case err := <-result:
				if !errors.Is(err, denied) {
					t.Fatal("late revocation did not reject the new turn", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("permission callback deadlocked with agent state")
			}
			if writes.Load() != 0 || len(rec.all()) != 0 || p.stopped {
				t.Fatal("refusal wrote/published a turn or stopped an existing worker")
			}
			if !continuation && (p.turns != 1 || len(p.turnIDs) != 1 || p.turnIDs[0] != "accepted-fixture") {
				t.Fatal("refusal changed an already accepted task")
			}
			if continuation && a.continuedTerminal[p.id] != "" {
				t.Fatal("refused continuation retained its reservation")
			}
		})
	}
}
