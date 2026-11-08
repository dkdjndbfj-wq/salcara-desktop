package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func TestStationGatewayRejectsForeignSelectionWithoutUpstreamTraffic(t *testing.T) {
	var aCalls, bCalls atomic.Int32
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aCalls.Add(1)
		w.Write([]byte(`{"id":"allowed","output":[]}`))
	}))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { bCalls.Add(1); w.Write([]byte(`{"id":"forbidden"}`)) }))
	defer b.Close()
	c := config.Config{RemoteExecutionArmed: true, GatewayRuntimeKey: "fixture-runtime", LocalAccounts: []config.LocalAccount{{ID: "a", Kind: "api", SourceType: "api_key", BaseURL: "https://a.invalid", Key: "fixture-a", Wire: "responses", Model: "fixture-model"}, {ID: "b", Kind: "api", SourceType: "api_key", BaseURL: b.URL, Key: "fixture-b", Wire: "responses", Model: "fixture-model"}}, RemoteAPI: map[string]config.RemoteAPI{"codex": {AccountID: "a", Model: "fixture-model"}}}
	c.RemoteStationAuthorization = &config.StationAuthorization{Identity: config.RemoteIdentity(c), Revision: "r1", Station: protocol.StationPolicy{Version: 1, APIBases: []string{"https://a.invalid"}}}
	c.LocalAccounts[0].BaseURL = a.URL
	c.RemoteStationAuthorization.Station.APIBases = []string{a.URL}
	key := c.RemoteGatewayCredential("remote-codex")
	if key == "" {
		t.Fatal("missing legitimate A lease")
	}
	h := New(nil, func(target string) (config.LocalAccount, bool) { return c.GatewaySourceAccount(target) })
	h.KeyForTarget = func(target string) string { return c.RemoteGatewayCredential(target) }
	requestA := httptest.NewRequest("POST", "/gateway/remote-codex/v1/responses", strings.NewReader(`{"model":"fixture-model","input":"test"}`))
	requestA.RemoteAddr = "127.0.0.1:12345"
	requestA.Header.Set("Authorization", "Bearer "+key)
	requestA.Header.Set("Content-Type", "application/json")
	responseA := httptest.NewRecorder()
	h.ServeHTTP(responseA, requestA)
	if responseA.Code != http.StatusOK || aCalls.Load() != 1 || bCalls.Load() != 0 {
		t.Fatalf("authorized A did not execute through gateway: %d %s", responseA.Code, responseA.Body.String())
	}
	c.RemoteAPI["codex"] = config.RemoteAPI{AccountID: "b", Model: "fixture-model"}
	for _, credential := range []string{key, c.GatewayRuntimeKey, "fixture-b"} {
		r := httptest.NewRequest("POST", "/gateway/remote-codex/v1/responses", strings.NewReader(`{"model":"fixture-model","input":"test"}`))
		r.RemoteAddr = "127.0.0.1:12345"
		r.Header.Set("Authorization", "Bearer "+credential)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized || bCalls.Load() != 0 {
			t.Fatalf("foreign account executed: status=%d calls=%d", w.Code, bCalls.Load())
		}
	}
}
