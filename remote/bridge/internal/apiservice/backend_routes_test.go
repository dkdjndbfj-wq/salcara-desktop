package apiservice

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func TestBackendRoutesReuseNamedSourceAliasAndPolicy(t *testing.T) {
	for _, prefix := range []string{"/backend-api/codex/", "/codex-api/backend-api/codex/"} {
		t.Run(prefix, func(t *testing.T) {
			s, live := fixtureService(t)
			s.ModelCatalog = func(context.Context, config.LocalAccount, bool) (protocol.ModelCatalog, error) {
				return protocol.ModelCatalog{Models: []string{"gpt-fixture"}}, nil
			}
			live.change(func(c *config.Config) {
				c.CodexAPI.Clients[0].ModelPrefix = "team/"
				c.CodexAPI.Clients[0].ModelAllowlist = []string{"gpt-fixture"}
			})
			calls := 0
			s.Official = func(_ context.Context, a config.LocalAccount, body []byte, stream, compact bool, current func() bool) (*http.Response, error) {
				calls++
				var request map[string]any
				if a.ID != "account-A" || !current() || json.Unmarshal(body, &request) != nil || request["model"] != "gpt-fixture" || stream {
					t.Error("backend alias changed source or model")
				}
				if (calls == 2) != compact {
					t.Error("backend compact alias lost protocol mode")
				}
				return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
			}
			for _, suffix := range []string{"responses", "responses/compact"} {
				r := apiRequest(context.Background(), syntheticClientKey, strings.NewReader(`{"model":"team/friendly","input":"fixture"}`))
				r.URL.Path = prefix + suffix
				w := httptest.NewRecorder()
				s.ServeHTTP(w, r)
				if w.Code != 200 {
					t.Fatalf("backend route unavailable: %d %s", w.Code, w.Body)
				}
			}
			for _, tc := range []struct {
				suffix, model, key string
				status             int
			}{
				{"responses", "friendly", syntheticClientKey, 403},
				{"responses", "team/denied", syntheticClientKey, 403},
				{"responses", "team/friendly", "synthetic-unknown-key", 401},
				{"unmanaged-operation", "team/friendly", syntheticClientKey, 404},
			} {
				r := apiRequest(context.Background(), tc.key, strings.NewReader(`{"model":"`+tc.model+`"}`))
				r.URL.Path = prefix + tc.suffix
				w := httptest.NewRecorder()
				s.ServeHTTP(w, r)
				if w.Code != tc.status {
					t.Fatalf("backend policy bypass: %d %s", w.Code, w.Body)
				}
			}
			if calls != 2 {
				t.Fatalf("rejected backend path executed: %d", calls)
			}
			r := apiRequest(context.Background(), syntheticClientKey, nil)
			r.URL.Path = prefix + "models"
			r.Method = http.MethodGet
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != 200 || !strings.Contains(w.Body.String(), "team/friendly") {
				t.Fatalf("backend discovery bypassed namespace: %d %s", w.Code, w.Body)
			}
		})
	}
}

func TestBackendWebSocketUsesSameNamedTurnAndLease(t *testing.T) {
	for _, prefix := range []string{"/backend-api/codex/", "/codex-api/backend-api/codex/"} {
		t.Run(prefix, func(t *testing.T) {
			s, _ := fixtureService(t)
			s.Official = func(_ context.Context, a config.LocalAccount, _ []byte, stream, compact bool, current func() bool) (*http.Response, error) {
				if a.ID != "account-A" || !current() || !stream || compact {
					t.Error("backend WS changed protocol/source")
				}
				return successfulResponse(io.NopCloser(strings.NewReader(namedCompleted("backend-ws-fixture"))), true), nil
			}
			address := strings.TrimSuffix(namedSocketServer(t, s), "/codex-api/v1/responses") + prefix + "responses"
			connection := namedSocket(t, address, syntheticClientKey)
			sendNamedFrame(t, connection, map[string]any{"type": "response.create", "model": "friendly", "input": "fixture"})
			result := namedSocketEvent(t, connection, "response.completed")
			if result["response"] == nil {
				t.Fatal("backend WS lost response")
			}
			connection.Close()
			waitNamedSocketReleased(t, s)
		})
	}
}
