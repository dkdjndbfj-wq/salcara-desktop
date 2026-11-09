package apiservice

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/usageledger"
)

func TestNativeStreamFlushAndCancellationReleaseAdmission(t *testing.T) {
	s, _, _ := nativeFixture(t)
	observed := make(chan usageledger.Observation, 1)
	s.Observe = func(o usageledger.Observation) { observed <- o }
	stopped := make(chan struct{})
	s.Official = func(ctx context.Context, _ config.LocalAccount, _ []byte, _, _ bool, _ func() bool) (*http.Response, error) {
		reader, writer := io.Pipe()
		go func() {
			defer close(stopped)
			_, _ = io.WriteString(writer, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"stream\"}}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"first\"}\n\n")
			<-ctx.Done()
			_ = writer.CloseWithError(ctx.Err())
		}()
		return successfulResponse(reader, true), nil
	}
	server := httptest.NewServer(s)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/codex-api/api/chat", strings.NewReader(`{"model":"friendly","messages":[{"role":"user","content":"hello"}]}`))
	r.Header.Set("Authorization", "Bearer "+syntheticClientKey)
	response, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(response.Body).ReadString('\n')
	if err != nil || !strings.Contains(line, `"content":"first"`) || strings.Contains(line, `"done":true`) {
		t.Fatalf("first chunk not flushed: %s %v", line, err)
	}
	cancel()
	_ = response.Body.Close()
	awaitDone(t, stopped)
	select {
	case o := <-observed:
		if o.State != "cancelled" {
			t.Fatalf("cancel state: %+v", o)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled request kept admission")
	}
	for _, state := range s.Snapshot() {
		if state.Inflight != 0 {
			t.Fatal("cancelled request leaked inflight")
		}
	}
}

func TestAnthropicStandardAPIKeyAuthenticatesWithoutBearer(t *testing.T) {
	s, live := fixtureService(t)
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "synthetic-upstream" || r.Header.Get("Authorization") == "Bearer "+syntheticClientKey {
			t.Error("Anthropic credential boundary")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg-fixture","type":"message","role":"assistant","model":"gpt-fixture","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	live.change(func(c *config.Config) {
		a := &c.LocalAccounts[0]
		a.SourceType = "api_key"
		a.CredentialRef = ""
		a.Key = "synthetic-upstream"
		a.BaseURL = upstream.URL
		a.Wire = "anthropic"
		a.AuthMode = "api-key"
	})
	r := apiRequest(context.Background(), "", strings.NewReader(`{"model":"friendly","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`))
	r.URL.Path = "/codex-api/v1/messages"
	r.Header.Del("Authorization")
	r.Header.Set("x-api-key", syntheticClientKey)
	r.Header.Set("anthropic-version", "2023-06-01")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || calls != 1 || !strings.Contains(w.Body.String(), "hello") {
		t.Fatalf("standard Anthropic auth: %d %s", w.Code, w.Body)
	}
}

const nativeCompleted = `{"type":"response.completed","response":{"id":"native-response","object":"response","status":"completed","model":"gpt-fixture","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":9,"output_tokens":4}}}`
const nativeFrames = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"native-response\"}}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\ndata: " + nativeCompleted + "\n\n"

func callNative(s *Service, method, path, body, header, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:15551"
	if header != "" {
		r.Header.Set(header, key)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func nativeFixture(t *testing.T) (*Service, *liveConfig, *atomic.Int32) {
	s, live := fixtureService(t)
	calls := &atomic.Int32{}
	s.ModelCatalog = nil
	s.Official = func(_ context.Context, a config.LocalAccount, body []byte, stream, compact bool, current func() bool) (*http.Response, error) {
		calls.Add(1)
		if a.ID != "account-A" || !current() || !strings.Contains(string(body), `"model":"gpt-fixture"`) {
			t.Error("native dispatch escaped client binding")
		}
		return successfulResponse(io.NopCloser(strings.NewReader(nativeFrames)), true), nil
	}
	return s, live, calls
}
func TestNativeProtocolRequestsUseNamedAuthorizationAndAdaptResponses(t *testing.T) {
	for _, tc := range []struct{ name, path, body, header, key, output string }{
		{"gemini", "/codex-api/v1beta/models/friendly:generateContent", `{"contents":[{"parts":[{"text":"hello"}]}]}`, "x-goog-api-key", syntheticClientKey, `"finishReason":"STOP"`},
		{"gemini-query", "/codex-api/v1beta/models/friendly:generateContent?key=" + syntheticClientKey, `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`, "", "", `"promptTokenCount":9`},
		{"ollama-chat", "/codex-api/api/chat", `{"model":"friendly","messages":[{"role":"user","content":"hello"}],"stream":false}`, "Authorization", "Bearer " + syntheticClientKey, `"done":true`},
		{"ollama-generate", "/codex-api/api/generate", `{"model":"friendly","prompt":"hello","system":"be concise","stream":false}`, "Authorization", "Bearer " + syntheticClientKey, `"response":"hello"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, calls := nativeFixture(t)
			var observations []usageledger.Observation
			s.Observe = func(o usageledger.Observation) { observations = append(observations, o) }
			w := callNative(s, "POST", tc.path, tc.body, tc.header, tc.key)
			if w.Code != 200 || calls.Load() != 1 || !json.Valid(w.Body.Bytes()) || !strings.Contains(w.Body.String(), tc.output) {
				t.Fatalf("native response %d %s", w.Code, w.Body)
			}
			if strings.Contains(w.Body.String(), "totalTokenCount") || len(observations) != 1 || strings.Contains(observations[0].Route, "key=") || observations[0].State != "completed" {
				t.Fatalf("wrong usage/observation: %s %+v", w.Body, observations)
			}
		})
	}
}
func TestNativeStreamingFramingAndCompletion(t *testing.T) {
	for _, tc := range []struct{ path, body, kind string }{
		{"/codex-api/v1beta/models/friendly:streamGenerateContent?alt=sse", `{"contents":[{"parts":[{"text":"hello"}]}]}`, "sse"},
		{"/codex-api/v1beta/models/friendly:streamGenerateContent", `{"contents":[{"parts":[{"text":"hello"}]}]}`, "array"},
		{"/codex-api/api/chat", `{"model":"friendly","messages":[{"role":"user","content":"hello"}]}`, "ndjson"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			s, _, _ := nativeFixture(t)
			w := callNative(s, "POST", tc.path, tc.body, "Authorization", "Bearer "+syntheticClientKey)
			if w.Code != 200 || strings.Contains(w.Body.String(), "error") {
				t.Fatalf("stream %d %s", w.Code, w.Body)
			}
			if tc.kind == "array" {
				if !json.Valid(w.Body.Bytes()) {
					t.Fatalf("array invalid %s", w.Body)
				}
			} else {
				for _, line := range strings.Split(strings.TrimSpace(w.Body.String()), "\n") {
					if line == "" {
						continue
					}
					line = strings.TrimPrefix(line, "data: ")
					if !json.Valid([]byte(line)) {
						t.Fatalf("frame invalid %s", line)
					}
				}
			}
			marker := `"finishReason":"STOP"`
			if tc.kind == "ndjson" {
				marker = `"done":true`
			}
			if strings.Count(w.Body.String(), marker) != 1 {
				t.Fatalf("wrong terminal count %s", w.Body)
			}
		})
	}
}
func TestNativeAndAnthropicCredentialConflictsAndRoutesFailClosed(t *testing.T) {
	s, _, calls := nativeFixture(t)
	for _, tc := range []struct {
		path    string
		headers map[string][]string
		status  int
	}{
		{"/codex-api/api/chat", nil, 401},
		{"/codex-api/v1/messages", map[string][]string{"X-Api-Key": {syntheticClientKey}}, 400},
		{"/codex-api/v1/messages", map[string][]string{"Authorization": {"Bearer " + syntheticClientKey}, "X-Api-Key": {syntheticClientKey}}, 401},
		{"/codex-api/v1/messages", map[string][]string{"X-Api-Key": {syntheticClientKey, syntheticClientKey}}, 401},
		{"/codex-api/api/chat", map[string][]string{"X-Api-Key": {syntheticClientKey}}, 401},
		{"/codex-api/v1beta/models/friendly:generateContent?key=" + syntheticClientKey, map[string][]string{"X-Goog-Api-Key": {syntheticClientKey}}, 401},
		{"/codex-api/v1beta/models/friendly:generateContent?key=" + syntheticClientKey + "&key=" + syntheticClientKey, nil, 404},
		{"/codex-api/v1beta/models/friendly:generateContent?key=" + syntheticClientKey + "&alt=sse", nil, 400},
		{"/codex-api/managed/codex/v1beta/models", map[string][]string{"Authorization": {"Bearer " + syntheticClientKey}}, 404},
		{"/codex-api/api/delete", map[string][]string{"Authorization": {"Bearer " + syntheticClientKey}}, 404},
	} {
		r := httptest.NewRequest("POST", tc.path, strings.NewReader(`{}`))
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header = tc.headers
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Errorf("%s status=%d want=%d body=%s", tc.path, w.Code, tc.status, w.Body)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("rejected credential reached upstream")
	}
}
func TestNativeRejectsUnsupportedSettingsAndAmbiguousToolHistory(t *testing.T) {
	for _, tc := range []struct{ path, body string }{
		{"api/chat", `{"model":"friendly","Model":"other","messages":[{"role":"user","content":"hello"}]}`},
		{"api/generate", `{"model":"friendly","prompt":"hello","context":[1,2]}`},
		{"api/generate", `{"model":"friendly","prompt":"hello","options":{"num_ctx":1024}}`},
		{"api/chat", `{"model":"friendly","messages":[{"role":"tool","content":"orphan"}]}`},
		{"v1beta/models/friendly:generateContent", `{"contents":[{"parts":[{"text":"hello","functionCall":{"name":"tool","args":{}}}]}]}`},
		{"v1beta/models/friendly:generateContent", `{"contents":[{"parts":[{"text":"hello"}]}],"safetySettings":[{"category":"test"}]}`},
		{"v1beta/models/friendly:generateContent", `{"contents":[{"parts":[{"text":"hello"}]}],"generationConfig":{"candidateCount":2}}`},
	} {
		s, _, calls := nativeFixture(t)
		w := callNative(s, "POST", "/codex-api/"+tc.path, tc.body, "Authorization", "Bearer "+syntheticClientKey)
		if w.Code != 400 || calls.Load() != 0 {
			t.Errorf("unsupported intent dispatched: %d %s", w.Code, w.Body)
		}
	}
}
func TestNativeModelNamespaceAndCatalogNeverExpandGrants(t *testing.T) {
	s, live, calls := nativeFixture(t)
	live.change(func(c *config.Config) {
		c.CodexAPI.Clients[0].ModelPrefix = "team/"
		c.CodexAPI.Clients[0].ModelAllowlist = []string{"gpt-fixture"}
		c.LocalAccounts[0].SourceType = "api_key"
		c.LocalAccounts[0].CredentialRef = ""
	})
	for _, path := range []string{"v1beta/models", "api/tags"} {
		w := callNative(s, "GET", "/codex-api/"+path, "", "Authorization", "Bearer "+syntheticClientKey)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "team/friendly") || strings.Contains(w.Body.String(), "account-B") {
			t.Fatalf("catalog %d %s", w.Code, w.Body)
		}
	}
	w := callNative(s, "POST", "/codex-api/v1beta/models/friendly:generateContent", `{"contents":[{"parts":[{"text":"hello"}]}]}`, "Authorization", "Bearer "+syntheticClientKey)
	if w.Code != 403 || calls.Load() != 0 {
		t.Fatalf("namespace bypass %d %s", w.Code, w.Body)
	}
	live.change(func(c *config.Config) { c.CodexAPI.Clients[0].Disabled = true })
	w = callNative(s, "GET", "/codex-api/api/tags", "", "Authorization", "Bearer "+syntheticClientKey)
	if w.Code != 401 {
		t.Fatal("disabled client listed models")
	}
}
func TestNativeStreamFailureNeverInventsSuccess(t *testing.T) {
	for _, path := range []string{"/codex-api/api/chat", "/codex-api/v1beta/models/friendly:streamGenerateContent?alt=sse"} {
		s, _, _ := nativeFixture(t)
		s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
			return successfulResponse(io.NopCloser(strings.NewReader("data: {\"type\":\"response.created\",\"response\":{\"id\":\"partial\"}}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")), true), nil
		}
		body := `{"model":"friendly","messages":[{"role":"user","content":"hello"}]}`
		if strings.Contains(path, "v1beta") {
			body = `{"contents":[{"parts":[{"text":"hello"}]}]}`
		}
		w := callNative(s, "POST", path, body, "Authorization", "Bearer "+syntheticClientKey)
		if !strings.Contains(w.Body.String(), "upstream_stream_error") || strings.Contains(w.Body.String(), `"done":true`) || strings.Contains(w.Body.String(), "finishReason") {
			t.Fatalf("false success %d %s", w.Code, w.Body)
		}
	}
}
func TestNativeQueuedUploadCannotSurviveRevocation(t *testing.T) {
	s, live, calls := nativeFixture(t)
	body := &blockedBody{entered: make(chan struct{}), release: make(chan struct{}), reader: strings.NewReader(`{"model":"friendly","prompt":"hello","stream":false}`)}
	r := apiRequest(context.Background(), syntheticClientKey, body)
	r.URL.Path = "/codex-api/api/generate"
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); s.ServeHTTP(w, r) }()
	awaitDone(t, body.entered)
	live.change(func(c *config.Config) { c.CodexAPI.Clients[0].Disabled = true })
	close(body.release)
	awaitDone(t, done)
	if w.Code < 400 || calls.Load() != 0 {
		t.Fatal("revoked native upload dispatched")
	}
}
func TestNativeAdapterPreservesToolHistoryAndStreamingArguments(t *testing.T) {
	requests := []struct {
		route nativeRoute
		body  string
	}{
		{nativeRoute{kind: "gemini", model: "friendly"}, `{"contents":[{"role":"user","parts":[{"text":"question"}]},{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"city":"test"}}}]},{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"result":"original result"}}},{"text":"follow up"}]}],"tools":[{"functionDeclarations":[{"name":"lookup","parameters":{"type":"OBJECT","properties":{"city":{"type":"STRING"}}}}]}]}`},
		{nativeRoute{kind: "ollama"}, `{"model":"friendly","stream":false,"messages":[{"role":"user","content":"question"},{"role":"assistant","content":"","tool_calls":[{"function":{"name":"lookup","arguments":{"city":"test"}}}]},{"role":"tool","tool_name":"lookup","content":"original result"},{"role":"user","content":"follow up"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`},
	}
	for _, tc := range requests {
		body, err := tc.route.request([]byte(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		s, _, _ := nativeFixture(t)
		s.Official = func(_ context.Context, _ config.LocalAccount, request []byte, _ bool, _ bool, _ func() bool) (*http.Response, error) {
			for _, text := range []string{"original result", "follow up", "lookup", "function_call"} {
				if !strings.Contains(string(request), text) {
					t.Errorf("lost %s: %s", text, request)
				}
			}
			return successfulResponse(io.NopCloser(strings.NewReader(nativeFrames)), true), nil
		}
		if w := callChatAPI(s, syntheticClientKey, string(body)); w.Code != 200 {
			t.Fatalf("tool history invalid %d %s", w.Code, w.Body)
		}
	}
	for _, kind := range []string{"gemini", "ollama"} {
		w := httptest.NewRecorder()
		adapter := newNativeResponseWriter(w, &nativeRoute{kind: kind, stream: true, sse: true, model: "friendly"})
		chunks := []string{`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","function":{"name":"lookup","arguments":"{\"city\":"}}]}}]}`, `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"test\"}"}}]},"finish_reason":"tool_calls"}]}`, `{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2}}`, `[DONE]`}
		for _, chunk := range chunks {
			for _, b := range []byte("data: " + chunk + "\n\n") {
				if _, err := adapter.Write([]byte{b}); err != nil {
					t.Fatal(err)
				}
			}
		}
		adapter.Finish()
		if strings.Contains(w.Body.String(), "error") || !strings.Contains(w.Body.String(), `"city":"test"`) {
			t.Fatalf("tool stream failed %s", w.Body)
		}
		if kind == "ollama" && !strings.Contains(w.Body.String(), `"done_reason":"stop"`) {
			t.Fatalf("tool result must use Ollama stop reason: %s", w.Body)
		}
	}
}

func TestNativeOllamaCompletionReasonsFailClosed(t *testing.T) {
	for _, reason := range []string{"", "content_filter", "unknown"} {
		t.Run(reason, func(t *testing.T) {
			w := httptest.NewRecorder()
			adapter := newNativeResponseWriter(w, &nativeRoute{kind: "ollama", model: "friendly"})
			body, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "partial"}, "finish_reason": reason}}})
			if _, err := adapter.Write(body); err != nil {
				t.Fatal(err)
			}
			adapter.Finish()
			if w.Code != http.StatusBadGateway || !adapter.failed || strings.Contains(w.Body.String(), `"done":true`) {
				t.Fatalf("unsupported completion became success: %d %s", w.Code, w.Body)
			}
		})
	}
}

func TestNativeGeminiSchemaPreservesLiteralValues(t *testing.T) {
	route := nativeRoute{kind: "gemini", model: "friendly"}
	body, err := route.request([]byte(`{"contents":[{"parts":[{"text":"hello"}]}],"generationConfig":{"responseMimeType":"application/json","responseSchema":{"type":"OBJECT","properties":{"type":{"type":"STRING"},"items":{"type":"ARRAY","items":{"type":"STRING"}}},"default":{"type":"CUSTOM","items":[{"type":"EXACT"}]},"enum":[{"type":"CASE-SENSITIVE"}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Format struct {
			JSONSchema struct {
				Schema map[string]any `json:"schema"`
			} `json:"json_schema"`
		} `json:"response_format"`
	}
	if err = json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	schema := request.Format.JSONSchema.Schema
	properties := schema["properties"].(map[string]any)
	if schema["type"] != "object" || properties["type"].(map[string]any)["type"] != "string" || properties["items"].(map[string]any)["items"].(map[string]any)["type"] != "string" {
		t.Fatalf("schema types were not converted: %s", body)
	}
	literal := schema["default"].(map[string]any)
	if literal["type"] != "CUSTOM" || literal["items"].([]any)[0].(map[string]any)["type"] != "EXACT" || schema["enum"].([]any)[0].(map[string]any)["type"] != "CASE-SENSITIVE" {
		t.Fatalf("schema changed literal user data: %s", body)
	}
}
