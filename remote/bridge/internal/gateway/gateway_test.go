package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
)

const localTestKey = "local-gateway-test-secret"
const upstreamTestKey = "private-upstream-test-secret"

func callGateway(h *Handler, kind, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/gateway/"+kind+"/v1/"+path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:10000"
	r.Header.Set("Authorization", "Bearer "+localTestKey)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func fixtureGateway(t *testing.T, kind, protocol, output, contentType string, inspect func(*http.Request, map[string]any)) *Handler {
	t.Helper()
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var request map[string]any
		if json.Unmarshal(b, &request) != nil {
			t.Errorf("invalid upstream request")
		}
		if strings.Contains(string(b), localTestKey) || strings.Contains(string(b), upstreamTestKey) {
			t.Errorf("credential in request body")
		}
		if r.Header.Get("Authorization") != "Bearer "+upstreamTestKey || r.Header.Get("Cookie") != "" {
			t.Errorf("upstream used client credentials")
		}
		if inspect != nil {
			inspect(r, request)
		}
		w.Header().Set("Content-Type", contentType)
		_, _ = io.WriteString(w, output)
	}))
	t.Cleanup(u.Close)
	a := config.LocalAccount{ID: "universal", Kind: kind, Key: upstreamTestKey, BaseURL: u.URL + "/prefix", Model: "chosen-model", Protocol: protocol, AuthMode: "bearer"}
	return New(func() string { return localTestKey }, func(target string) (config.LocalAccount, bool) { return a, true })
}

func TestCodexToChatNonStreamRetainsModelHistoryAndCustomTools(t *testing.T) {
	output := `{"id":"chat-id","object":"chat.completion","created":1,"model":"chosen-model","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-patch","type":"function","function":{"name":"editor__apply_patch","arguments":"{\"input\":\"*** Begin Patch\\n*** End Patch\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`
	h := fixtureGateway(t, "codex", "chat", output, "application/json", func(r *http.Request, in map[string]any) {
		if r.URL.Path != "/prefix/v1/chat/completions" || in["model"] != "chosen-model" {
			t.Errorf("wrong path/model")
		}
		b, _ := json.Marshal(in)
		for _, text := range []string{"original user context", "original tool result", "editor__apply_patch", "developer instruction", `"input"`} {
			if !strings.Contains(string(b), text) {
				t.Errorf("lost %s", text)
			}
		}
	})
	w := callGateway(h, "codex", "responses", `{"model":"chosen-model","instructions":"developer instruction","input":[{"role":"user","content":[{"type":"input_text","text":"original user context"}]},{"type":"function_call","name":"read","call_id":"old-call","arguments":"{}"},{"type":"function_call_output","call_id":"old-call","output":"original tool result"}],"tools":[{"type":"namespace","name":"editor","tools":[{"type":"custom","name":"apply_patch","description":"apply a patch","format":{"type":"text"}}]}]}`)
	if w.Code != 200 {
		t.Fatalf("%d: %s", w.Code, w.Body)
	}
	var out map[string]any
	if json.Unmarshal(w.Body.Bytes(), &out) != nil || out["model"] != "chosen-model" || out["object"] != "response" {
		t.Fatalf("wrong downstream response: %s", w.Body)
	}
	for _, text := range []string{`"custom_tool_call"`, `"apply_patch"`, `"namespace":"editor"`, "*** Begin Patch", `"input_tokens":11`} {
		if !strings.Contains(w.Body.String(), text) {
			t.Errorf("lost downstream %s: %s", text, w.Body)
		}
	}
}

func TestCodexToChatStreamCompletesOnlyAfterDoneAndKeepsUsage(t *testing.T) {
	chunks := `data: {"id":"chat-id","object":"chat.completion.chunk","created":1,"model":"chosen-model","choices":[{"index":0,"delta":{"role":"assistant","content":"Grok-compatible reply"},"finish_reason":null}]}

data: {"id":"chat-id","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"id":"chat-id","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}

data: [DONE]

`
	h := fixtureGateway(t, "codex", "chat", chunks, "text/event-stream", nil)
	w := callGateway(h, "codex", "responses", `{"model":"chosen-model","stream":true,"input":"hello"}`)
	for _, text := range []string{"response.completed", "Grok-compatible reply", `"input_tokens":11`, `"output_tokens":7`} {
		if !strings.Contains(w.Body.String(), text) {
			t.Errorf("lost %s: %s", text, w.Body)
		}
	}
	if w.Code != 200 || strings.Contains(w.Body.String(), "upstream_stream_error") {
		t.Fatalf("%d: %s", w.Code, w.Body)
	}
}

const anthropicFixtureSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg-fixture","type":"message","role":"assistant","model":"chosen-model","content":[],"usage":{"input_tokens":12,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Claude-compatible reply"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":8}}

event: message_stop
data: {"type":"message_stop"}

`

func TestCodexToAnthropicStreamAndNonStream(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(map[bool]string{true: "stream", false: "nonstream"}[stream], func(t *testing.T) {
			h := fixtureGateway(t, "codex", "anthropic", anthropicFixtureSSE, "text/event-stream", func(r *http.Request, in map[string]any) {
				if r.URL.Path != "/prefix/v1/messages" || in["model"] != "chosen-model" || in["stream"] != true || r.Header.Get("anthropic-version") == "" {
					t.Errorf("incorrect Anthropic conversion")
				}
			})
			body := `{"model":"chosen-model","input":"hello","stream":false}`
			if stream {
				body = strings.Replace(body, "false", "true", 1)
			}
			w := callGateway(h, "codex", "responses", body)
			if w.Code != 200 || !strings.Contains(w.Body.String(), "Claude-compatible reply") || strings.Contains(w.Body.String(), "upstream_stream_error") {
				t.Fatalf("%d: %s", w.Code, w.Body)
			}
			if stream && !strings.Contains(w.Body.String(), "response.completed") {
				t.Fatalf("missing terminal event: %s", w.Body)
			}
			if !stream && !json.Valid(w.Body.Bytes()) {
				t.Fatalf("invalid response: %s", w.Body)
			}
		})
	}
}

func TestClaudeToChatAndResponses(t *testing.T) {
	for _, protocol := range []string{"chat", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			out := `{"id":"chat-id","object":"chat.completion","model":"chosen-model","choices":[{"index":0,"message":{"role":"assistant","content":"reply"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`
			contentType := "application/json"
			if protocol == "responses" {
				out = `event: response.completed` + "\n" + `data: {"type":"response.completed","response":{"id":"resp-id","object":"response","status":"completed","model":"chosen-model","output":[{"id":"msg","type":"message","role":"assistant","content":[{"type":"output_text","text":"reply"}]}],"usage":{"input_tokens":2,"output_tokens":1}}}` + "\n\n"
				contentType = "text/event-stream"
			}
			h := fixtureGateway(t, "claude", protocol, out, contentType, func(r *http.Request, in map[string]any) {
				if in["model"] != "chosen-model" {
					t.Error("model changed")
				}
				b, _ := json.Marshal(in)
				if !strings.Contains(string(b), "history to preserve") || !strings.Contains(string(b), "Read") {
					t.Error("lost history/tools")
				}
			})
			w := callGateway(h, "claude", "messages", `{"model":"chosen-model","max_tokens":100,"system":"Keep this context","messages":[{"role":"user","content":"history to preserve"}],"tools":[{"name":"Read","description":"read","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}]}`)
			if w.Code != 200 || !json.Valid(w.Body.Bytes()) || !strings.Contains(w.Body.String(), "reply") || !strings.Contains(w.Body.String(), `"type":"message"`) {
				t.Fatalf("%d: %s", w.Code, w.Body)
			}
		})
	}
}

func TestNativeAnthropicNamedSSEPreserved(t *testing.T) {
	h := fixtureGateway(t, "claude", "anthropic", anthropicFixtureSSE, "text/event-stream", nil)
	w := callGateway(h, "claude", "messages", `{"model":"chosen-model","stream":true,"messages":[]}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "event: message_start") || !strings.Contains(w.Body.String(), "event: message_stop") || strings.Contains(w.Body.String(), "upstream_stream_error") {
		t.Fatalf("%d: %s", w.Code, w.Body)
	}
}

func TestGatewayRejectsUnknownModelsAndPreviousIDWithoutFallback(t *testing.T) {
	calls := 0
	h := fixtureGateway(t, "codex", "chat", `{}`, "application/json", func(*http.Request, map[string]any) { calls++ })
	for _, body := range []string{`{"model":"unauthorized-model","input":"hi"}`, `{"model":"chosen-model","previous_response_id":"old-id","input":"hi"}`} {
		w := callGateway(h, "codex", "responses", body)
		if w.Code != 400 || calls != 0 {
			t.Fatal("unsupported request sent upstream")
		}
	}
}

func TestGatewayLoopbackAuthenticationAndSourceInvalidation(t *testing.T) {
	h := New(func() string { return localTestKey }, func(string) (config.LocalAccount, bool) { return config.LocalAccount{}, false })
	for _, tc := range []struct {
		address, origin, key string
		status               int
	}{{"10.1.1.1:10", "", localTestKey, 403}, {"127.0.0.1:10", "https://evil.test", localTestKey, 403}, {"127.0.0.1:10", "", "wrong", 401}, {"127.0.0.1:10", "", localTestKey, 409}} {
		r := httptest.NewRequest("GET", "/gateway/codex/v1/models", nil)
		r.RemoteAddr = tc.address
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("Authorization", "Bearer "+tc.key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%d != %d", w.Code, tc.status)
		}
	}
}

func TestTruncatedAndErroredStreamsNeverManufactureCompletion(t *testing.T) {
	for _, output := range []string{`data: {"choices":[{"index":0,"delta":{"content":"partial"}}]}` + "\n\n", `data: {"error":{"message":"` + upstreamTestKey + `"}}` + "\n\n"} {
		h := fixtureGateway(t, "codex", "chat", output, "text/event-stream", nil)
		w := callGateway(h, "codex", "responses", `{"model":"chosen-model","stream":true,"input":"hi"}`)
		if strings.Contains(w.Body.String(), "response.completed") || strings.Contains(w.Body.String(), upstreamTestKey) || !strings.Contains(w.Body.String(), "upstream_stream_error") {
			t.Fatalf("bad truncated stream: %s", w.Body)
		}
	}
}
