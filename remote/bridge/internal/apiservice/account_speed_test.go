package apiservice

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
)

func TestNamedServiceAndCodexProfileApplyAccountSpeed(t *testing.T) {
	for _, mode := range []string{"standard", "fast", "ultrafast"} {
		for _, profile := range []bool{false, true} {
			s, live, key := profileFixtureService(t)
			live.change(func(c *config.Config) { c.LocalAccounts[0].SpeedMode = mode })
			calls := 0
			s.Official = func(_ context.Context, a config.LocalAccount, body []byte, stream, compact bool, current func() bool) (*http.Response, error) {
				calls++
				var in map[string]any
				if json.Unmarshal(body, &in) != nil || in["model"] != "gpt-fixture" || !current() {
					t.Error("service changed model or lost authorization")
				}
				want := config.AccountServiceTier(mode)
				if want == "" {
					if in["service_tier"] != nil {
						t.Error("standard injected a tier")
					}
				} else if in["service_tier"] != want {
					t.Errorf("wrong tier: %v", in["service_tier"])
				}
				return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
			}
			route := "/codex-api/v1/responses"
			if profile {
				route = serviceProfileRoute
			} else {
				key = syntheticClientKey
			}
			w := profileRequest(s, route, key, "")
			if w.Code != 200 || calls != 1 {
				t.Fatalf("mode %s profile %v: %d %s", mode, profile, w.Code, w.Body)
			}
		}
	}
}
