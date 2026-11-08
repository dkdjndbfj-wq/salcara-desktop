package agents

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/protocol"
)

func sideStateSnapshot(reading string) SideQuestionSnapshot {
	return SideQuestionSnapshot{Revision: reading, Messages: []SideQuestionMessage{{ID: "message", Role: "user", Text: "loaded context"}}}
}

func TestSideQuestionStateIndependentReadingsDoNotReplaceOtherPanels(t *testing.T) {
	var registry sideQuestionRegistry
	ctx, key := context.Background(), "claude:parent"
	binding := sideQuestionBinding{Source: "source", Runtime: new(int)}
	guard := func() error { return nil }
	var calls int
	run := func(_ context.Context, input sideQuestionInput) (sideQuestionAnswer, bool, error) {
		calls++
		return sideQuestionAnswer{Text: input.Question}, true, nil
	}
	a, b := sideStateSnapshot("panel-a"), sideStateSnapshot("panel-b")
	viewA, err := registry.catalog(ctx, key, a, binding, guard)
	if err != nil || !viewA.Available {
		t.Fatal(err)
	}
	viewB, err := registry.catalog(ctx, key, b, binding, guard)
	if err != nil || !viewB.Available || viewA.Revision == viewB.Revision {
		t.Fatal("panels shared a revision")
	}
	reqA, reqB := sideFixtureRequest(viewA), sideFixtureRequest(viewB)
	if result, err := registry.ask(ctx, key, a, reqA, binding, guard, run, nil); err != nil || result.State != "completed" {
		t.Fatalf("panel a lost: %+v %v", result, err)
	}
	if result, err := registry.ask(ctx, key, b, reqB, binding, guard, run, nil); err != nil || result.State != "completed" {
		t.Fatalf("panel b lost: %+v %v", result, err)
	}
	if calls != 2 {
		t.Fatal("wrong inference count")
	}
	if _, err := registry.ask(ctx, key, b, reqA, binding, guard, run, nil); !errors.Is(err, errSideQuestionStale) {
		t.Fatal("cross-panel snapshot accepted")
	}
}

func TestSideQuestionStateExpiresReclaimsCapacityAndNeverReusesRevision(t *testing.T) {
	var registry sideQuestionRegistry
	ctx, key := context.Background(), "claude:parent"
	binding, guard := sideQuestionBinding{Source: "source"}, func() error { return nil }
	views := make([]protocol.SideQuestionCatalog, maxSideQuestionSessions)
	for i := range views {
		view, err := registry.catalog(ctx, key, sideStateSnapshot(fmt.Sprintf("reading-%d", i)), binding, guard)
		if err != nil || !view.Available {
			t.Fatalf("catalog %d: %+v %v", i, view, err)
		}
		views[i] = view
	}
	view, err := registry.catalog(ctx, key, sideStateSnapshot("overflow"), binding, guard)
	if err != nil || view.Available {
		t.Fatal("unbounded registry")
	}
	registry.mu.Lock()
	for _, state := range registry.sessions {
		state.created = time.Now().Add(-sideQuestionLifetime - time.Second)
	}
	// An outstanding HTTP request cannot be evicted even when its catalog is old.
	registry.sessions[views[0].Revision].pending = &sideQuestionOperation{}
	registry.mu.Unlock()
	view, err = registry.catalog(ctx, key, sideStateSnapshot("overflow"), binding, guard)
	if err != nil || !view.Available || len(registry.sessions) != 2 || registry.sessions[views[0].Revision] == nil {
		t.Fatalf("expired capacity/pending fence: %+v %v", view, err)
	}
	old := sideFixtureRequest(views[1])
	called := false
	run := func(context.Context, sideQuestionInput) (sideQuestionAnswer, bool, error) {
		called = true
		return sideQuestionAnswer{Text: "answer"}, true, nil
	}
	if _, err = registry.ask(ctx, key, sideStateSnapshot("reading-1"), old, binding, guard, run, nil); !errors.Is(err, errSideQuestionStale) || called {
		t.Fatal("expired request recreated a chain")
	}
	refreshed, err := registry.catalog(ctx, key, sideStateSnapshot("reading-1"), binding, guard)
	if err != nil || !refreshed.Available || refreshed.Revision == views[1].Revision {
		t.Fatal("reclaimed revision reused")
	}
}

func TestSideQuestionStateSixTurnsContextBoundsAndExpiry(t *testing.T) {
	var registry sideQuestionRegistry
	ctx, key, snapshot := context.Background(), "claude:parent", sideStateSnapshot("reading")
	binding, guard := sideQuestionBinding{Source: "source"}, func() error { return nil }
	view, _ := registry.catalog(ctx, key, snapshot, binding, guard)
	var calls int
	run := func(context.Context, sideQuestionInput) (sideQuestionAnswer, bool, error) {
		calls++
		return sideQuestionAnswer{Text: "answer"}, true, nil
	}
	previous := ""
	for i := 0; i < maxSideQuestionTurns; i++ {
		request := sideFixtureRequest(view)
		request.PreviousOperationID = previous
		result, err := registry.ask(ctx, key, snapshot, request, binding, guard, run, nil)
		if err != nil || result.State != "completed" {
			t.Fatalf("turn %d: %+v %v", i, result, err)
		}
		previous = request.OperationID
	}
	request := sideFixtureRequest(view)
	request.PreviousOperationID = previous
	if _, err := registry.ask(ctx, key, snapshot, request, binding, guard, run, nil); !errors.Is(err, errSideQuestionLimit) || calls != maxSideQuestionTurns {
		t.Fatal("seventh turn reached transport")
	}
	registry.mu.Lock()
	registry.sessions[view.Revision].created = time.Now().Add(-sideQuestionLifetime - time.Second)
	registry.mu.Unlock()
	if _, err := registry.ask(ctx, key, snapshot, request, binding, guard, run, nil); !errors.Is(err, errSideQuestionExpired) {
		t.Fatal("expired chain accepted")
	}
	big := sideStateSnapshot("large-reading")
	big.Messages[0].Text = strings.Repeat("x", maxSideQuestionSnapshot)
	largeView, _ := registry.catalog(ctx, key, big, binding, guard)
	request = sideFixtureRequest(largeView)
	start, end := int64(0), int64(32<<10)
	request.Selection = &protocol.ContextReference{Kind: "message", SessionKey: key, MessageID: "older-message", Label: "Quote", Text: strings.Repeat("q", int(end)), StartOffset: &start, EndOffset: &end, CapturedAt: 1}
	if _, err := registry.ask(ctx, key, big, request, binding, guard, run, nil); !errors.Is(err, errSideQuestionLimit) || calls != maxSideQuestionTurns || registry.activeTurns() {
		t.Fatal("oversized retained chain reached transport")
	}
}

func TestSideQuestionStateRegistersPendingBeforeFinalGuardAndReleasesDefiniteFailure(t *testing.T) {
	var registry sideQuestionRegistry
	ctx, key, snapshot := context.Background(), "claude:parent", sideStateSnapshot("reading")
	binding := sideQuestionBinding{Source: "source"}
	view, _ := registry.catalog(ctx, key, snapshot, binding, func() error { return nil })
	request := sideFixtureRequest(view)
	var guards, calls int
	guard := func() error {
		guards++
		if guards == 2 {
			if !registry.activeTurns() {
				t.Error("API switch could miss the reserved request")
			}
			return errSideQuestionStale
		}
		return nil
	}
	run := func(context.Context, sideQuestionInput) (sideQuestionAnswer, bool, error) {
		calls++
		return sideQuestionAnswer{Text: "answer"}, true, nil
	}
	if _, err := registry.ask(ctx, key, snapshot, request, binding, guard, run, nil); !errors.Is(err, errSideQuestionStale) || calls != 0 || registry.activeTurns() {
		t.Fatal("definitely unsent request retained pending state")
	}
	if result, err := registry.ask(ctx, key, snapshot, request, binding, func() error { return nil }, run, nil); err != nil || result.State != "completed" || calls != 1 {
		t.Fatalf("unsent reservation poisoned receipt: %+v %v", result, err)
	}
}

func TestSideQuestionStateReceiptRechecksGuardAfterRegistryWait(t *testing.T) {
	var registry sideQuestionRegistry
	ctx, key, snapshot := context.Background(), "claude:parent", sideStateSnapshot("reading")
	binding := sideQuestionBinding{Source: "source"}
	guard := func() error { return nil }
	view, _ := registry.catalog(ctx, key, snapshot, binding, guard)
	request := sideFixtureRequest(view)
	var calls atomic.Int32
	run := func(context.Context, sideQuestionInput) (sideQuestionAnswer, bool, error) {
		calls.Add(1)
		return sideQuestionAnswer{Text: "private answer"}, true, nil
	}
	if _, err := registry.ask(ctx, key, snapshot, request, binding, guard, run, nil); err != nil {
		t.Fatal(err)
	}
	var revoked atomic.Bool
	entered := make(chan struct{}, 1)
	guard = func() error {
		select {
		case entered <- struct{}{}:
		default:
		}
		if revoked.Load() {
			return errSideQuestionStale
		}
		return nil
	}
	registry.mu.Lock()
	done := make(chan error, 1)
	go func() { _, err := registry.ask(ctx, key, snapshot, request, binding, guard, run, nil); done <- err }()
	<-entered
	revoked.Store(true)
	registry.mu.Unlock()
	if err := <-done; !errors.Is(err, errSideQuestionStale) || calls.Load() != 1 {
		t.Fatal("receipt replay ignored guard change during registry wait")
	}
}

func TestSideQuestionGatewayRejectsUnmanagedEndpointsBeforeDispatch(t *testing.T) {
	for _, endpoint := range []string{"https://api.anthropic.com/v1/messages", "http://localhost:47831/gateway/claude/v1/messages", "http://127.0.0.1:47831/v1/messages", "http://127.0.0.1:47831/gateway/claude/v1/messages?key=secret", "http://user@127.0.0.1:47831/gateway/claude/v1/messages", "http://127.0.0.1:047831/gateway/claude/v1/messages", "http://127.0.0.1:47831/gateway/claude/v1/%6dessages"} {
		if _, attempted, err := sideQuestionGatewayRequest(context.Background(), endpoint, "synthetic", []byte(`{}`)); err == nil || attempted {
			t.Fatalf("unmanaged endpoint accepted: %s", endpoint)
		}
	}
}
