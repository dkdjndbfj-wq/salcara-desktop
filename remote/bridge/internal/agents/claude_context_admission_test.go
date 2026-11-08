package agents

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/protocol"
)

func TestClaudeCanceledStartRefusesBeforeLaunchingOrPublishing(t *testing.T) {
	rec := &recorder{}
	a := newClaudeAgent(rec.sink, func() Settings { return Settings{ClaudeRoot: "http://127.0.0.1:1", ClaudeKey: "synthetic-api-key"} }, "must-not-be-launched", t.TempDir())
	t.Cleanup(a.Close)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	id, err := a.StartWithOptions(ctx, t.TempDir(), "must not start", "ask", TurnOptions{})
	if id != "" || !errors.Is(err, context.Canceled) || len(rec.all()) != 0 || len(a.live) != 0 || len(a.procs) != 0 || len(a.workers) != 0 {
		t.Fatal("canceled startup reached process creation or admission")
	}
}

func TestClaudeCanceledStartupLockWaitDoesNotCreateProcess(t *testing.T) {
	a := newClaudeAgent(func(protocol.Event) {}, func() Settings { return Settings{ClaudeRoot: "http://127.0.0.1:1", ClaudeKey: "synthetic-api-key"} }, "must-not-be-launched", t.TempDir())
	t.Cleanup(a.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startMu.Lock()
	completed := make(chan error, 1)
	go func() {
		p, err := a.spawn(ctx, "synthetic-canceled-start", t.TempDir(), "", "ask", false)
		if p != nil {
			completed <- errors.New("canceled startup unexpectedly returned a process")
			return
		}
		completed <- err
	}()
	cancel()
	a.startMu.Unlock()
	select {
	case err := <-completed:
		if !errors.Is(err, context.Canceled) || len(a.workers) != 0 {
			t.Fatal("canceled startup reached executable lookup or process creation")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled startup did not leave its startup-lock wait")
	}
}

func TestClaudeSendCanceledWhileWaitingForWriterDoesNotAdmit(t *testing.T) {
	a, p, rec := newClaudeResultFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var writes, permissionChecks atomic.Int32
	entered := make(chan struct{})
	p.stdin = &claudeDeliveryFixtureWriter{write: func(data []byte) (int, error) {
		writes.Add(1)
		return len(data), nil
	}}
	p.wmu.Lock()
	completed := make(chan error, 1)
	go func() {
		completed <- a.SendWithOptions(ctx, p.id, "must not be admitted", TurnOptions{ValidatePermission: func() error {
			if permissionChecks.Add(1) == 1 {
				close(entered)
			}
			return nil
		}})
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		p.wmu.Unlock()
		t.Fatal("sender did not reach initial permission check")
	}
	cancel()
	p.wmu.Unlock()
	select {
	case err := <-completed:
		if !errors.Is(err, context.Canceled) || writes.Load() != 0 || len(rec.all()) != 0 || permissionChecks.Load() != 1 {
			t.Fatal("canceled queued sender reached write or lost original guard")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled queued sender did not return")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.turns != 0 || len(p.turnIDs) != 0 || p.stopped {
		t.Fatal("canceled queued send altered worker or admitted work")
	}
}

func TestClaudeContextCancellationAfterAdmissionKeepsOwnedTurn(t *testing.T) {
	a, p, rec := newClaudeResultFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var writes, permissionChecks atomic.Int32
	p.stdin = &claudeDeliveryFixtureWriter{write: func(data []byte) (int, error) {
		writes.Add(1)
		return len(data), nil
	}}
	a.sink = func(e protocol.Event) {
		rec.sink(e)
		if e.Type == "message" && e.Role == "user" {
			cancel() // The turn was already reserved before publication.
		}
	}
	err := a.SendWithOptions(ctx, p.id, "accepted exactly once", TurnOptions{ValidatePermission: func() error {
		permissionChecks.Add(1)
		return nil
	}})
	if err != nil || writes.Load() != 1 || permissionChecks.Load() != 2 {
		t.Fatal("canceled transport context rolled back or replayed an admitted turn")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped || p.turns != 1 || len(p.turnIDs) != 1 {
		t.Fatal("accepted owned work was stopped when its caller went away")
	}
}

func TestClaudeContextCanceledInsidePermissionGuardCannotAdmit(t *testing.T) {
	for _, at := range []int32{1, 2} {
		a, p, rec := newClaudeResultFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var writes, checks atomic.Int32
		p.stdin = &claudeDeliveryFixtureWriter{write: func(data []byte) (int, error) {
			writes.Add(1)
			return len(data), nil
		}}
		err := a.SendWithOptions(ctx, p.id, "not admitted", TurnOptions{ValidatePermission: func() error {
			if checks.Add(1) == at {
				cancel()
			}
			return nil // An independently successful guard is not a live context.
		}})
		if !errors.Is(err, context.Canceled) || writes.Load() != 0 || len(rec.all()) != 0 || p.turns != 0 || checks.Load() != at {
			t.Fatal("successful permission guard hid context cancellation")
		}
	}
}

type claudeAdmissionCheckContext struct {
	context.Context
	checks       atomic.Int32
	guardChecked chan struct{}
}

func (c *claudeAdmissionCheckContext) Err() error {
	err := c.Context.Err()
	// Snapshot the last unlocked guard check before signaling the test. The
	// caller may then be canceled while waiting for a.mu; a later locked check
	// is what must observe that cancellation rather than this prior snapshot.
	if c.checks.Add(1) == 4 {
		close(c.guardChecked)
	}
	return err
}

func TestClaudeContextCanceledWhileWaitingForAgentLockCannotAdmit(t *testing.T) {
	a, p, rec := newClaudeResultFixture(t)
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &claudeAdmissionCheckContext{Context: base, guardChecked: make(chan struct{})}
	var writes, checks atomic.Int32
	p.stdin = &claudeDeliveryFixtureWriter{write: func(data []byte) (int, error) {
		writes.Add(1)
		return len(data), nil
	}}
	completed := make(chan error, 1)
	go func() {
		completed <- a.SendWithOptions(ctx, p.id, "not admitted", TurnOptions{ValidatePermission: func() error {
			if checks.Add(1) == 2 {
				a.mu.Lock() // The test releases this after canceling the context.
			}
			return nil
		}})
	}()
	select {
	case <-ctx.guardChecked:
	case <-time.After(3 * time.Second):
		t.Fatal("sender did not pass the final unlocked guard")
	}
	cancel()
	a.mu.Unlock()
	select {
	case err := <-completed:
		if !errors.Is(err, context.Canceled) || writes.Load() != 0 || len(rec.all()) != 0 || p.turns != 0 {
			t.Fatal("canceled lock-waiting sender reached admission")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled lock-waiting sender did not return")
	}
}
