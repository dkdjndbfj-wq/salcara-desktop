package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"salcara/bridge/internal/config"
)

func wsFixture(t *testing.T, h *Handler) *websocket.Conn {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	header := http.Header{"Authorization": []string{"Bearer " + localTestKey}}
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(s.URL, "http")+"/gateway/codex/v1/responses", header)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
func wsUntil(t *testing.T, c *websocket.Conn, kind string) map[string]any {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var event map[string]any
		if err := c.ReadJSON(&event); err != nil {
			t.Fatal(err)
		}
		if event["type"] == kind {
			return event
		}
		if event["type"] == "error" {
			t.Fatalf("unexpected socket error: %v", event)
		}
	}
}

func TestWebSocketWarmupAndIncrementalContextStayInConnection(t *testing.T) {
	calls := 0
	output := `data: {"id":"socket-fixture","object":"chat.completion.chunk","created":1,"model":"chosen-model","choices":[{"index":0,"delta":{"role":"assistant","content":"original assistant output"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"socket-fixture","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" + "data: [DONE]\n\n"
	h := fixtureGateway(t, "codex", "chat", output, "text/event-stream", func(r *http.Request, in map[string]any) {
		calls++
		b, _ := json.Marshal(in)
		if calls == 2 && (!strings.Contains(string(b), "original question") || !strings.Contains(string(b), "original assistant output") || !strings.Contains(string(b), "follow-up")) {
			t.Errorf("incremental history lost: %s", b)
		}
		if in["previous_response_id"] != nil {
			t.Error("upstream cannot resolve local response ID")
		}
	})
	c := wsFixture(t, h)
	_ = c.WriteJSON(map[string]any{"type": "response.create", "model": "chosen-model", "generate": false, "input": []any{map[string]any{"role": "user", "content": "original question"}}})
	warm := wsUntil(t, c, "response.completed")["response"].(map[string]any)
	if calls != 0 {
		t.Fatal("warmup billed upstream")
	}
	_ = c.WriteJSON(map[string]any{"type": "response.create", "model": "chosen-model", "previous_response_id": warm["id"], "input": []any{}})
	first := wsUntil(t, c, "response.completed")["response"].(map[string]any)
	_ = c.WriteJSON(map[string]any{"type": "response.create", "previous_response_id": first["id"], "input": []any{map[string]any{"role": "user", "content": "follow-up"}}})
	_ = wsUntil(t, c, "response.completed")
	if calls != 2 {
		t.Fatalf("unexpected inference count: %d", calls)
	}
	other := wsFixture(t, h)
	_ = other.WriteJSON(map[string]any{"type": "response.create", "model": "chosen-model", "previous_response_id": first["id"], "input": []any{}})
	errEvent := wsUntil(t, other, "error")
	if errEvent["error"].(map[string]any)["code"] != "previous_response_not_found" || calls != 2 {
		t.Fatal("history crossed sockets")
	}
}

func TestWebSocketRejectsChangedKeyAndCrossOrigin(t *testing.T) {
	a := config.LocalAccount{Kind: "codex", Key: upstreamTestKey, Model: "chosen-model", Protocol: "chat"}
	h := New(func() string { return localTestKey }, func(string) (config.LocalAccount, bool) { return a, true })
	c := wsFixture(t, h)
	a.Key = "changed-key"
	_ = c.WriteJSON(map[string]any{"type": "response.create", "model": "chosen-model", "input": []any{}})
	if wsUntil(t, c, "error")["error"].(map[string]any)["code"] != "configuration_changed" {
		t.Fatal("socket followed changed key")
	}
	s := httptest.NewServer(h)
	defer s.Close()
	for _, header := range []http.Header{{"Authorization": []string{"Bearer " + localTestKey}, "Origin": []string{"https://evil.invalid"}}, {"Authorization": []string{"Bearer wrong"}}} {
		c, r, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(s.URL, "http")+"/gateway/codex/v1/responses", header)
		if c != nil {
			_ = c.Close()
		}
		if err == nil || r == nil || (r.StatusCode != 401 && r.StatusCode != 403) {
			t.Fatal("unauthorized websocket upgraded")
		}
	}
}

const wsCompletedFixture = `event: response.completed
data: {"type":"response.completed","response":{"id":"resp-socket-one","object":"response","status":"completed","model":"chosen-model","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"original assistant output"}]}]}}

`

func wsSend(t *testing.T, c *websocket.Conn, input map[string]any) {
	t.Helper()
	if err := c.WriteJSON(input); err != nil {
		t.Fatal(err)
	}
}

// The Source deliberately starts returning another key after validation. A
// second lookup inside ServeHTTP would expose the old socket context to it.
func TestWebSocketPinsVerifiedAccountAcrossForwardingRace(t *testing.T) {
	var upstreamCalls atomic.Int32
	h := fixtureGateway(t, "codex", "responses", wsCompletedFixture, "text/event-stream", func(r *http.Request, in map[string]any) {
		upstreamCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+upstreamTestKey {
			t.Errorf("old socket context forwarded with a changed credential")
		}
		if in["model"] != "chosen-model" {
			t.Errorf("original model changed")
		}
	})
	original, _ := h.Source("codex")
	changed := original
	changed.Key = "different-upstream-key"
	var sourceCalls atomic.Int32
	h.Source = func(string) (config.LocalAccount, bool) {
		if sourceCalls.Add(1) <= 2 { // upgrade, then this frame's validation
			return original, true
		}
		return changed, true
	}
	c := wsFixture(t, h)
	wsSend(t, c, map[string]any{"type": "response.create", "model": "chosen-model", "input": "original private context"})
	wsUntil(t, c, "response.completed")
	if sourceCalls.Load() != 2 || upstreamCalls.Load() != 1 {
		t.Fatalf("forwarding reread live source: source=%d upstream=%d", sourceCalls.Load(), upstreamCalls.Load())
	}
	wsSend(t, c, map[string]any{"type": "response.create", "model": "chosen-model", "input": "next turn"})
	event := wsUntil(t, c, "error")
	if event["error"].(map[string]any)["code"] != "configuration_changed" || upstreamCalls.Load() != 1 {
		t.Fatal("an already established socket followed the newly selected key")
	}
}

func TestWebSocketFailedTurnInvalidatesPreviousContext(t *testing.T) {
	for _, tc := range []struct {
		name, output, event string
		status              int
	}{
		{"http-error", `{"error":{"message":"fixture failure"}}`, "error", http.StatusBadGateway},
		{"incomplete", "data: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp-incomplete\",\"status\":\"incomplete\",\"output\":[]}}\n\n", "response.incomplete", http.StatusOK},
		{"truncated", "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-truncated\",\"status\":\"in_progress\"}}\n\n", "error", http.StatusOK},
		{"sse-error", "data: {\"type\":\"error\",\"error\":{\"message\":\"fixture failure\"}}\n\n", "error", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				number := calls.Add(1)
				if number == 1 {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, wsCompletedFixture)
					return
				}
				if number != 2 {
					t.Errorf("stale previous-response chain reached upstream: %s", body)
				}
				for _, text := range []string{"original question", "original assistant output", "failed follow-up"} {
					if !strings.Contains(string(body), text) {
						t.Errorf("valid incremental request lost %q: %s", text, body)
					}
				}
				if tc.status == http.StatusOK {
					w.Header().Set("Content-Type", "text/event-stream")
				} else {
					w.Header().Set("Content-Type", "application/json")
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.output)
			}))
			t.Cleanup(u.Close)
			a := config.LocalAccount{Kind: "codex", Key: upstreamTestKey, BaseURL: u.URL, Model: "chosen-model", Protocol: "responses", AuthMode: "bearer"}
			h := New(func() string { return localTestKey }, func(string) (config.LocalAccount, bool) { return a, true })
			c := wsFixture(t, h)
			wsSend(t, c, map[string]any{"type": "response.create", "model": "chosen-model", "input": "original question"})
			first := wsUntil(t, c, "response.completed")["response"].(map[string]any)
			wsSend(t, c, map[string]any{"type": "response.create", "previous_response_id": first["id"], "input": "failed follow-up"})
			wsUntil(t, c, tc.event)
			wsSend(t, c, map[string]any{"type": "response.create", "model": "chosen-model", "previous_response_id": first["id"], "input": "must resend full original context"})
			event := wsUntil(t, c, "error")
			if event["error"].(map[string]any)["code"] != "previous_response_not_found" || calls.Load() != 2 {
				t.Fatal("failed turn reused an obsolete context chain")
			}
		})
	}
}

func TestWebSocketRejectsNonStringPreviousIDWithoutUpstreamCall(t *testing.T) {
	var calls atomic.Int32
	h := fixtureGateway(t, "codex", "responses", wsCompletedFixture, "text/event-stream", func(*http.Request, map[string]any) { calls.Add(1) })
	c := wsFixture(t, h)
	for _, previous := range []any{12, true, []any{"response-id"}, map[string]any{"id": "response-id"}} {
		wsSend(t, c, map[string]any{"type": "response.create", "model": "chosen-model", "previous_response_id": previous, "input": "must not silently discard history"})
		event := wsUntil(t, c, "error")
		if event["error"].(map[string]any)["code"] != "invalid_previous_response_id" {
			t.Fatalf("wrong validation for %T: %v", previous, event)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("malformed previous-response IDs caused inference")
	}
}

// Block the upstream before its first SSE event. A client disconnect must
// cancel this request without needing upstream output to trigger a write error.
func TestWebSocketClosePromptlyCancelsBlockedUpstream(t *testing.T) {
	testWebSocketUpstreamCancellation(t, false)
}

// A full queue must not block the sole reader and prevent disconnect detection.
func TestWebSocketPipeliningClosesAndCancelsBlockedUpstream(t *testing.T) {
	testWebSocketUpstreamCancellation(t, true)
}

func testWebSocketUpstreamCancellation(t *testing.T, pipeline bool) {
	t.Helper()
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var startedOnce, canceledOnce sync.Once
	var calls atomic.Int32
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		startedOnce.Do(func() { close(started) })
		select {
		case <-r.Context().Done():
			canceledOnce.Do(func() { close(canceled) })
		case <-release:
		}
	}))
	a := config.LocalAccount{Kind: "codex", Key: upstreamTestKey, BaseURL: u.URL, Model: "chosen-model", Protocol: "responses", AuthMode: "bearer"}
	h := New(func() string { return localTestKey }, func(string) (config.LocalAccount, bool) { return a, true })
	s := httptest.NewServer(h)
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(s.URL, "http")+"/gateway/codex/v1/responses", http.Header{"Authorization": []string{"Bearer " + localTestKey}})
	// Unblock fixtures first even when testing an older, broken implementation;
	// httptest.Close must not hang while a failed cancellation leaves inference live.
	t.Cleanup(func() {
		close(release)
		if c != nil {
			_ = c.Close()
		}
		s.Close()
		u.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
	wsSend(t, c, map[string]any{"type": "response.create", "model": "chosen-model", "input": "blocked inference"})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream fixture never started")
	}
	if pipeline {
		wsSend(t, c, map[string]any{"type": "response.create", "model": "chosen-model", "input": "queued frame"})
		wsSend(t, c, map[string]any{"type": "response.create", "model": "chosen-model", "input": "queue overflow"})
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _, closeErr := c.ReadMessage()
		if !websocket.IsCloseError(closeErr, websocket.ClosePolicyViolation) {
			t.Fatalf("pipelining did not close with policy violation: %v", closeErr)
		}
	} else {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal(fmt.Sprintf("socket close did not promptly cancel upstream (pipelining=%v)", pipeline))
	}
	if calls.Load() != 1 {
		t.Fatalf("pending frames started additional upstream inference: %d", calls.Load())
	}
}
