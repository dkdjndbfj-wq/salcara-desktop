package agents

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestClaudeIdleRetirementAndNewTurnAdmissionHaveOneWinner(t *testing.T) {
	for attempt := 0; attempt < 48; attempt++ {
		a, p, _ := newClaudeResultFixture(t)
		p.elicitationHook = &claudeElicitationLease{done: make(chan struct{})}
		var writes atomic.Int32
		p.stdin = &claudeDeliveryFixtureWriter{write: func(data []byte) (int, error) {
			writes.Add(1)
			return len(data), nil
		}}
		start := make(chan struct{})
		sent, retired := make(chan error, 1), make(chan error, 1)
		go func() { <-start; sent <- a.Send(context.Background(), p.id, "synthetic admission") }()
		go func() { <-start; retired <- a.retireIdleClaudeWorker(p) }()
		close(start)
		sendErr, retireErr := <-sent, <-retired
		p.mu.Lock()
		stopped, turns := p.stopped, p.turns
		p.mu.Unlock()
		if retireErr == nil {
			if !stopped || approvalSourceOpen(p.permissionLease.done) || approvalSourceOpen(p.elicitationHook.done) || sendErr == nil || turns != 0 || writes.Load() != 0 {
				t.Fatal("idle retirement admitted or stopped a newly accepted turn")
			}
		} else if stopped || sendErr != nil || turns != 1 || writes.Load() != 1 {
			t.Fatal("winning admission was retired or lost")
		}
	}
}

func TestClaudeIdleRetirementFencesWriterWaitingBeforeAdmission(t *testing.T) {
	a, p, _ := newClaudeResultFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var validations atomic.Int32
	opts := TurnOptions{ValidatePermission: func() error {
		if validations.Add(1) == 2 { // inside writeTurn, with the writer held
			close(entered)
			<-release
		}
		return nil
	}}
	var writes atomic.Int32
	p.stdin = &claudeDeliveryFixtureWriter{write: func(data []byte) (int, error) {
		writes.Add(1)
		return len(data), nil
	}}
	done := make(chan error, 1)
	go func() { done <- a.SendWithOptions(context.Background(), p.id, "synthetic admission", opts) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("sender did not reach admission guard")
	}
	err := a.retireIdleClaudeWorker(p)
	close(release)
	if err != nil {
		t.Fatal("a writer still before admission was treated as accepted work")
	}
	select {
	case err := <-done:
		if err == nil || writes.Load() != 0 {
			t.Fatal("retired worker accepted queued write")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("retired writer did not return")
	}
}

func TestClaudePreciseInterruptFencesNewTurnsBeforeSlowCleanup(t *testing.T) {
	a, p, _ := newClaudeResultFixture(t)
	p.elicitationHook = &claudeElicitationLease{done: make(chan struct{})}
	turn := sendClaudeResultFixture(t, a, p)
	cleanupEntered, releaseCleanup, cleanupDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		p.stopOnce.Do(func() {
			close(cleanupEntered)
			<-releaseCleanup
			p.permissionLease.revoke()
			close(p.stopCh)
		})
		close(cleanupDone)
	}()
	<-cleanupEntered
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	interrupted := make(chan error, 1)
	go func() {
		_, err := a.InterruptTurn(ctx, p.id, turn, "10000000-0000-4000-8000-000000000021")
		interrupted <- err
	}()
	// Cleanup is intentionally stalled, so only the precise-turn CAS can
	// retire this worker. Waiting for stop's pipe close would be too late.
	for {
		p.mu.Lock()
		fenced := p.stopped
		p.mu.Unlock()
		if fenced {
			if approvalSourceOpen(p.permissionLease.done) || approvalSourceOpen(p.elicitationHook.done) {
				close(releaseCleanup)
				<-cleanupDone
				a.onExit(p, nil)
				t.Fatal("interrupt retained a helper grant until slow cleanup")
			}
			break
		}
		select {
		case <-ctx.Done():
			close(releaseCleanup)
			<-cleanupDone
			a.onExit(p, nil)
			t.Fatal("interrupt did not fence admission before slow cleanup")
		case <-time.After(time.Millisecond):
		}
	}
	var writes atomic.Int32
	p.stdin = &claudeDeliveryFixtureWriter{write: func(data []byte) (int, error) {
		writes.Add(1)
		return len(data), nil
	}}
	err := a.Send(context.Background(), p.id, "must remain unsent")
	close(releaseCleanup)
	<-cleanupDone
	a.onExit(p, nil)
	if err == nil || writes.Load() != 0 {
		t.Fatal("interrupt killed a later admitted prompt")
	}
	if err := <-interrupted; err != nil {
		t.Fatalf("precise interrupted worker did not finish: %v", err)
	}
}

func TestClaudeCanceledControlCannotRetireWorker(t *testing.T) {
	a, p, _ := newClaudeResultFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.SendWithOptions(ctx, p.id, "not sent", TurnOptions{Model: "different-model"}); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled model switch not rejected")
	}
	turn := sendClaudeResultFixture(t, a, p)
	if _, err := a.InterruptTurn(ctx, p.id, turn, "10000000-0000-4000-8000-000000000022"); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled interrupt not rejected")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped || p.turns != 1 {
		t.Fatal("canceled control changed active work")
	}
}
