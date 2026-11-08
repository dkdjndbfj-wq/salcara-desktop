package console

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func readinessFixture(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	s, _ := newTestServer(t)
	s.d.Store.SetRuntimeGatewayKey("synthetic-readiness-runtime")
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.LocalAccounts = []config.LocalAccount{{ID: "readiness-api", Name: "Ready display", Kind: "api", SourceType: "api_key", BaseURL: "https://must-not-contact.invalid", Key: "private-provider-secret", Model: "gpt-allowed", Models: []string{"gpt-allowed"}, ModelDenylist: []string{"gpt-blocked"}}, {ID: "bad-api", Name: "Bad API", Kind: "api", SourceType: "api_key", BaseURL: "invalid private address", Key: "bad-private-key"}, {ID: "official-unavailable", Name: "Official display", Kind: "api", SourceType: "official_account", ProviderID: "openai", CredentialRef: "vault-readiness", CredentialEpoch: 1, BaseURL: "https://chatgpt.com/backend-api/codex", Key: "protected-readiness-marker", Wire: "responses"}}
		c.CodexAPI = config.CodexAPIService{Enabled: true, Clients: []config.NamedAPIClient{
			{ID: "ready-client", Name: "Ready", AccountID: "readiness-api", KeyDigest: config.APIClientDigest("synthetic-readiness-key"), ModelAllowPrefixes: []string{"gpt-"}, ModelDenyPrefixes: []string{"gpt-block"}, ModelAliases: map[string]string{"friendly": "gpt-allowed"}},
			{ID: "disabled-client", Name: "Disabled", AccountID: "readiness-api", KeyDigest: config.APIClientDigest("synthetic-disabled-key"), Disabled: true},
			{ID: "invalid-client", Name: "Invalid", AccountID: "readiness-api", KeyDigest: "private-invalid-digest"},
			{ID: "missing-client", Name: "Missing", AccountID: "not-present", KeyDigest: config.APIClientDigest("synthetic-missing-key")},
			{ID: "bad-account-client", Name: "Bad account", AccountID: "bad-api", KeyDigest: config.APIClientDigest("synthetic-bad-account-key")},
			{ID: "official-client", Name: "Official", AccountID: "official-unavailable", KeyDigest: config.APIClientDigest("synthetic-official-key")},
		}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return s, s.Handler()
}
func readReadiness(t *testing.T, s *Server, h http.Handler) apiReadiness {
	t.Helper()
	w := localRequest(s, h, "GET", "/api/local/codex-api/readiness", nil)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	var out apiReadiness
	if json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatal("invalid readiness DTO")
	}
	return out
}

func TestReadinessDistinguishesRestartRecoveryAndInvalidBindingsWithoutNetworkOrMutation(t *testing.T) {
	s, h := readinessFixture(t)
	before := s.d.Store.Get()
	s.apiService.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
		t.Error("readiness executed inference")
		return nil, nil
	}
	s.apiService.ModelCatalog = func(context.Context, config.LocalAccount, bool) (protocol.ModelCatalog, error) {
		t.Error("readiness refreshed model data")
		return protocol.ModelCatalog{}, nil
	}
	out := readReadiness(t, s, h)
	if !out.Configured || out.Serving || !out.RequiresStart || !out.HandlerAvailable || out.UpstreamVerified || out.ProviderEntitlement != "not_checked" || out.ReadyClients != 1 {
		t.Fatalf("unverified/restart state presented as live: %+v", out)
	}
	want := []string{"configured", "client_disabled", "client_configuration_invalid", "account_missing", "account_configuration_invalid", "official_executor_unavailable"}
	if len(out.Clients) != len(want) {
		t.Fatal("client readiness rows lost")
	}
	for i, row := range out.Clients {
		if row.State != want[i] || row.KeyAuthentication != "not_checked" {
			t.Fatalf("client %d false readiness/auth claim: %+v", i, row)
		}
	}
	if !reflect.DeepEqual(before, s.d.Store.Get()) || len(s.apiService.Snapshot()) != 0 {
		t.Fatal("readiness changed authorization or request statistics")
	}
	w := localRequest(s, h, "GET", "/api/local/codex-api/readiness", nil)
	for _, secret := range []string{"private-provider-secret", "bad-private-key", "private-invalid-digest", "invalid private address", "protected-readiness-marker", "synthetic-readiness-key"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("readiness leaked secret/config source detail: %s", secret)
		}
	}
	if w := localRequest(s, h, "POST", "/api/local/codex-api/enabled", map[string]bool{"enabled": true}); w.Code != 200 {
		t.Fatal(w.Body)
	}
	out = readReadiness(t, s, h)
	if !out.Serving || out.RequiresStart {
		t.Fatal("explicit runtime service start not reflected")
	}
}

func TestReadinessKeyCheckSeparatesAuthenticationLocalModelPolicyAndProviderEntitlement(t *testing.T) {
	s, h := readinessFixture(t)
	before := s.d.Store.Get()
	for _, test := range []struct {
		key, model, state string
		auth              bool
		policy            *bool
	}{
		{"synthetic-readiness-key", "friendly", "configured", true, boolPointer(true)},
		{"synthetic-readiness-key", "gpt-blocked", "configured", true, boolPointer(false)},
		{"synthetic-readiness-key", "other-model", "configured", true, boolPointer(false)},
		{"synthetic-readiness-key", "", "configured", true, nil},
		{"synthetic-disabled-key", "gpt-allowed", "client_disabled", false, nil},
		{"unknown-private-key", "gpt-allowed", "client_key_invalid", false, nil},
	} {
		w := localRequest(s, h, "POST", "/api/local/codex-api/key-check", map[string]string{"key": test.key, "model": test.model})
		if w.Code != 200 {
			t.Fatal(w.Body)
		}
		var out struct {
			Authenticated       bool
			State               string
			Serving             bool
			ModelPolicyAllowed  *bool
			UpstreamVerified    bool
			ProviderEntitlement string
		}
		if json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Authenticated != test.auth || out.State != test.state || out.Serving || out.UpstreamVerified || out.ProviderEntitlement != "not_checked" || !reflect.DeepEqual(out.ModelPolicyAllowed, test.policy) {
			t.Fatalf("key check claimed incorrect permission/verification: %s", w.Body)
		}
		if strings.Contains(w.Body.String(), test.key) {
			t.Fatal("key check echoed submitted secret")
		}
	}
	if !reflect.DeepEqual(before, s.d.Store.Get()) || len(s.apiService.Snapshot()) != 0 {
		t.Fatal("local key/policy checks mutated service or counted an inference")
	}
}

func TestReadinessDoesNotTreatExecutorClosureAsAvailableProtectedCredentials(t *testing.T) {
	s, h := readinessFixture(t)
	s.d.OfficialGateway = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
		t.Fatal("readiness invoked an executor")
		return nil, nil
	}
	s.d.CodexAccounts = nil
	out := readReadiness(t, s, h)
	if out.Clients[len(out.Clients)-1].State != "official_executor_unavailable" || out.ReadyClients != 1 {
		t.Fatal("unavailable protected manager presented as ready")
	}
}
func boolPointer(value bool) *bool { return &value }

func TestReadinessAndModelCheckUseExplicitTextMembers(t *testing.T) {
	s, h := readinessFixture(t)
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.LocalAccounts = append(c.LocalAccounts, config.LocalAccount{ID: "pool-member", Name: "Pool member", Kind: "api", SourceType: "api_key", BaseURL: "https://fixture.invalid", Key: "synthetic-pool-key", Model: "pool-only", Models: []string{"pool-only"}})
		c.CodexAPI.Clients[0].AccountIDs = []string{"pool-member"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	out := readReadiness(t, s, h)
	if out.Clients[0].State != "configured" || len(out.Clients[0].Members) != 1 || out.Clients[0].Members[0].Name != "Pool member" {
		t.Fatalf("pool readiness: %+v", out.Clients[0])
	}
	// The client's namespace ACL still applies before member policies.
	if err := s.d.Store.Update(func(c *config.Config) error { c.CodexAPI.Clients[0].ModelAllowPrefixes = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	w := localRequest(s, h, "POST", "/api/local/codex-api/key-check", map[string]string{"key": "synthetic-readiness-key", "model": "pool-only"})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"modelPolicyAllowed":true`) {
		t.Fatalf("pool model check: %d %s", w.Code, w.Body)
	}
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.CodexAPI.Clients[0].AccountIDs = append(c.CodexAPI.Clients[0].AccountIDs, "removed-member")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	out = readReadiness(t, s, h)
	if out.Clients[0].State != "pool_configuration_invalid" {
		t.Fatalf("missing member reported ready: %+v", out.Clients[0])
	}
	w = localRequest(s, h, "POST", "/api/local/codex-api/key-check", map[string]string{"key": "synthetic-readiness-key", "model": "pool-only"})
	if !strings.Contains(w.Body.String(), `"modelPolicyAllowed":false`) {
		t.Fatalf("missing member policy: %s", w.Body)
	}
}

func TestReadinessAndKeyChecksRequireConsoleAuthorizationAndRejectQuerySecrets(t *testing.T) {
	s, h := readinessFixture(t)
	for _, path := range []string{"/api/local/codex-api/readiness", "/api/local/codex-api/key-check"} {
		method := "GET"
		if strings.HasSuffix(path, "key-check") {
			method = "POST"
		}
		if w := do(h, method, path, "127.0.0.1:47831", nil, `{}`); w.Code != 401 {
			t.Fatal("readiness bypassed console authorization")
		}
	}
	if w := localRequest(s, h, "GET", "/api/local/codex-api/readiness?key=private", nil); w.Code != 400 {
		t.Fatal("readiness accepted secrets in a query")
	}
	for _, body := range []string{`{"key":"x","key":"y"}`, `{"Key":"x"}`, `{"key":"x","clientId":"other"}`, `{"key":"x"} {}`} {
		if w := do(h, "POST", "/api/local/codex-api/key-check", "127.0.0.1:47831", map[string]string{"Cookie": CookieName + "=" + s.Token(), "Content-Type": "application/json"}, body); w.Code != 400 {
			t.Fatal("ambiguous key check accepted")
		}
	}
	if w := do(h, "POST", "/api/local/codex-api/key-check", "127.0.0.1:47831", map[string]string{"Cookie": CookieName + "=" + s.Token(), "Content-Type": "application/json", "Origin": "https://evil.example"}, `{"key":"synthetic-readiness-key"}`); w.Code != 403 {
		t.Fatal("other origin could probe client key")
	}
}
