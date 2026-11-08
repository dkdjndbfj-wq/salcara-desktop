package apiservice

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/usageledger"
)

func callChatAPI(s *Service, key, body string) *httptest.ResponseRecorder {
	r := apiRequest(context.Background(), key, strings.NewReader(body))
	r.URL.Path = "/codex-api/v1/chat/completions"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

const completedChatResponseEvent = `{"type":"response.completed","response":{"id":"resp-chat","object":"response","status":"completed","model":"gpt-fixture","output":[{"id":"msg-one","type":"message","role":"assistant","content":[{"type":"output_text","text":"ready"}]},{"type":"function_call","id":"fc-one","call_id":"call-new","name":"edit_files","arguments":"{\"path\":\"file.txt\"}"}],"usage":{"input_tokens":9,"output_tokens":4}}}`
const chatWithTools = `{"model":"friendly","messages":[{"role":"system","content":"keep developer instruction"},{"role":"user","content":"original question"},{"role":"assistant","content":null,"tool_calls":[{"id":"call-old","type":"function","function":{"name":"edit_files","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call-old","content":"original tool result"},{"role":"user","content":"follow-up"}],"tools":[{"type":"function","function":{"name":"edit_files","description":"edit","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}],"tool_choice":{"type":"function","function":{"name":"edit_files"}}}`

func TestChatCompatibilityPreservesCompleteToolHistoryAndProtectedAccountBinding(t *testing.T) {
	s, _ := fixtureService(t)
	var calls int
	var observations []usageledger.Observation
	s.Observe = func(o usageledger.Observation) { observations = append(observations, o) }
	s.Official = func(_ context.Context, a config.LocalAccount, body []byte, stream, compact bool, _ func() bool) (*http.Response, error) {
		calls++
		if a.ID != "account-A" || a.CredentialRef != "credential-A" || !stream || compact {
			t.Fatal("Chat left the protected bound account path")
		}
		var request map[string]any
		if json.Unmarshal(body, &request) != nil || request["model"] != "gpt-fixture" {
			t.Fatal("alias model was not resolved")
		}
		for _, value := range []string{"developer", "keep developer instruction", "original question", "function_call", "original tool result", "follow-up", "edit_files"} {
			if !strings.Contains(string(body), value) {
				t.Errorf("SDK silently lost complete chat context: %s", value)
			}
		}
		if strings.Contains(string(body), syntheticClientKey) {
			t.Fatal("client key entered provider body")
		}
		return successfulResponse(io.NopCloser(strings.NewReader("data: "+completedChatResponseEvent+"\n\n")), true), nil
	}
	w := callChatAPI(s, syntheticClientKey, chatWithTools)
	if w.Code != 200 || calls != 1 {
		t.Fatalf("Chat response: %d %s", w.Code, w.Body)
	}
	for _, value := range []string{`"object":"chat.completion"`, `"edit_files"`, `"call-new"`, `"tool_calls"`, `"ready"`} {
		if !strings.Contains(w.Body.String(), value) {
			t.Fatalf("Chat lost provider output: %s %s", value, w.Body)
		}
	}
	if len(observations) != 1 || observations[0].Route != "codex-api/v1/chat/completions" || observations[0].State != "completed" || observations[0].Tokens.Input == nil || *observations[0].Tokens.Input != 9 || observations[0].Tokens.Total != nil {
		t.Fatal("Chat usage was duplicated or synthesized")
	}
}

func TestChatCompatibilityStreamUsesSSEFramesToolsAndOneDoneAfterProviderCompletion(t *testing.T) {
	s, _ := fixtureService(t)
	frames := []string{
		`{"type":"response.created","response":{"id":"resp-chat","created_at":1,"model":"gpt-fixture"}}`,
		`{"type":"response.output_text.delta","delta":"ready"}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc-one","call_id":"call-new","name":"edit_files","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc-one","output_index":1,"delta":"{\"path\":\"file.txt\"}"}`,
		completedChatResponseEvent,
	}
	var output strings.Builder
	for _, frame := range frames {
		output.WriteString("data: " + frame + "\n\n")
	}
	s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
		return successfulResponse(io.NopCloser(strings.NewReader(output.String())), true), nil
	}
	body := strings.TrimSuffix(chatWithTools, "}") + `,"stream":true,"stream_options":{"include_usage":true}}`
	w := callChatAPI(s, syntheticClientKey, body)
	if w.Code != 200 || strings.Contains(w.Body.String(), "upstream_stream_error") || strings.Count(w.Body.String(), "data: [DONE]") != 1 {
		t.Fatalf("Chat stream completion broken: %d %s", w.Code, w.Body)
	}
	for _, frame := range strings.Split(w.Body.String(), "\n\n") {
		if frame == "" {
			continue
		}
		if !strings.HasPrefix(frame, "data: ") {
			t.Fatalf("SDK JSON was emitted without an SSE frame: %s", frame)
		}
		raw := strings.TrimPrefix(frame, "data: ")
		if raw != "[DONE]" && !json.Valid([]byte(raw)) {
			t.Fatal("Chat emitted invalid JSON SSE")
		}
	}
	for _, value := range []string{`"object":"chat.completion.chunk"`, `"edit_files"`, `"finish_reason":"tool_calls"`, `"prompt_tokens":9`} {
		if !strings.Contains(w.Body.String(), value) {
			t.Fatalf("stream missing %s: %s", value, w.Body)
		}
	}
}

func TestChatNativeWirePreservesMultipleChoicesAndProviderOptions(t *testing.T) {
	s, live := fixtureService(t)
	var seen []byte
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = io.ReadAll(r.Body)
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer synthetic-provider-only" {
			t.Error("native Chat selected the wrong route/credential")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"first"},"finish_reason":"stop"},{"index":1,"message":{"role":"assistant","content":"second"},"finish_reason":"length"}],"usage":{"prompt_tokens":5,"completion_tokens":6}}`)
	}))
	defer u.Close()
	live.change(func(c *config.Config) {
		c.LocalAccounts[0].SourceType = "api_key"
		c.LocalAccounts[0].CredentialRef = ""
		c.LocalAccounts[0].Key = "synthetic-provider-only"
		c.LocalAccounts[0].BaseURL = u.URL
		c.LocalAccounts[0].Wire = "chat"
	})
	w := callChatAPI(s, syntheticClientKey, `{"model":"friendly","messages":[{"role":"user","content":"fixture"}],"n":2,"temperature":0.4,"max_tokens":30,"provider_extension":true}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "first") || !strings.Contains(w.Body.String(), "second") {
		t.Fatalf("native multiple choices lost: %d %s", w.Code, w.Body)
	}
	for _, option := range []string{`"n":2`, `"temperature":0.4`, `"max_tokens":30`, `"provider_extension":true`} {
		if !strings.Contains(string(seen), option) {
			t.Fatalf("native provider option silently dropped: %s", option)
		}
	}
}

func TestChatNonStreamProviderFailureCannotBeHiddenByCompletionEvent(t *testing.T) {
	s, _ := fixtureService(t)
	s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
		return successfulResponse(io.NopCloser(strings.NewReader(`data: {"type":"error","error":{"message":"private-provider-error-body"}}`+"\n\ndata: "+completedChatResponseEvent+"\n\n")), true), nil
	}
	var records []usageledger.Observation
	s.Observe = func(o usageledger.Observation) { records = append(records, o) }
	w := callChatAPI(s, syntheticClientKey, chatWithTools)
	if w.Code != 502 || strings.Contains(w.Body.String(), "private-provider-error-body") || strings.Contains(w.Body.String(), `"chat.completion"`) {
		t.Fatalf("upstream error fabricated a successful Chat: %d %s", w.Code, w.Body)
	}
	if len(records) != 1 || records[0].State != "failed" || records[0].UsageFinal {
		t.Fatalf("failure ledger state wrong: %+v", records)
	}
}

func TestChatStreamDuplicateProviderCompletionProducesOneDone(t *testing.T) {
	s, _ := fixtureService(t)
	s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
		return successfulResponse(io.NopCloser(strings.NewReader("data: "+completedChatResponseEvent+"\n\ndata: "+completedChatResponseEvent+"\n\n")), true), nil
	}
	w := callChatAPI(s, syntheticClientKey, strings.TrimSuffix(chatWithTools, "}")+`,"stream":true}`)
	if w.Code != 200 || strings.Count(w.Body.String(), "data: [DONE]") != 1 {
		t.Fatalf("Chat repeated terminal marker: %d %s", w.Code, w.Body)
	}
}

func TestChatCrossProtocolRejectsUnsupportedOrAmbiguousIntentBeforeExecuting(t *testing.T) {
	for _, tail := range []string{`,"n":2`, `,"temperature":0.4`, `,"max_tokens":30`, `,"parallel_tool_calls":false`, `,"response_format":{"type":"json_object"}`, `,"provider_extension":true`} {
		s, _ := fixtureService(t)
		var calls atomic.Int32
		s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
			calls.Add(1)
			return nil, nil
		}
		w := callChatAPI(s, syntheticClientKey, `{"model":"gpt-fixture","messages":[{"role":"user","content":"fixture"}]`+tail+`}`)
		if w.Code != 501 || calls.Load() != 0 {
			t.Fatalf("unsupported Chat intent was silently executed: %d %s", w.Code, w.Body)
		}
	}
	for _, body := range []string{`{"model":"gpt-fixture","Model":"different","messages":[{"role":"user","content":"fixture"}]}`, `{"model":"gpt-fixture","model":"different","messages":[{"role":"user","content":"fixture"}]}`, `{"model":"gpt-fixture","messages":[{"role":"tool","tool_call_id":"orphan","content":"original tool result"}]}`, `{"model":"gpt-fixture","messages":[{"role":"user","content":[{"type":"unknown_content","private":"must remain"}]}]}`} {
		s, _ := fixtureService(t)
		var calls atomic.Int32
		s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
			calls.Add(1)
			return nil, nil
		}
		w := callChatAPI(s, syntheticClientKey, body)
		if w.Code < 400 || calls.Load() != 0 {
			t.Fatal("ambiguous Chat or silently discarded context reached provider")
		}
	}
}

func TestChatStreamErrorsDoNotEmitDoneOrPrivateProviderText(t *testing.T) {
	for _, suffix := range []string{"", "data: {\"type\":\"error\",\"error\":{\"message\":\"private provider failure\"}}\n\n"} {
		s, _ := fixtureService(t)
		s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
			return successfulResponse(io.NopCloser(strings.NewReader("data: {\"type\":\"response.created\",\"response\":{\"id\":\"partial\"}}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial text\"}\n\n"+suffix)), true), nil
		}
		w := callChatAPI(s, syntheticClientKey, `{"model":"gpt-fixture","messages":[{"role":"user","content":"fixture"}],"stream":true}`)
		if strings.Contains(w.Body.String(), "[DONE]") || strings.Contains(w.Body.String(), "private provider failure") || !strings.Contains(w.Body.String(), "upstream_stream_error") {
			t.Fatalf("Chat stream concealed failure or leaked provider text: %s", w.Body)
		}
	}
}

func TestChatQueuedBodyCannotExecuteAfterNamedClientRevocation(t *testing.T) {
	s, live := fixtureService(t)
	var calls atomic.Int32
	s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
		calls.Add(1)
		return nil, nil
	}
	body := &blockedBody{entered: make(chan struct{}), release: make(chan struct{}), reader: strings.NewReader(`{"model":"friendly","messages":[{"role":"user","content":"fixture"}]}`)}
	r := apiRequest(context.Background(), syntheticClientKey, body)
	r.URL.Path = "/codex-api/v1/chat/completions"
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); s.ServeHTTP(w, r) }()
	<-body.entered
	live.change(func(c *config.Config) { c.CodexAPI.Clients[0].Disabled = true })
	close(body.release)
	awaitDone(t, done)
	if w.Code < 400 || calls.Load() != 0 {
		t.Fatal("Chat bypassed the existing queued authorization fence")
	}
}
