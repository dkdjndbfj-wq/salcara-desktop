package gateway

import (
	"net/http"
	"strings"
	"testing"
)

func TestAdditionalAgentRoutesUseChatAndRejectOtherSurfaces(t *testing.T) {
	for _, kind := range []string{"deepseek-harness", "hermes"} {
		for _, prefix := range []string{"", "remote-", "direct-"} {
			t.Run(prefix+kind, func(t *testing.T) {
				calls := 0
				h := fixtureGateway(t, kind, "chat", `{"id":"test","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`, "application/json", func(r *http.Request, in map[string]any) {
					calls++
					if r.URL.Path != "/prefix/v1/chat/completions" {
						t.Error("wrong upstream protocol")
					}
				})
				w := callGateway(h, prefix+kind, "chat/completions", `{"model":"chosen-model","messages":[{"role":"user","content":"hello"}]}`)
				if w.Code != 200 || !strings.Contains(w.Body.String(), "ok") || calls != 1 {
					t.Fatalf("chat: %d %s", w.Code, w.Body)
				}
				for _, path := range []string{"responses", "messages", "responses/compact"} {
					w := callGateway(h, prefix+kind, path, `{}`)
					if w.Code != 404 || calls != 1 {
						t.Fatal("opened unrelated API surface")
					}
				}
			})
		}
	}
}

func TestAdditionalAgentsConvertAnthropicReplyAndKeepModel(t *testing.T) {
	for _, kind := range []string{"deepseek-harness", "hermes"} {
		for _, prefix := range []string{"", "remote-", "direct-"} {
			t.Run(prefix+kind, func(t *testing.T) {
				h := fixtureGateway(t, kind, "anthropic", anthropicFixtureSSE, "text/event-stream", func(r *http.Request, in map[string]any) {
					if r.URL.Path != "/prefix/v1/messages" || in["model"] != "chosen-model" {
						t.Error("new Agent conversion lost upstream protocol or model")
					}
				})
				w := callGateway(h, prefix+kind, "chat/completions", `{"model":"chosen-model","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
				if w.Code != 200 || !strings.Contains(w.Body.String(), "Claude-compatible reply") || !strings.Contains(w.Body.String(), "[DONE]") {
					t.Fatalf("converted reply: %d %s", w.Code, w.Body)
				}
			})
		}
	}
}
