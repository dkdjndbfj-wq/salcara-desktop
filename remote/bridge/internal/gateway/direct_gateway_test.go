package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"salcara/bridge/internal/config"
)

func directGatewayFixture() config.Config {
	c := config.Config{
		GatewayRuntimeKey: "synthetic-direct-runtime", GatewayAuthEpochs: map[string]string{},
		DirectRelayEnabled: true, DirectPhoneID: "direct-test-phone", DirectPhoneHash: "synthetic-phone-binding",
		DirectAPI: map[string]config.RemoteAPI{},
		LocalAccounts: []config.LocalAccount{
			{ID: "selected", Kind: "api", Key: "selected-private-key", BaseURL: "https://selected.invalid", Models: []string{"selected-model"}, PoolEnabled: true, PoolGroup: "same-group"},
			{ID: "other", Kind: "api", Key: "other-private-key", BaseURL: "https://other.invalid", Models: []string{"other-model"}, PoolEnabled: true, PoolGroup: "same-group"},
		},
	}
	for _, family := range []string{"codex", "claude", "deepseek-harness", "hermes"} {
		c.DirectAPI[family] = config.RemoteAPI{AccountID: "selected", Model: "selected-model"}
		c.GatewayAuthEpochs[config.DirectGatewayTarget(family)] = "synthetic-route-epoch"
	}
	return c
}

func TestDirectGatewayRoutesKeepCredentialsAndCatalogsSeparateFromPools(t *testing.T) {
	c := directGatewayFixture()
	h := New(nil, func(target string) (config.LocalAccount, bool) { return c.GatewaySourceAccount(target) })
	h.KeyForTarget = func(target string) string { return c.DirectGatewayCredential(target) }
	h.AuthorizationRevision = func(target string) string { return c.GatewayAuthorizationRevision(target) }
	h.Pool, h.PoolEnabled = NewAccountPool(), func(string) bool { return true }
	h.Accounts = func(target string) []config.LocalAccount {
		family := strings.TrimPrefix(target, "direct-")
		return []config.LocalAccount{c.APIForTool(c.LocalAccounts[0], family), c.APIForTool(c.LocalAccounts[1], family)}
	}
	request := func(target, key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/gateway/"+target+"/v1/models", nil)
		r.RemoteAddr = "127.0.0.1:12345"
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, family := range []string{"codex", "claude", "deepseek-harness", "hermes"} {
		target := config.DirectGatewayTarget(family)
		key := c.DirectGatewayCredential(target)
		w := request(target, key)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "selected-model") || strings.Contains(w.Body.String(), "other-model") {
			t.Fatalf("%s inherited another pool member: status=%d body=%s", target, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "private-key") {
			t.Fatal("catalog exposed an upstream credential")
		}
		wrong := c.DirectGatewayCredential("direct-codex")
		if family == "codex" {
			wrong = c.DirectGatewayCredential("direct-claude")
		}
		if request(target, wrong).Code != http.StatusUnauthorized {
			t.Fatal("another family's direct credential was accepted")
		}
	}
	for _, target := range []string{"direct-remote-codex", "direct-claude-desktop", "direct-shell"} {
		if request(target, c.DirectGatewayCredential("direct-codex")).Code != http.StatusNotFound {
			t.Fatal("unexpected direct route was exposed")
		}
	}
	oldKey := c.DirectGatewayCredential("direct-codex")
	c.DirectRelayEnabled = false
	if request("direct-codex", oldKey).Code != http.StatusUnauthorized {
		t.Fatal("revoked direct worker still reached the gateway")
	}
}

func TestDirectGatewayRejectsSourceChangedAfterCredentialAdmission(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(http.StatusNoContent) }))
	defer upstream.Close()
	c := directGatewayFixture()
	for i := range c.LocalAccounts {
		c.LocalAccounts[i].BaseURL = upstream.URL
		c.LocalAccounts[i].PoolEnabled = false
	}
	key := c.DirectGatewayCredential("direct-codex")
	h := New(nil, func(target string) (config.LocalAccount, bool) {
		c.DirectAPI["codex"] = config.RemoteAPI{AccountID: "other", Model: "other-model"}
		return c.GatewaySourceAccount(target)
	})
	h.KeyForTarget = func(target string) string { return c.DirectGatewayCredential(target) }
	r := httptest.NewRequest(http.MethodPost, "/gateway/direct-codex/v1/responses", strings.NewReader(`{"model":"selected-model","input":"fixture"}`))
	r.RemoteAddr = "127.0.0.1:12345"
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusConflict || calls.Load() != 0 {
		t.Fatalf("stale direct source dispatched: status=%d upstream=%d", w.Code, calls.Load())
	}
}
