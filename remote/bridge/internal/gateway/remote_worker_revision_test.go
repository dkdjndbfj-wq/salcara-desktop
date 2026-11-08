package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func TestRemoteWorkerCredentialRejectsRestorationBeforeFirstRequest(t *testing.T) {
	for _, family := range []string{"codex", "claude"} {
		for _, change := range []string{"account-selection", "station-revocation"} {
			t.Run(family+"/"+change, func(t *testing.T) {
				store, err := config.Open(filepath.Join(t.TempDir(), "config.json"))
				if err != nil {
					t.Fatal(err)
				}
				store.SetRuntimeGatewayKey("synthetic-runtime-key")
				update := func(fn func(*config.Config)) {
					t.Helper()
					if err := store.Update(func(c *config.Config) error { fn(c); return nil }); err != nil {
						t.Fatal(err)
					}
				}
				model, wire, endpoint := "gpt-fixture", "responses", "responses"
				body := `{"model":"gpt-fixture","input":"synthetic"}`
				response := `{"id":"response-fixture","object":"response","status":"completed","output":[]}`
				if family == "claude" {
					model, wire, endpoint = "claude-fixture", "anthropic", "messages"
					body = `{"model":"claude-fixture","max_tokens":32,"messages":[{"role":"user","content":"synthetic"}]}`
					response = `{"id":"message-fixture","type":"message","role":"assistant","model":"claude-fixture","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
				}
				update(func(c *config.Config) {
					c.LocalAccounts = []config.LocalAccount{
						{ID: "account-A", Kind: "api", SourceType: "api_key", BaseURL: "https://upstream.fixture.test", Key: "synthetic-upstream-A", Model: model, Wire: wire},
						{ID: "account-B", Kind: "api", SourceType: "api_key", BaseURL: "https://upstream.fixture.test", Key: "synthetic-upstream-B", Model: model, Wire: wire},
					}
					c.RemoteAPI = map[string]config.RemoteAPI{family: {AccountID: "account-A", Model: model}}
					c.RemoteStationAuthorization = &config.StationAuthorization{Identity: config.RemoteIdentity(*c), Revision: "r1", Station: protocol.StationPolicy{Version: 1, APIBases: []string{"https://upstream.fixture.test"}}}
				})
				target := config.RemoteGatewayTarget(family)
				workerConfig := store.Get()
				account, ok := workerConfig.GatewaySourceAccount(target)
				if !ok {
					t.Fatal("fixture has no authorized worker source")
				}
				oldConnection := workerConfig.ToolConnection(account, target, 12345)
				if oldConnection.Key == "" {
					t.Fatal("fixture has no worker credential")
				}
				h := New(nil, func(target string) (config.LocalAccount, bool) { return store.Get().GatewaySourceAccount(target) })
				h.KeyForTarget = func(target string) string { return store.Get().RemoteGatewayCredential(target) }
				h.AuthorizationRevision = func(target string) string { return store.Get().GatewayAuthorizationRevision(target) }
				var upstreamCalls atomic.Int32
				h.Client.Transport = compactRoundTripper(func(r *http.Request) (*http.Response, error) {
					upstreamCalls.Add(1)
					if r.URL.Host != "upstream.fixture.test" || r.Header.Get("Authorization") != "Bearer synthetic-upstream-A" {
						t.Error("restored worker used the wrong upstream source")
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(response))}, nil
				})
				request := func(key string) *httptest.ResponseRecorder {
					r := httptest.NewRequest(http.MethodPost, "/gateway/"+target+"/v1/"+endpoint, strings.NewReader(body))
					r.RemoteAddr = "127.0.0.1:12345"
					r.Header.Set("Content-Type", "application/json")
					r.Header.Set("Authorization", "Bearer "+key)
					w := httptest.NewRecorder()
					h.ServeHTTP(w, r)
					return w
				}
				// The worker is waiting before its first gateway request, as it can
				// during native skill reload. Restore the source before HTTP admission.
				update(func(c *config.Config) {
					if change == "account-selection" {
						c.RemoteAPI[family] = config.RemoteAPI{AccountID: "account-B", Model: model}
					} else {
						c.RemoteStationAuthorization = nil
					}
				})
				update(func(c *config.Config) {
					restored := workerConfig.Clone()
					c.RemoteAPI = restored.RemoteAPI
					c.RemoteStationAuthorization = restored.RemoteStationAuthorization
				})
				if w := request(oldConnection.Key); w.Code != http.StatusUnauthorized || upstreamCalls.Load() != 0 {
					t.Fatalf("retired worker reached restored source: status=%d upstream=%d", w.Code, upstreamCalls.Load())
				}
				current := store.Get()
				account, ok = current.GatewaySourceAccount(target)
				if !ok {
					t.Fatal("restoration lost the authorized source")
				}
				newConnection := current.ToolConnection(account, target, 12345)
				if w := request(newConnection.Key); w.Code != http.StatusOK || upstreamCalls.Load() != 1 {
					t.Fatalf("current worker could not use restored source: status=%d upstream=%d body=%s", w.Code, upstreamCalls.Load(), w.Body)
				}
			})
		}
	}
}
