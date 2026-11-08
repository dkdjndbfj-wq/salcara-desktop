package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
)

func TestClaudeNativeBetaMessagesAndTokenCount(t *testing.T) {
	for _, target := range []string{"claude", "remote-claude", "direct-claude", "claude-desktop"} {
		for _, path := range []string{"messages", "messages/count_tokens"} {
			t.Run(target+"/"+path, func(t *testing.T) {
				calls := 0
				h := fixtureGateway(t, "claude", "anthropic", `{"input_tokens":10,"content":[]}`, "application/json", func(r *http.Request, body map[string]any) {
					calls++
					if r.URL.RawQuery != "beta=true" || r.URL.Path != "/prefix/v1/"+path {
						t.Errorf("native beta route lost: %s", r.URL)
					}
				})
				model := "chosen-model"
				if target == "claude-desktop" {
					desktopCatalog(h, model)
					model = config.ClaudeDesktopModelRoute(model)
				}
				w := callGateway(h, target, path+"?beta=true", `{"model":"`+model+`","messages":[{"role":"user","content":"fixture"}]}`)
				if w.Code != http.StatusOK || calls != 1 {
					t.Fatalf("native beta request rejected: %d %s calls=%d", w.Code, w.Body, calls)
				}
			})
		}
	}
}

func TestClaudeNativeBetaHeadersStayOnAnthropicUpstream(t *testing.T) {
	for _, wire := range []string{"anthropic", "chat"} {
		t.Run(wire, func(t *testing.T) {
			calls := 0
			h := fixtureGateway(t, "claude", wire, `{"content":[]}`, "application/json", func(r *http.Request, _ map[string]any) {
				calls++
				if wire == "anthropic" {
					if r.Header.Get("anthropic-beta") != "interleaved-thinking-2025-05-14" || r.URL.RawQuery != "beta=true" {
						t.Error("native protocol flags were lost")
					}
				} else if r.Header.Get("anthropic-beta") != "" || r.URL.RawQuery != "" {
					t.Error("Claude flags leaked across protocol translation")
				}
				if r.Header.Get("X-Private-Fixture") != "" {
					t.Error("unrelated client header forwarded")
				}
			})
			r := httptest.NewRequest(http.MethodPost, "/gateway/direct-claude/v1/messages?beta=true", strings.NewReader(`{"model":"chosen-model","messages":[{"role":"user","content":"fixture"}]}`))
			r.RemoteAddr = "127.0.0.1:10000"
			r.Header.Set("Authorization", "Bearer "+localTestKey)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("anthropic-beta", "interleaved-thinking-2025-05-14")
			r.Header.Set("X-Private-Fixture", "private")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if calls != 1 {
				t.Fatalf("beta request did not reach upstream: %d %s", w.Code, w.Body)
			}
		})
	}
}

func TestClaudeNativeBetaQueryRejectsOtherFieldsAndRoutes(t *testing.T) {
	for _, query := range []string{"beta=false", "beta=", "beta=true&beta=true", "beta=true&key=other", "beta=true;other=x", "beta=%GG", "model=other"} {
		t.Run(query, func(t *testing.T) {
			h := fixtureGateway(t, "claude", "anthropic", `{}`, "application/json", func(*http.Request, map[string]any) { t.Error("invalid query reached upstream") })
			if w := callGateway(h, "direct-claude", "messages?"+query, `{"model":"chosen-model"}`); w.Code != http.StatusNotFound {
				t.Fatalf("query admitted: %d", w.Code)
			}
		})
	}
	for _, target := range []string{"codex", "remote-codex", "direct-codex", "hermes"} {
		h := fixtureGateway(t, "codex", "responses", `{}`, "application/json", nil)
		if w := callGateway(h, target, "messages?beta=true", `{"model":"chosen-model"}`); w.Code != http.StatusNotFound {
			t.Fatalf("beta escaped Claude boundary: %s %d", target, w.Code)
		}
	}
}
