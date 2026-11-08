package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"salcara/bridge/internal/codexaccounts"
	"salcara/bridge/internal/config"
)

func summaryAPI(id string) config.LocalAccount {
	return config.LocalAccount{ID: id, Name: id, Kind: "api", SourceType: "api_key", BaseURL: "https://no-network.invalid", Key: "synthetic-summary-secret"}
}

func summaryClient(id, anchor string) config.NamedAPIClient {
	return config.NamedAPIClient{ID: id, Name: id, AccountID: anchor, KeyDigest: config.APIClientDigest("synthetic-summary-client")}
}

func TestAPIServiceSummaryEffectiveMembershipAndNoFalseAvailability(t *testing.T) {
	s, _ := newTestServer(t)
	c := s.d.Store.Get()
	c.LocalAccounts = []config.LocalAccount{summaryAPI("anchor"), summaryAPI("text-a"), summaryAPI("text-b"), summaryAPI("image"), summaryAPI("not-joined")}
	client := summaryClient("client", "anchor")
	client.AccountIDs, client.ImageAccountIDs = []string{"text-a", "text-b"}, []string{"image"}
	client.PoolStrategy, client.PoolAccountID = "single_account", "text-b"
	disabled := summaryClient("disabled", "not-joined")
	disabled.Disabled = true
	c.CodexAPI.Clients = []config.NamedAPIClient{client, client, disabled}
	for _, tc := range []struct {
		mode string
		ids  []string
	}{
		{"", []string{"text-b"}}, {"enabled", []string{"image", "text-b"}}, {"images_only", []string{"image"}},
	} {
		c.CodexAPI.ImagesMode = tc.mode
		out := s.readAPIServiceSummary(c)
		ids := []string{}
		for _, member := range out.Members {
			ids = append(ids, member.ID)
			if member.Availability != "unknown" || member.Usage != nil {
				t.Fatalf("config-only source claimed provider availability: %+v", member)
			}
		}
		if !reflect.DeepEqual(ids, tc.ids) {
			t.Fatalf("mode %q membership: %v want %v", tc.mode, ids, tc.ids)
		}
	}
}

func TestAPIServiceSummaryBrokenPoolDoesNotAdvertiseHealthyMember(t *testing.T) {
	s, _ := newTestServer(t)
	c := s.d.Store.Get()
	c.LocalAccounts = []config.LocalAccount{summaryAPI("present")}
	client := summaryClient("client", "present")
	client.AccountIDs = []string{"present", "missing"}
	c.CodexAPI.Clients = []config.NamedAPIClient{client}
	out := s.readAPIServiceSummary(c)
	if len(out.Members) != 2 {
		t.Fatal("broken membership hidden")
	}
	for _, row := range out.Members {
		if row.Availability != "unavailable" {
			t.Fatal("broken pool advertised availability")
		}
	}
	c.CodexAPI.Clients = append(c.CodexAPI.Clients, summaryClient("valid-route", "present"))
	out = s.readAPIServiceSummary(c)
	if out.Members[1].ID != "present" || out.Members[1].Availability != "unknown" {
		t.Fatal("independent valid route lost")
	}
}

func TestAPIServiceSummaryVerifiedQuotaDedupAndIdentityFence(t *testing.T) {
	remaining, calls, failed := 37.0, 0, false
	s, h := officialConsoleWithUsage(t, codexaccounts.UsageFetchFunc(func(context.Context, *coreauth.Auth) (codexaccounts.UsageFetchResult, error) {
		calls++
		if failed {
			return codexaccounts.UsageFetchResult{}, errors.New("synthetic usage failure")
		}
		seconds := int64(604800)
		return codexaccounts.UsageFetchResult{ProviderAccountID: "fixture-provider-id", Snapshot: codexaccounts.UsageSnapshot{
			Plan: codexaccounts.UsagePlan{Raw: "business"}, RateLimit: &codexaccounts.UsageLimit{ID: "codex", Primary: &codexaccounts.UsageWindow{RemainingPercent: &remaining, WindowSeconds: &seconds}},
		}}, nil
	}))
	s.d.OfficialGateway = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
		t.Fatal("summary called inference")
		return nil, nil
	}
	ref := importOfficialConsole(t, s, h)
	c := s.d.Store.Get()
	var original config.LocalAccount
	for _, a := range c.LocalAccounts {
		if a.CredentialRef == ref {
			original = a
			break
		}
	}
	if original.ID == "" {
		t.Fatal("missing synthetic account")
	}
	duplicate := original
	duplicate.ID, duplicate.Name = "duplicate-handle", "duplicate"
	c.LocalAccounts = append(c.LocalAccounts, duplicate)
	c.CodexAPI.Clients = []config.NamedAPIClient{summaryClient("one", original.ID), summaryClient("two", duplicate.ID)}
	before := s.d.Store.Get()
	get := func(want, reason string) {
		t.Helper()
		networkBefore := calls
		out := s.readAPIServiceSummary(c)
		if calls != networkBefore || len(out.Members) != 1 || out.Members[0].Availability != want || out.Members[0].Reason != reason {
			t.Fatalf("summary: %+v", out)
		}
		raw, _ := json.Marshal(out)
		assertNoOfficialSecrets(t, string(raw))
		if strings.Contains(string(raw), original.Key) {
			t.Fatal("protected marker leaked")
		}
	}
	get("unknown", "upstream_not_verified")
	if _, err := s.d.CodexAccounts.RefreshUsage(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	get("ready", "verified_quota")
	remaining = 0
	if _, err := s.d.CodexAccounts.RefreshUsage(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	get("unavailable", "quota_exhausted")
	failed = true
	_, _ = s.d.CodexAccounts.RefreshUsage(context.Background(), ref)
	get("unknown", "upstream_not_verified")
	if !reflect.DeepEqual(before, s.d.Store.Get()) {
		t.Fatal("summary changed config")
	}
	for i := range c.LocalAccounts {
		c.LocalAccounts[i].CredentialEpoch++
	}
	out := s.readAPIServiceSummary(c)
	if out.Members[0].Availability != "unavailable" || out.Members[0].Usage != nil {
		t.Fatal("old identity quota leaked into replacement")
	}
}

func TestAPIServiceSummaryStatusEndpointIsCacheOnly(t *testing.T) {
	s, h := readinessFixture(t)
	w := localRequest(s, h, "GET", "/api/local/codex-api", nil)
	if w.Code != 200 {
		t.Fatal("status failed")
	}
	var out struct {
		Summary apiServiceSummary `json:"summary"`
	}
	if json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Summary.CheckedAt == 0 || len(out.Summary.Members) == 0 {
		t.Fatal("summary missing")
	}
	assertNoOfficialSecrets(t, w.Body.String())
	for _, row := range out.Summary.Members {
		if row.Availability == "ready" {
			t.Fatal("uncertain account claimed ready")
		}
	}
}

func TestManagedFirstSetupDoesNotReturnKeyOrChangeManualClients(t *testing.T) {
	s, h := newTestServer(t)
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.LocalAccounts = []config.LocalAccount{summaryAPI("setup-account")}
		c.CodexAPI.Clients = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w := localRequest(s, h, "POST", "/api/local/codex-api/clients", map[string]any{"name": "本机客户端", "accountId": "setup-account", "onlyIfEmpty": true, "suppressKey": true})
	if w.Code != 200 || strings.Contains(w.Body.String(), `"key"`) || strings.Contains(w.Body.String(), "sk-salcara-client-") {
		t.Fatal("automatic setup exposed a manual key")
	}
	clients := s.d.Store.Get().CodexAPI.Clients
	if len(clients) != 1 || config.ValidateAPIClient(clients[0]) != nil {
		t.Fatal("managed setup did not preserve valid client policy")
	}
	for _, request := range []map[string]any{
		{"name": "extra", "accountId": "setup-account", "suppressKey": true},
		{"id": clients[0].ID, "name": "edited", "accountId": "setup-account", "onlyIfEmpty": true, "suppressKey": true},
		{"id": clients[0].ID, "rotate": true, "name": "rotated", "accountId": "setup-account", "suppressKey": true},
	} {
		if got := localRequest(s, h, "POST", "/api/local/codex-api/clients", request); got.Code != 400 {
			t.Fatal("suppression accepted outside explicit first setup")
		}
	}
	if !reflect.DeepEqual(clients, s.d.Store.Get().CodexAPI.Clients) {
		t.Fatal("bad setup replaced client policy")
	}
}
