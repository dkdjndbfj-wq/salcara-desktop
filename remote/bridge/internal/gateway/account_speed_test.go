package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"salcara/bridge/internal/config"
)

func TestAccountSpeedOnlyOverridesCompatibleTier(t *testing.T) {
	input := []byte(`{"model":"chosen-model","service_tier":"flex","input":"keep prompt","reasoning":{"effort":"high"},"tools":[{"type":"function","name":"keep_tool"}],"opaque":{"n":1234567890123456789}}`)
	for _, protocol := range []string{"responses", "chat", "anthropic"} {
		for _, mode := range []string{"", "standard", "fast", "ultrafast"} {
			t.Run(protocol+"/"+mode, func(t *testing.T) {
				body, err := applyAccountSpeed(input, config.LocalAccount{Protocol: protocol, SpeedMode: mode})
				if err != nil {
					t.Fatal(err)
				}
				var before, after map[string]json.RawMessage
				_ = json.Unmarshal(input, &before)
				_ = json.Unmarshal(body, &after)
				want := `"flex"`
				if protocol != "anthropic" && config.AccountServiceTier(mode) != "" {
					encoded, _ := json.Marshal(config.AccountServiceTier(mode))
					want = string(encoded)
				}
				if string(after["service_tier"]) != want {
					t.Fatalf("wrong tier: %s", body)
				}
				delete(before, "service_tier")
				delete(after, "service_tier")
				if !reflect.DeepEqual(before, after) {
					t.Fatal("tier changed model, reasoning or request data")
				}
			})
		}
	}
}

func TestAccountSpeedHTTPAndWebSocketUseSelectedTierAfterTranslation(t *testing.T) {
	for _, protocol := range []string{"responses", "chat"} {
		for _, mode := range []string{"fast", "ultrafast"} {
			t.Run(protocol+"/"+mode, func(t *testing.T) {
				var calls atomic.Int32
				output := wsCompletedFixture
				if protocol == "chat" {
					output = "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"model\":\"chosen-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
				}
				h := fixtureGateway(t, "codex", protocol, output, "text/event-stream", func(_ *http.Request, in map[string]any) {
					calls.Add(1)
					if in["service_tier"] != config.AccountServiceTier(mode) || in["model"] != "chosen-model" {
						t.Errorf("wrong tier/model: %v", in)
					}
				})
				a, _ := h.Source("codex")
				a.SpeedMode = mode
				h.Source = func(string) (config.LocalAccount, bool) { return a, true }
				w := callGateway(h, "codex", "responses", `{"model":"chosen-model","stream":true,"service_tier":"flex","input":"keep input"}`)
				if w.Code != 200 {
					t.Fatalf("HTTP: %d %s", w.Code, w.Body)
				}
				c := wsFixture(t, h)
				wsSend(t, c, map[string]any{"type": "response.create", "model": "chosen-model", "service_tier": "flex", "input": "keep socket input"})
				wsUntil(t, c, "response.completed")
				if calls.Load() != 2 {
					t.Fatal("inference unexpectedly retried")
				}
			})
		}
	}
}

func TestAccountSpeedProviderRejectionDoesNotRetryAtAnotherTier(t *testing.T) {
	var calls atomic.Int32
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"unsupported service tier"}}`)
	}))
	defer u.Close()
	a := config.LocalAccount{ID: "tier", Kind: "codex", Model: "gpt-fixture", BaseURL: u.URL, Key: upstreamTestKey, SpeedMode: "ultrafast"}
	h := New(func() string { return localTestKey }, func(string) (config.LocalAccount, bool) { return a, true })
	w := callGateway(h, "codex", "responses", `{"model":"gpt-fixture","input":"fixture"}`)
	if w.Code != http.StatusBadRequest || calls.Load() != 1 {
		t.Fatal("unsupported tier silently retried")
	}
}

func TestAccountSpeedDoesNotInventAnthropicTier(t *testing.T) {
	for _, mode := range []string{"fast", "ultrafast"} {
		h := fixtureGateway(t, "codex", "anthropic", anthropicFixtureSSE, "text/event-stream", func(_ *http.Request, in map[string]any) {
			if _, exists := in["service_tier"]; exists {
				t.Error("OpenAI tier leaked onto Anthropic request")
			}
			if in["model"] != "chosen-model" {
				t.Error("unsupported tier changed the selected model")
			}
		})
		a, _ := h.Source("codex")
		a.SpeedMode = mode
		h.Source = func(string) (config.LocalAccount, bool) { return a, true }
		w := callGateway(h, "codex", "responses", `{"model":"chosen-model","input":"synthetic input"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("Anthropic request failed: %d %s", w.Code, w.Body)
		}
	}
}
