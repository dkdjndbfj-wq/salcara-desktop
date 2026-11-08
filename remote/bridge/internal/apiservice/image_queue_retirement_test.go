package apiservice

import (
	"context"
	"testing"
	"time"
)

func TestImageQueueTightenedCeilingCanOnlyRelaxAfterCompleteDrain(t *testing.T) {
	q := new(imageQueue)
	first, err := q.acquire(context.Background(), "fixture-shared", 2)
	if err != nil {
		t.Fatal(err)
	}
	defer first()
	second, err := q.acquire(context.Background(), "fixture-shared", 2)
	if err != nil {
		t.Fatal(err)
	}
	defer second()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := func(limit int) <-chan func() {
		result := make(chan func(), 1)
		go func() {
			release, err := q.acquire(ctx, "fixture-shared", limit)
			if err != nil {
				close(result)
				return
			}
			result <- release
		}()
		t.Cleanup(func() {
			select {
			case release := <-result:
				if release != nil {
					release()
				}
			default:
			}
		})
		return result
	}
	reduced := start(1)
	waitImageQueue(t, q, 2, 1)
	stale := start(4)
	waitImageQueue(t, q, 2, 2)
	first()
	waitImageQueue(t, q, 1, 2)
	second()
	awaitRelease := func(result <-chan func()) func() {
		select {
		case release := <-result:
			if release == nil {
				t.Fatal("image queue failed")
			}
			return release
		case <-time.After(2 * time.Second):
			t.Fatal("image queue failed to drain")
			return nil
		}
	}
	one := awaitRelease(reduced)
	waitImageQueue(t, q, 1, 1)
	one()
	two := awaitRelease(stale)
	waitImageQueue(t, q, 1, 0)
	two()
	waitImageQueue(t, q, 0, 0)
	// A fresh budget now follows the new user-requested relaxed setting.
	freshA, err := q.acquire(context.Background(), "fixture-shared", 4)
	if err != nil {
		t.Fatal(err)
	}
	defer freshA()
	freshB, err := q.acquire(context.Background(), "fixture-shared", 4)
	if err != nil {
		t.Fatal(err)
	}
	defer freshB()
	waitImageQueue(t, q, 2, 0)
}
