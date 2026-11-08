package agents

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/protocol"
)

func TestClaudeContinueRechecksTerminalAfterWaitingForWriter(t *testing.T) {
	for _, mutation := range []string{"new terminal", "new active turn", "new child"} {
		t.Run(mutation, func(t *testing.T) {
			a, p, rec := newClaudeResultFixture(t)
			a.live[p.id] = protocol.SessionInfo{SessionKey: "claude:" + p.id, Status: "idle", LastTerminalTurnID: "terminal-first"}
			var writes atomic.Int32
			p.stdin = &claudeDeliveryFixtureWriter{write: func(b []byte) (int, error) { writes.Add(1); return len(b), nil }}
			entered := make(chan struct{}, 1)
			opts := TurnOptions{ExpectedTerminalTurnID: "must-not-override-observed-terminal", ValidatePermission: func() error {
				select {
				case entered <- struct{}{}:
				default:
				}
				return nil
			}}
			p.wmu.Lock()
			finished := make(chan error, 1)
			go func() {
				_, err := a.ContinueWithOptions(context.Background(), p.id, "unsent", "terminal-first", "10000000-0000-4000-8000-000000000010", opts)
				finished <- err
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				p.wmu.Unlock()
				t.Fatal("continue did not reach writer barrier")
			}
			a.mu.Lock()
			info := a.live[p.id]
			switch mutation {
			case "new terminal":
				info.LastTerminalTurnID = "terminal-second"
			case "new active turn":
				info.ActiveTurnID, info.Status = "accepted-turn", "running"
				p.mu.Lock()
				p.turns, p.turnIDs = 1, []string{"accepted-turn"}
				p.mu.Unlock()
			case "new child":
				p.activeSubtasks.Store(true)
			}
			a.live[p.id] = info
			a.mu.Unlock()
			p.wmu.Unlock()
			select {
			case err := <-finished:
				if !errors.Is(err, ErrTurnControlStale) {
					t.Fatalf("stale continuation admitted: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("continue final CAS deadlocked")
			}
			if writes.Load() != 0 || len(rec.all()) != 0 || p.stopped {
				t.Fatal("stale continue changed active work")
			}
			a.mu.Lock()
			reservation := a.continuedTerminal[p.id]
			a.mu.Unlock()
			if reservation != "" {
				t.Fatal("rejected continue retained reservation")
			}
		})
	}
}

func TestClaudeContinueUsesExpectedParameterAtFinalAdmission(t *testing.T) {
	a, p, rec := newClaudeResultFixture(t)
	a.live[p.id] = protocol.SessionInfo{SessionKey: "claude:" + p.id, Status: "idle", LastTerminalTurnID: "terminal-first"}
	got, err := a.ContinueWithOptions(context.Background(), p.id, "synthetic continuation", "terminal-first", "10000000-0000-4000-8000-000000000011", TurnOptions{ExpectedTerminalTurnID: "wrong-option"})
	if err != nil || !got.Accepted || got.PreviousTurnID != "terminal-first" {
		t.Fatalf("valid observed terminal not admitted: %+v %v", got, err)
	}
	starts := 0
	for _, e := range rec.all() {
		if e.Type == "turn" && e.Status == "started" {
			starts++
		}
	}
	if starts != 1 {
		t.Fatal("continue did not admit exactly one turn")
	}
}
