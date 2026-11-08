package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/usageledger"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func dispatchFixture(t *testing.T) *Handler {
	t.Helper()
	a := config.LocalAccount{ID: "official", Kind: "codex", SourceType: "official_account", CredentialRef: "synthetic-ref", CredentialEpoch: 1, BaseURL: "https://chatgpt.com/backend-api/codex", Model: "chosen-model", Models: []string{"chosen-model"}, Protocol: "responses"}
	return New(func() string { return localTestKey }, func(string) (config.LocalAccount, bool) { return a, true })
}

func TestBeforeDispatchRechecksAuthorizationAfterAdmissionAndReleasesOnReject(t *testing.T) {
	h := dispatchFixture(t)
	var changed atomic.Bool
	h.Key = func() string {
		if changed.Load() {
			return "rotated-synthetic-key"
		}
		return localTestKey
	}
	entered, resume := make(chan struct{}), make(chan struct{})
	var calls, released atomic.Int32
	h.BeforeDispatch = func(_ context.Context, a config.LocalAccount, body []byte) (func(), int) {
		if a.CredentialRef != "synthetic-ref" || !strings.Contains(string(body), "image_generation") {
			t.Error("admission lost fixed account/request")
		}
		close(entered)
		<-resume
		return func() { released.Add(1) }, 0
	}
	h.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
		calls.Add(1)
		return nil, nil
	}
	r := httptest.NewRequest(http.MethodPost, "/gateway/codex/v1/responses", strings.NewReader(`{"model":"chosen-model","input":"synthetic","tools":[{"type":"image_generation"}]}`))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Authorization", "Bearer "+localTestKey)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); h.ServeHTTP(w, r) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("dispatch admission not reached")
	}
	changed.Store(true)
	close(resume)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("dispatch did not finish")
	}
	if w.Code != 409 || calls.Load() != 0 || released.Load() != 1 {
		t.Fatal("waiting resources resurrected revoked authority", w.Code, calls.Load(), released.Load())
	}
}

func TestBeforeDispatchWebSocketWarmupSkipsAndRealTurnOwnsOneRelease(t *testing.T) {
	h := dispatchFixture(t)
	var admitted, released, calls atomic.Int32
	done := make(chan struct{})
	h.BeforeDispatch = func(context.Context, config.LocalAccount, []byte) (func(), int) {
		admitted.Add(1)
		return func() { released.Add(1); close(done) }, 0
	}
	h.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wsCompletedFixture))}, nil
	}
	c := wsFixture(t, h)
	wsSend(t, c, map[string]any{"type": "response.create", "model": "chosen-model", "generate": false})
	warm := wsUntil(t, c, "response.completed")["response"].(map[string]any)
	if admitted.Load() != 0 || calls.Load() != 0 {
		t.Fatal("warmup acquired inference resources")
	}
	wsSend(t, c, map[string]any{"type": "response.create", "previous_response_id": warm["id"], "input": "synthetic"})
	wsUntil(t, c, "response.completed")
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("real turn did not release execution resources")
	}
	if admitted.Load() != 1 || released.Load() != 1 || calls.Load() != 1 {
		t.Fatal("inner WebSocket admission duplicated", admitted.Load(), released.Load(), calls.Load())
	}
}

func TestBeforeDispatchAccountSelectsExactWebSocketTurnAndMarksSDKContext(t *testing.T) {
	h := dispatchFixture(t)
	selected := config.LocalAccount{ID: "official-image", Kind: "codex", SourceType: "official_account", CredentialRef: "synthetic-image-ref", CredentialEpoch: 2, BaseURL: "https://chatgpt.com/backend-api/codex", Model: "chosen-model", Models: []string{"chosen-model"}, Protocol: "responses"}
	var admitted, released, calls atomic.Int32
	h.BeforeDispatchAccount = func(_ context.Context, initial config.LocalAccount, body []byte) (config.LocalAccount, func() bool, func(), int) {
		if initial.ID != "official" || !strings.Contains(string(body), "image_generation") {
			t.Error("turn selector lost original authorization or normalized body")
		}
		admitted.Add(1)
		return selected, func() bool { return true }, func() { released.Add(1) }, 0
	}
	h.Official = func(ctx context.Context, a config.LocalAccount, _ []byte, _ bool, _ bool, current func() bool) (*http.Response, error) {
		calls.Add(1)
		if a.ID != selected.ID || !current() || !cliproxyexecutor.DownstreamWebsocket(ctx) {
			t.Error("selected WebSocket account or SDK transport marker was lost")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wsCompletedFixture))}, nil
	}
	observed := make(chan usageledger.Observation, 1)
	observationPending, completeObservation := context.WithCancel(context.Background())
	defer completeObservation()
	h.Observe = func(o usageledger.Observation) {
		observed <- o
		// Deliberately hold the turn in its final observation. A terminal
		// provider frame is already visible, but the selected lease must still
		// belong to this turn until the inner HTTP handler finishes.
		<-observationPending.Done()
	}
	c := wsFixture(t, h)
	wsSend(t, c, map[string]any{"type": "response.create", "model": "chosen-model", "input": "fixture", "tools": []any{map[string]any{"type": "image_generation"}}})
	wsUntil(t, c, "response.completed")
	select {
	case o := <-observed:
		if o.AccountID != selected.ID {
			t.Fatalf("selected account was not recorded: %+v", o)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("selected WebSocket turn was not observed")
	}
	if released.Load() != 0 {
		t.Fatal("selected account released before the turn's final observation completed")
	}
	completeObservation()
	// The socket loop cannot process this local warmup until the real turn
	// returns from ServeHTTP and releases its selected account. Unlike merely
	// receiving response.completed (or Observe), this is a completion fence.
	// Warmup must not acquire another lease or call the provider.
	wsSend(t, c, map[string]any{"type": "response.create", "model": "chosen-model", "generate": false})
	wsUntil(t, c, "response.completed")
	if admitted.Load() != 1 || released.Load() != 1 || calls.Load() != 1 {
		t.Fatalf("per-turn selector ownership mismatch: admitted=%d released=%d calls=%d", admitted.Load(), released.Load(), calls.Load())
	}
}
