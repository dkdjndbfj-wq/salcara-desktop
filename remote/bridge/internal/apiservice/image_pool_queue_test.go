package apiservice

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestImageQueueAnyChoosesLeastJobsAndPreservesOverlappingFIFO(t *testing.T) {
	q := new(imageQueue)
	key, holdA, err := q.acquireAny(context.Background(), []string{"a"}, 1)
	if err != nil || key != "a" {
		t.Fatal(key, err)
	}
	key, holdB, err := q.acquireAny(context.Background(), []string{"a", "b"}, 1)
	if err != nil || key != "b" {
		t.Fatalf("least active image member not selected: %q %v", key, err)
	}

	first := make(chan string, 1)
	second := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		key, release, acquireErr := q.acquireAny(ctx, []string{"a", "b"}, 1)
		if acquireErr != nil {
			first <- "error"
			return
		}
		first <- key
		release()
	}()
	waitImageQueue(t, q, 2, 1)
	go func() {
		key, release, acquireErr := q.acquireAny(ctx, []string{"b"}, 1)
		if acquireErr != nil {
			second <- "error"
			return
		}
		second <- key
		release()
	}()
	waitImageQueue(t, q, 2, 2)
	holdB()
	if got := <-first; got != "b" {
		t.Fatalf("later overlapping waiter bypassed FIFO: first=%q", got)
	}
	if got := <-second; got != "b" {
		t.Fatalf("second waiter did not follow released first: %q", got)
	}
	holdA()
	waitImageQueue(t, q, 0, 0)
}

func TestImageQueueAnyCancellationDoesNotConsumeOtherMember(t *testing.T) {
	q := new(imageQueue)
	_, releaseA, _ := q.acquireAny(context.Background(), []string{"a"}, 1)
	_, releaseB, _ := q.acquireAny(context.Background(), []string{"b"}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, _, err := q.acquireAny(ctx, []string{"a", "b"}, 1); done <- err }()
	waitImageQueue(t, q, 2, 1)
	cancel()
	if !errors.Is(<-done, context.Canceled) {
		t.Fatal("cancelled image-pool waiter remained active")
	}
	releaseA()
	releaseB()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if inflight, queued := q.snapshot(); inflight == 0 && queued == 0 {
			return
		}
	}
	t.Fatal("cancelled image pool did not drain")
}
