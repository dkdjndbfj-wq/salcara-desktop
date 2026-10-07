package launcher

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestInventoryCoalescesAndReturnsOwnedCopies(t *testing.T) {
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	s := New(t.TempDir())
	s.FindTools = func(context.Context, map[string]string) []Tool {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		return []Tool{{ID: "codex", Available: true}}
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out := s.Inventory(context.Background(), nil)
			if len(out) != 1 {
				t.Error("missing inventory")
			}
			out[0].ID = "mutated"
		}()
	}
	<-started
	close(release)
	wg.Wait()
	if calls.Load() != 1 || s.Inventory(context.Background(), nil)[0].ID != "codex" {
		t.Fatal("duplicate probe or shared mutable inventory")
	}
	s.Inventory(context.Background(), map[string]string{"codex": "new"})
	if calls.Load() != 2 {
		t.Fatal("path change used stale inventory")
	}
}
func TestInventoryCanceledCallerDoesNotCancelOthers(t *testing.T) {
	release := make(chan struct{})
	s := New(t.TempDir())
	s.FindTools = func(ctx context.Context, _ map[string]string) []Tool {
		select {
		case <-release:
			return []Tool{{ID: "codex"}}
		case <-ctx.Done():
			return nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if len(s.Inventory(ctx, nil)) != 0 {
		t.Fatal("canceled caller waited")
	}
	close(release)
	if len(s.Inventory(context.Background(), nil)) != 1 {
		t.Fatal("caller cancellation killed shared probe")
	}
}
func TestInvalidateDuringFlightCannotRepopulateOldCache(t *testing.T) {
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	s := New(t.TempDir())
	s.FindTools = func(context.Context, map[string]string) []Tool {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			return []Tool{{ID: "old"}}
		}
		return []Tool{{ID: "fresh"}}
	}
	done := make(chan []Tool)
	go func() { done <- s.Inventory(context.Background(), nil) }()
	<-started
	s.InvalidateInventory()
	close(release)
	if (<-done)[0].ID != "fresh" || calls.Load() != 2 {
		t.Fatal("in-flight probe ignored invalidation")
	}
}

func TestInventorySnapshotDoesNotWaitForDiscoveryAndRemembersColdStart(t *testing.T) {
	dir, exe := t.TempDir(), locationFixture(t, "Codex.exe")
	s := New(dir)
	s.rememberLocations([]Tool{{ID: "codex-desktop", Path: exe, Available: true}})
	s = New(dir)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s.FindTools = func(context.Context, map[string]string) []Tool {
		calls.Add(1)
		close(started)
		<-release
		return []Tool{{ID: "codex-desktop", Path: exe, Available: true}}
	}
	got, pending := s.InventorySnapshot(nil)
	if !pending || len(got) != 4 || !got[0].Available || got[0].Path != exe {
		t.Fatal("cold local snapshot missing")
	}
	<-started
	s.inventoryMu.Lock()
	done := s.inventoryFlight.done
	s.inventoryMu.Unlock()
	defer func() { close(release); <-done }()
	for i := 0; i < 8; i++ {
		s.InventorySnapshot(nil)
	}
	if calls.Load() != 1 {
		t.Fatal("discovery was not coalesced")
	}
}
