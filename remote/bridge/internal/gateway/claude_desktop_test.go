package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
)

func desktopCatalog(h *Handler, models ...string) {
	source := h.Source
	h.Source = func(target string) (config.LocalAccount, bool) {
		a, ok := source(target)
		a.Models, a.Model, a.Wire = models, "", "auto"
		return a, ok
	}
}

func TestClaudeDesktopDeepSeekAliasUsesRealModelToolsAndReplyIdentity(t *testing.T) {
	model := "deepseek-chat"
	route := config.ClaudeDesktopModelRoute(model)
	output := `{"id":"reply","object":"chat.completion","model":"deepseek-chat","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"read-id","type":"function","function":{"name":"Read","arguments":"{\"path\":\"fixture.txt\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`
	h := fixtureGateway(t, "claude", "chat", output, "application/json", func(r *http.Request, in map[string]any) {
		if r.URL.Path != "/prefix/v1/chat/completions" || in["model"] != model {
			t.Error("wrong real model/protocol")
		}
		b, _ := json.Marshal(in)
		if !strings.Contains(string(b), "Read") || !strings.Contains(string(b), "existing context") || in["thinking"] != nil || in["reasoning_effort"] != nil {
			t.Error("lost tools/context or claimed unsupported effort")
		}
	})
	desktopCatalog(h, model)
	body := `{"model":"` + route + `","max_tokens":100,"thinking":{"type":"adaptive"},"output_config":{"effort":"low"},"messages":[{"role":"user","content":"existing context"}],"tools":[{"name":"Read","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}]}`
	w := callGateway(h, "claude-desktop", "messages", body)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"tool_use"`) || !strings.Contains(w.Body.String(), route) || strings.Contains(w.Body.String(), `"model":"deepseek-chat"`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestClaudeDesktopGrokAliasStreamingKeepsNativeEventsAndModel(t *testing.T) {
	model := "grok-fixture"
	route := config.ClaudeDesktopModelRoute(model)
	chunks := "data: {\"id\":\"reply\",\"model\":\"grok-fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"fixture progress\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"reply\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	h := fixtureGateway(t, "claude", "chat", chunks, "text/event-stream", func(_ *http.Request, in map[string]any) {
		if in["model"] != model {
			t.Error("compatibility alias leaked upstream")
		}
	})
	desktopCatalog(h, model)
	w := callGateway(h, "claude-desktop", "messages", `{"model":"`+route+`","stream":true,"max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
	for _, text := range []string{"message_start", "fixture progress", "message_stop", route} {
		if w.Code != 200 || !strings.Contains(w.Body.String(), text) {
			t.Fatalf("missing %s: %d %s", text, w.Code, w.Body.String())
		}
	}
	if strings.Contains(w.Body.String(), "upstream_stream_error") {
		t.Fatal("completed stream flagged as truncated")
	}
}

func TestClaudeDesktopDiscoveryLabelsAreRealAndForeignAliasesFailClosed(t *testing.T) {
	upstreamCalls := 0
	h := fixtureGateway(t, "claude", "chat", `{}`, "application/json", func(*http.Request, map[string]any) { upstreamCalls++ })
	desktopCatalog(h, "deepseek-chat", "claude-sonnet-4-6")
	r := httptest.NewRequest("GET", "/gateway/claude-desktop/v1/models", nil)
	r.RemoteAddr = "127.0.0.1:10000"
	r.Header.Set("Authorization", "Bearer "+localTestKey)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"display_name":"deepseek-chat"`) || !strings.Contains(w.Body.String(), `"has_more":false`) {
		t.Fatal(w.Code, w.Body.String())
	}
	w = callGateway(h, "claude-desktop", "messages", `{"model":"`+config.ClaudeDesktopModelRoute("other-key-model")+`","messages":[]}`)
	if w.Code != 400 || upstreamCalls != 0 {
		t.Fatal("unknown route sent a paid request")
	}
}

func TestClaudeDesktopEmptyCatalogDiscoversOnlyOnDemand(t *testing.T) {
	h := fixtureGateway(t, "claude", "chat", `{}`, "application/json", nil)
	desktopCatalog(h)
	calls := 0
	h.RefreshModels = func(_ context.Context, target string, _ config.LocalAccount) ([]string, error) {
		calls++
		if target != "claude-desktop" {
			t.Error("wrong target")
		}
		return []string{"deepseek-chat"}, nil
	}
	if calls != 0 {
		t.Fatal("background discovery")
	}
	r := httptest.NewRequest("GET", "/gateway/claude-desktop/v1/models", nil)
	r.RemoteAddr = "127.0.0.1:10000"
	r.Header.Set("Authorization", "Bearer "+localTestKey)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if calls != 1 || w.Code != 200 || !strings.Contains(w.Body.String(), "deepseek-chat") {
		t.Fatal("native on-demand discovery failed")
	}
}
