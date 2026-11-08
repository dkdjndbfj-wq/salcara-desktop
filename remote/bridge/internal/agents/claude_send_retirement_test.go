package agents

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/protocol"
)

func TestClaudeWorkerExitDuringLocalSendAdmissionCannotRestoreRunning(t *testing.T) {
	for _, stage := range []string{"user", "started", "running"} {
		t.Run(stage, func(t *testing.T) {
			rec := &recorder{}
			a := newClaudeAgent(rec.sink, func() Settings { return Settings{ClaudeRoot: "http://127.0.0.1:1", ClaudeKey: "synthetic-api-key"} }, "not-launched", t.TempDir())
			t.Cleanup(a.Close)
			p := claudeSyntheticPermissionWorker(t, a, "fixture-session")
			p.workerSettingsSig = claudeSettingsSig(a.settings(), a.exeOverride)
			var writes atomic.Int32
			p.stdin = &claudeDeliveryFixtureWriter{write: func(data []byte) (int, error) {
				writes.Add(1)
				return len(data), nil
			}}
			entered, release := make(chan struct{}), make(chan struct{})
			var blocked atomic.Bool
			a.sink = func(e protocol.Event) {
				// Admission/exit sinks may inspect busy state. No vendor mutex
				// may be held while this callback executes.
				_ = a.ActiveRemoteTurns()
				rec.sink(e)
				matches := stage == "user" && e.Type == "message" && e.Role == "user" ||
					stage == "started" && e.Type == "turn" && e.Status == "started" ||
					stage == "running" && e.Type == "session.updated" && e.Session != nil && e.Session.Status == "running"
				if matches && blocked.CompareAndSwap(false, true) {
					close(entered)
					<-release
				}
			}
			result := make(chan error, 1)
			go func() { result <- a.Send(context.Background(), p.id, "synthetic input") }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				close(release)
				t.Fatal("local send did not reach the synthetic admission barrier")
			}
			// A real reader can observe process exit while a local sink is busy.
			// Complete that cleanup before the sender is allowed to continue.
			a.stop(p)
			a.onExit(p, nil)
			close(release)
			var err error
			select {
			case err = <-result:
			case <-time.After(3 * time.Second):
				t.Fatal("retired send admission did not return")
			}
			a.mu.Lock()
			status := a.live[p.id].Status
			a.mu.Unlock()
			if status != "idle" || a.ActiveRemoteTurns() {
				t.Fatalf("retired worker was restored to running/busy: %s", status)
			}
			if writes.Load() != 0 {
				t.Fatal("retired-before-write worker attempted a new frame")
			}
			if err == nil || protocol.ExecutionErrorCode(err) == "command_delivery_uncertain" {
				t.Fatal("definite pre-write retirement was confused with attempted delivery")
			}
			terminal := false
			for _, e := range rec.all() {
				if e.Type == "turn" && (e.Status == "interrupted" || e.Status == "failed") {
					terminal = true
				}
				if terminal && (e.Type == "turn" && e.Status == "started" || e.Type == "session.updated" && e.Session != nil && e.Session.Status == "running") {
					t.Fatal("late local admission was published after authoritative exit")
				}
			}
		})
	}
}

func TestClaudeRetiredSendCannotUpdateSuccessorWorker(t *testing.T) {
	rec := &recorder{}
	a := newClaudeAgent(rec.sink, func() Settings { return Settings{ClaudeRoot: "http://127.0.0.1:1", ClaudeKey: "synthetic-api-key"} }, "not-launched", t.TempDir())
	t.Cleanup(a.Close)
	old := claudeSyntheticPermissionWorker(t, a, "fixture-session")
	old.workerSettingsSig = claudeSettingsSig(a.settings(), a.exeOverride)
	var oldWrites, nextWrites atomic.Int32
	old.stdin = &claudeDeliveryFixtureWriter{write: func(data []byte) (int, error) {
		oldWrites.Add(1)
		return len(data), nil
	}}
	entered, release := make(chan struct{}), make(chan struct{})
	var blocked atomic.Bool
	a.sink = func(e protocol.Event) {
		_ = a.ActiveRemoteTurns()
		rec.sink(e)
		if e.Type == "message" && e.Role == "user" && blocked.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
	}
	result := make(chan error, 1)
	go func() { result <- a.Send(context.Background(), old.id, "synthetic input") }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("old send did not reach the synthetic admission barrier")
	}
	next := claudeSyntheticPermissionWorker(t, a, old.id)
	next.stdin = &claudeDeliveryFixtureWriter{write: func(data []byte) (int, error) {
		nextWrites.Add(1)
		return len(data), nil
	}}
	a.setLive(next.id, func(info *protocol.SessionInfo) { info.Status, info.Model = "idle", "successor-model" })
	a.stop(old)
	a.onExit(old, nil)
	close(release)
	select {
	case err := <-result:
		if err == nil || protocol.ExecutionErrorCode(err) == "command_delivery_uncertain" {
			t.Fatal("retired-before-write sender lost definite refusal")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("old sender did not return after its worker was replaced")
	}
	a.mu.Lock()
	current, info := a.procs[next.id], a.live[next.id]
	a.mu.Unlock()
	if current != next || info.Status != "idle" || info.Model != "successor-model" || a.ActiveRemoteTurns() || oldWrites.Load() != 0 || nextWrites.Load() != 0 {
		t.Fatal("retired sender mutated or wrote to a successor's session")
	}
}
