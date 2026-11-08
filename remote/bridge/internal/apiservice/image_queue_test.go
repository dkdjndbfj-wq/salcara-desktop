package apiservice

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitImageQueue(t *testing.T, q *imageQueue, inflight, queued int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		i, n := q.snapshot()
		if i == inflight && n == queued {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("image queue did not reach expected state")
}

func TestImageQueueSharedCredentialCancellationAndIndependentAccount(t *testing.T) {
	q := new(imageQueue)
	release, err := q.acquire(context.Background(), "shared-upstream", 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	queued := make(chan error, 1)
	go func() { _, err := q.acquire(ctx, "shared-upstream", 1); queued <- err }()
	waitImageQueue(t, q, 1, 1)
	other, err := q.acquire(context.Background(), "other-upstream", 1)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if !errors.Is(<-queued, context.Canceled) {
		t.Fatal("cancelled queued image remained active")
	}
	other()
	release()
	release()
	waitImageQueue(t, q, 0, 0)
	if len(q.budgets) != 0 {
		t.Fatal("idle image identities retained")
	}
}

func TestImageQueueParallelJobsNeverExceedPerCredentialLimit(t *testing.T) {
	q := new(imageQueue)
	var running, peak atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := q.acquire(context.Background(), "shared-upstream", 2)
			if err != nil {
				t.Error(err)
				return
			}
			n := running.Add(1)
			for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
			}
			time.Sleep(time.Millisecond)
			running.Add(-1)
			release()
		}()
	}
	wg.Wait()
	if peak.Load() > 2 || peak.Load() == 0 {
		t.Fatal("per-credential image slots bypassed")
	}
	waitImageQueue(t, q, 0, 0)
}

func TestImageQueueCancellationWinsAlreadyGrantedWakeup(t *testing.T) {
	for i := 0; i < 100; i++ {
		q := new(imageQueue)
		release, _ := q.acquire(context.Background(), "fixture", 1)
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() {
			next, err := q.acquire(ctx, "fixture", 1)
			if next != nil {
				next()
			}
			result <- err
		}()
		waitImageQueue(t, q, 1, 1)
		cancel()
		release()
		if !errors.Is(<-result, context.Canceled) {
			t.Fatal("cancelled grant became executable")
		}
		waitImageQueue(t, q, 0, 0)
	}
}
