package codexruntime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"salcara/bridge/internal/usageledger"
)

func nativeWSFixture(t *testing.T, h http.Handler) *Runtime {
	t.Helper()
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	r := New(t.TempDir())
	r.websocketURL = "ws" + strings.TrimPrefix(server.URL, "http") + "/responses"
	t.Cleanup(func() { _ = r.Close() })
	return r
}
func TestNativeWebsocketUsesSelectedCredentialAndFinalUsage(t *testing.T) {
	var requests atomic.Int32
	r := nativeWSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		requests.Add(1)
		if q.Header.Get("Authorization") != "Bearer synthetic-not-real-access-A" || q.Header.Get("ChatGPT-Account-Id") != "synthetic-provider-A" || q.Header.Get("OpenAI-Beta") != "responses_websockets=2026-02-06" {
			t.Error("selected identity or protocol missing")
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, q, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Error(err)
			return
		}
		var in map[string]any
		_ = json.Unmarshal(raw, &in)
		if in["type"] != "response.create" || in["model"] != "gpt-fixture" || in["previous_response_id"] != "fixture-previous" {
			t.Errorf("request changed: %s", raw)
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"fixture"}}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"fixture","status":"completed","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"late"}`))
	}))
	a := syntheticAuth("A")
	a.Metadata["account_id"] = "synthetic-provider-A"
	resp, err := r.Execute(executor.WithDownstreamWebsocket(context.Background()), a, []byte(`{"model":"gpt-fixture","input":[],"previous_response_id":"fixture-previous"}`), true, false)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || strings.Contains(string(raw), "late") || requests.Load() != 1 {
		t.Fatalf("bad completion: %s %v", raw, err)
	}
	var collector usageledger.Collector
	collector.ObserveSSE(raw)
	if !collector.Final() || collector.Tokens().Total == nil || *collector.Tokens().Total != 8 {
		t.Fatalf("final usage missing: %s", raw)
	}
}
func TestNativeWebsocketRedirectAndCapacityStayPrivate(t *testing.T) {
	var replay atomic.Int32
	trap := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { replay.Add(1) }))
	defer trap.Close()
	for _, status := range []int{302, 307, 401, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			r := nativeWSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
				w.Header().Set("Location", trap.URL)
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":{"code":"model_capacity","message":"synthetic-secret-body"}}`)
			}))
			_, err := r.executeWebSocket(context.Background(), syntheticAuth("A"), []byte(`{"model":"gpt-fixture","input":[]}`))
			if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "synthetic-secret") {
				t.Fatalf("unsafe error: %v", err)
			}
			var safe providerExecutionError
			if status >= 400 && (!errors.As(err, &safe) || safe.StatusCode() != status || safe.ExecutionCode() != "model_capacity") {
				t.Fatalf("lost routing metadata: %v", err)
			}
		})
	}
	if replay.Load() != 0 {
		t.Fatal("credential request followed provider redirect")
	}
}
func TestNativeWebsocketFirstEventErrorsAndMalformedFrame(t *testing.T) {
	for _, raw := range []string{`{"type":"error","status":429,"error":{"code":"model_capacity","message":"synthetic-secret"}}`, `invalid-json`, `{"type":"foreign.created"}`} {
		r := nativeWSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
			conn, err := (&websocket.Upgrader{}).Upgrade(w, q, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			_, _, _ = conn.ReadMessage()
			_ = conn.WriteMessage(websocket.TextMessage, []byte(raw))
		}))
		resp, err := r.executeWebSocket(context.Background(), syntheticAuth("A"), []byte(`{"model":"gpt-fixture","input":[]}`))
		if resp != nil || !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "synthetic-secret") {
			t.Fatalf("bad first event became successful: %v", err)
		}
	}
}
func TestNativeWebsocketCancellationAndRuntimeCloseReleaseConnections(t *testing.T) {
	for _, closeRuntime := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "close"}[closeRuntime], func(t *testing.T) {
			released := make(chan struct{})
			r := nativeWSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
				conn, err := (&websocket.Upgrader{}).Upgrade(w, q, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				_, _, _ = conn.ReadMessage()
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created"}`))
				_, _, _ = conn.ReadMessage()
				close(released)
			}))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			resp, err := r.executeWebSocket(ctx, syntheticAuth("A"), []byte(`{"model":"gpt-fixture","input":[]}`))
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, e := io.ReadAll(resp.Body); done <- e }()
			if closeRuntime {
				_ = r.Close()
			} else {
				cancel()
			}
			select {
			case err = <-done:
				if err == nil {
					t.Fatal("partial stream became successful")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("reader leaked")
			}
			resp.Body.Close()
			waitChannel(t, released)
		})
	}
}
func TestNativeWebsocketLateCapacityIsSafeAndDoesNotComplete(t *testing.T) {
	r := nativeWSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, q, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created"}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","status_code":503,"body":{"error":{"code":"model_capacity","message":"synthetic-private-error"}}}`))
	}))
	resp, err := r.executeWebSocket(context.Background(), syntheticAuth("A"), []byte(`{"model":"gpt-fixture","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !errors.Is(err, ErrUnavailable) || strings.Contains(string(raw), "synthetic-private-error") || !strings.Contains(string(raw), "model_capacity") {
		t.Fatalf("unsafe partial failure %s %v", raw, err)
	}
	var collector usageledger.Collector
	collector.ObserveSSE(raw)
	if collector.State() != "failed" || collector.Final() {
		t.Fatalf("partial error became completion: %s", raw)
	}
}
func TestNativeWebsocketRuntimeCloseCancelsUnconsumedPipe(t *testing.T) {
	released := make(chan struct{})
	r := nativeWSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, q, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created"}`))
		_, _, _ = conn.ReadMessage()
		close(released)
	}))
	resp, err := r.executeWebSocket(context.Background(), syntheticAuth("A"), []byte(`{"model":"gpt-fixture","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	waitChannel(t, released)
	// The producer was blocked on the first pipe write when Runtime closed.
	if _, err = io.ReadAll(resp.Body); err == nil {
		t.Fatal("closed unconsumed stream became successful")
	}
	resp.Body.Close()
}
func TestNativeWebsocket401UsesOwnedRefreshExactlyOnceBeforeOutput(t *testing.T) {
	for _, frameError := range []bool{false, true} {
		t.Run(map[bool]string{false: "handshake", true: "first_frame"}[frameError], func(t *testing.T) {
			var calls, refreshes atomic.Int32
			e, a := handlerFixture(t, "2099-01-01T00:00:00Z", &stubExecutor{refresh: func(_ context.Context, record *coreauth.Auth) (*coreauth.Auth, error) {
				refreshes.Add(1)
				return rotatedSDK(record), nil
			}})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
				n := calls.Add(1)
				expected := "Bearer synthetic-access-old"
				if n > 1 {
					expected = "Bearer synthetic-access-rotated"
				}
				if q.Header.Get("Authorization") != expected {
					t.Error("refresh ownership not preserved")
				}
				if n == 1 && !frameError {
					w.WriteHeader(401)
					return
				}
				conn, err := (&websocket.Upgrader{}).Upgrade(w, q, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				_, _, _ = conn.ReadMessage()
				if n == 1 {
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","status_code":401,"error":{"message":"synthetic-secret"}}`))
					return
				}
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.done","response":{"id":"fixture","status":"completed"}}`))
			}))
			defer server.Close()
			e.Runtime.websocketURL = "ws" + strings.TrimPrefix(server.URL, "http") + "/responses"
			resp, err := e.Do(executor.WithDownstreamWebsocket(context.Background()), a, []byte(`{"model":"gpt-fixture","input":[]}`), true, false, func() bool { return true })
			if err != nil {
				t.Fatal(err)
			}
			raw, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || !strings.Contains(string(raw), "response.completed") || calls.Load() != 2 || refreshes.Load() != 1 {
				t.Fatalf("bad auth retry calls=%d refresh=%d body=%s err=%v", calls.Load(), refreshes.Load(), raw, err)
			}
		})
	}
}
func TestNativeWebsocketDoesNotInheritEnvironmentProxy(t *testing.T) {
	var proxyCalls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { proxyCalls.Add(1) }))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("ALL_PROXY", proxy.URL)
	r := nativeWSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, q, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed"}`))
	}))
	resp, err := r.executeWebSocket(context.Background(), syntheticAuth("A"), []byte(`{"model":"gpt-fixture","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || proxyCalls.Load() != 0 {
		t.Fatal("environment captured protected request", err)
	}
	injected := syntheticAuth("A")
	injected.ProxyURL = proxy.URL
	if _, err = r.executeWebSocket(context.Background(), injected, []byte(`{"model":"gpt-fixture","input":[]}`)); !errors.Is(err, ErrUnavailable) || proxyCalls.Load() != 0 {
		t.Fatal("injected proxy bypassed owner", err)
	}
}
