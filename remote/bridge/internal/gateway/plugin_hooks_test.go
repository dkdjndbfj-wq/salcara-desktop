package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/pluginhook"
)

func attachPluginFixture(t *testing.T, h *Handler, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	u, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(u.Port())
	h.Plugins = &pluginhook.Hooks{}
	if err := h.Plugins.Configure(port, "synthetic-plugin-gateway-token"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Plugins.Disconnect("synthetic-plugin-gateway-token") })
}
func TestPluginsTransformOnlyAuthorizedLocalPromptsAndJSONResponses(t *testing.T) {
	for _, kind := range []string{"codex", "claude"} {
		t.Run(kind, func(t *testing.T) {
			protocol, route := "responses", "responses"
			if kind == "claude" {
				protocol, route = "anthropic", "messages"
			}
			var requestCalls, responseCalls atomic.Int32
			h := fixtureGateway(t, kind, protocol, `{"id":"answer","model":"chosen-model","output":[],"content":[]}`, "application/json", func(r *http.Request, in map[string]any) {
				if in["model"] != "chosen-model" || in["instructions"] != "plugin prompt" || r.Header.Get("X-Salcara-Plugin-Note") != "enabled" {
					t.Errorf("upstream did not receive safe mutation: %v", in)
				}
			})
			attachPluginFixture(t, h, func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				if strings.Contains(string(raw), localTestKey) || strings.Contains(string(raw), upstreamTestKey) {
					t.Error("credential reached plugin")
				}
				switch r.URL.Path {
				case "/plugin/hook/request":
					requestCalls.Add(1)
					fmt.Fprint(w, `{"body":{"model":"denied","instructions":"plugin prompt","input":"test","messages":[],"tools":[{"type":"image_generation"}]},"headers":{"Authorization":"wrong","X-Salcara-Plugin-Note":"enabled"}}`)
				case "/plugin/hook/response":
					responseCalls.Add(1)
					fmt.Fprint(w, `{"status":200,"body":{"id":"answer","model":"chosen-model","output":[],"content":[],"plugin":"processed"}}`)
				default:
					fmt.Fprint(w, `{}`)
				}
			})
			w := callGateway(h, kind, route, `{"model":"chosen-model","input":"original","messages":[],"tools":[]}`)
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"plugin":"processed"`) || requestCalls.Load() != 1 || responseCalls.Load() != 1 {
				t.Fatalf("hook chain: %d %s request=%d response=%d", w.Code, w.Body, requestCalls.Load(), responseCalls.Load())
			}
			denied := callGateway(h, kind, route, `{"model":"unknown","input":"private denied prompt"}`)
			if denied.Code != 400 || requestCalls.Load() != 1 {
				t.Fatal("unauthorized model reached plugin")
			}
		})
	}
}
func TestPluginsNeverRunForRejectedCredentialsOrRemoteGateway(t *testing.T) {
	h := fixtureGateway(t, "codex", "responses", `{"id":"r","output":[]}`, "application/json", nil)
	var calls atomic.Int32
	attachPluginFixture(t, h, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, `{}`) })
	r := httptest.NewRequest(http.MethodPost, "/gateway/codex/v1/responses", strings.NewReader(`{"model":"chosen-model","input":"private"}`))
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 || calls.Load() != 0 {
		t.Fatal("unauthorized hook")
	}
	w = callGateway(h, "remote-codex", "responses", `{"model":"chosen-model","input":"remote"}`)
	if w.Code != 200 || calls.Load() != 0 {
		t.Fatalf("remote ran hook: %d %d", w.Code, calls.Load())
	}
}
func TestPluginStreamObservationKeepsSSETransportAndUsage(t *testing.T) {
	stream := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"model\":\"chosen-model\"}}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"model\":\"chosen-model\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n\n"
	h := fixtureGateway(t, "codex", "responses", stream, "text/event-stream", nil)
	events := make(chan string, 20)
	var responses atomic.Int32
	attachPluginFixture(t, h, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/plugin/hook/response" {
			responses.Add(1)
		}
		if r.URL.Path == "/plugin/event" {
			var envelope struct {
				Event string `json:"event"`
			}
			_ = json.Unmarshal(raw, &envelope)
			events <- envelope.Event
		}
		fmt.Fprint(w, `{}`)
	})
	w := callGateway(h, "codex", "responses", `{"model":"chosen-model","input":"hello","stream":true}`)
	if w.Code != 200 || !w.Flushed || !strings.Contains(w.Body.String(), "response.completed") || responses.Load() != 0 {
		t.Fatalf("stream altered: %d %s", w.Code, w.Body)
	}
	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for !seen["stream"] || !seen["usage"] {
		select {
		case event := <-events:
			seen[event] = true
		case <-deadline:
			t.Fatalf("missing events: %v", seen)
		}
	}
}
func TestPluginWebSocketWarmupIsFreeAndRealTurnKeepsContext(t *testing.T) {
	var requests atomic.Int32
	h := fixtureGateway(t, "codex", "responses", wsCompletedFixture, "text/event-stream", func(_ *http.Request, in map[string]any) {
		raw, _ := json.Marshal(in)
		if in["instructions"] != "plugin instruction" || in["stream"] != true || !strings.Contains(string(raw), "original question") {
			t.Errorf("WS prompt or transport lost: %s", raw)
		}
	})
	attachPluginFixture(t, h, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/plugin/hook/request" {
			requests.Add(1)
			var payload struct {
				Body map[string]any `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			payload.Body["instructions"] = "plugin instruction"
			payload.Body["stream"] = false
			payload.Body["model"] = "forbidden"
			_ = json.NewEncoder(w).Encode(map[string]any{"body": payload.Body})
		} else {
			fmt.Fprint(w, `{}`)
		}
	})
	c := wsFixture(t, h)
	wsSend(t, c, map[string]any{"type": "response.create", "model": "chosen-model", "generate": false, "input": []any{map[string]any{"role": "user", "content": "original question"}}})
	warm := wsUntil(t, c, "response.completed")["response"].(map[string]any)
	if requests.Load() != 0 {
		t.Fatal("warmup invoked request hook")
	}
	wsSend(t, c, map[string]any{"type": "response.create", "model": "chosen-model", "previous_response_id": warm["id"], "input": []any{}})
	result := wsUntil(t, c, "response.completed")["response"].(map[string]any)
	if requests.Load() != 1 || result["id"] == "" {
		t.Fatal("real turn did not complete through hook")
	}
}
