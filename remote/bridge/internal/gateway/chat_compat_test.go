package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"salcara/bridge/internal/usageledger"
)

func TestLocalToolGatewayDoesNotExposeNamedChatPath(t *testing.T) {
	h := fixtureGateway(t, "codex", "chat", `{}`, "application/json", nil)
	if w := callGateway(h, "codex", "chat/completions", `{"model":"chosen-model","messages":[{"role":"user","content":"fixture"}]}`); w.Code != 404 {
		t.Fatal("named Chat route expanded the local tool contract")
	}
}

func TestChatAnthropicSDKPreservesToolsHistoryAndCompletion(t *testing.T) {
	const chatBody = `{"model":"chosen-model","messages":[{"role":"developer","content":"developer instruction"},{"role":"user","content":"original context"},{"role":"assistant","content":null,"tool_calls":[{"id":"call-old","type":"function","function":{"name":"edit_files","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call-old","content":"original result"}],"tools":[{"type":"function","function":{"name":"edit_files","parameters":{"type":"object"}}}],"tool_choice":{"type":"function","function":{"name":"edit_files"}}}`
	output := strings.Replace(anthropicFixtureSSE, "event: message_delta", "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call-new\",\"name\":\"edit_files\",\"input\":{}}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\\\"file.txt\\\"}\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\nevent: message_delta", 1)
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonstream", true: "stream"}[stream], func(t *testing.T) {
			h := fixtureGateway(t, "codex", "anthropic", output+"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", "text/event-stream", func(r *http.Request, request map[string]any) {
				if r.URL.Path != "/prefix/v1/messages" || request["stream"] != true {
					t.Error("Chat did not use translated Anthropic stream")
				}
				raw, _ := json.Marshal(request)
				for _, text := range []string{"developer instruction", "original context", "original result", "tool_use", "tool_result", "edit_files"} {
					if !bytes.Contains(raw, []byte(text)) {
						t.Errorf("Chat silently lost %s", text)
					}
				}
			})
			h.ChatCompletions = true
			var observed []usageledger.Observation
			h.Observe = func(o usageledger.Observation) { observed = append(observed, o) }
			body := chatBody
			if stream {
				body = strings.TrimSuffix(body, "}") + `,"stream":true,"stream_options":{"include_usage":true}}`
			}
			w := callGateway(h, "codex", "chat/completions", body)
			if w.Code != 200 || strings.Contains(w.Body.String(), "upstream_stream_error") {
				t.Fatalf("%d: %s", w.Code, w.Body)
			}
			for _, text := range []string{"Claude-compatible reply", "edit_files", "call-new", "tool_calls"} {
				if !strings.Contains(w.Body.String(), text) {
					t.Fatalf("Chat lost %s: %s", text, w.Body)
				}
			}
			if stream && strings.Count(w.Body.String(), "data: [DONE]") != 1 {
				t.Fatal("duplicate or missing Chat completion")
			}
			if !stream && !json.Valid(w.Body.Bytes()) {
				t.Fatal("invalid Chat nonstream output")
			}
			if len(observed) != 1 || observed[0].Tokens.Input == nil || *observed[0].Tokens.Input != 12 || observed[0].Tokens.Output == nil || *observed[0].Tokens.Output != 8 || observed[0].Tokens.Total != nil {
				t.Fatal("Chat fabricated or duplicated raw provider usage")
			}
		})
	}
}

func TestChatAnthropicRejectsSDKOptionsThatWouldChangeCallerIntent(t *testing.T) {
	for _, tail := range []string{`,"tool_choice":"none"`, `,"tools":[{"type":"function","function":{"name":"edit","strict":true}}]`} {
		if status, _ := validateChatRequest([]byte(`{"model":"chosen-model","messages":[{"role":"user","content":"fixture"}]`+tail+`}`), translator.FormatClaude); status != 501 {
			t.Fatal("unsupported Claude option silently accepted")
		}
	}
	for _, schema := range []string{`{"type":"array"}`, `{"oneOf":[{"type":"object"},{"type":"object"}]}`, `{"type":"object","properties":[]}`} {
		if status, _ := validateChatRequest([]byte(`{"model":"chosen-model","messages":[{"role":"user","content":"fixture"}],"tools":[{"type":"function","function":{"name":"edit","parameters":`+schema+`}}]}`), translator.FormatClaude); status != 501 {
			t.Fatal("Claude SDK would change the requested function schema")
		}
	}
	for _, arguments := range []string{`not-json`, `[]`} {
		raw, _ := json.Marshal(map[string]any{"model": "chosen-model", "messages": []any{map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "call-a", "type": "function", "function": map[string]any{"name": "edit", "arguments": arguments}}}}}})
		if status, _ := validateChatRequest(raw, translator.FormatClaude); status != 501 {
			t.Fatal("Claude SDK would silently replace arguments with empty object")
		}
	}
	batch := `{"role":"assistant","content":null,"tool_calls":[{"id":"call-reused","type":"function","function":{"name":"edit","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call-reused","content":"original result"}`
	if status, _ := validateChatRequest([]byte(`{"model":"chosen-model","messages":[`+batch+`,{"role":"user","content":"next"},`+batch+`]}`), translator.FormatClaude); status != 400 {
		t.Fatal("SDK would replace original history through a reused tool call ID")
	}
}

func TestChatSupportedStructuredOutputsAndMessagesRemainExplicit(t *testing.T) {
	body := []byte(`{"model":"gpt-fixture","messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,ZmFrZQ=="}}]}],"n":1,"parallel_tool_calls":true,"reasoning_effort":"low","response_format":{"type":"json_schema","json_schema":{"name":"fixture","strict":true,"schema":{"type":"object","properties":{"x":{"type":"string"},"X":{"type":"string"}}}}}}`)
	if status, message := validateChatRequest(body, translator.FormatCodex); status != 0 {
		t.Fatalf("supported request rejected: %d %s", status, message)
	}
	converted := translator.TranslateRequest(translator.FormatOpenAI, translator.FormatCodex, "gpt-fixture", body, true)
	for _, field := range []string{"input_image", "data:image/png", "json_schema", "fixture", "low"} {
		if !bytes.Contains(converted, []byte(field)) {
			t.Fatalf("SDK lost a validated field: %s", field)
		}
	}
}
