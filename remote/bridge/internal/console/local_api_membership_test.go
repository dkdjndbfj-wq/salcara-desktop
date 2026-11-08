package console

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/launcher"
)

func TestLocalAPIServiceMembershipUsesIndependentExecutionSets(t *testing.T) {
	for _, test := range []struct {
		name       string
		accountID  string
		client     config.NamedAPIClient
		quotaOnly  bool
		want       localAPIClientMembership
		configured int
		active     int
	}{
		{
			name: "explicit pools exclude compatibility anchor", accountID: "primary",
			client: config.NamedAPIClient{AccountID: "primary", AccountIDs: []string{"text-member"}, ImageAccountIDs: []string{"image-member"}},
			want:   localAPIClientMembership{Primary: true},
		},
		{
			name: "empty pools fall back to primary exactly once", accountID: "primary",
			client: config.NamedAPIClient{AccountID: "primary"},
			want:   localAPIClientMembership{Primary: true, Text: true, Image: true, Active: true}, configured: 1, active: 1,
		},
		{
			name: "text fallback does not widen explicit image pool", accountID: "primary",
			client: config.NamedAPIClient{AccountID: "primary", ImageAccountIDs: []string{"image-member"}},
			want:   localAPIClientMembership{Primary: true, Text: true, Active: true}, configured: 1, active: 1,
		},
		{
			name: "image fallback does not widen explicit text pool", accountID: "primary",
			client: config.NamedAPIClient{AccountID: "primary", AccountIDs: []string{"text-member"}},
			want:   localAPIClientMembership{Primary: true, Image: true, Active: true}, configured: 1, active: 1,
		},
		{
			name: "text member does not join image pool", accountID: "text-member",
			client: config.NamedAPIClient{AccountID: "primary", AccountIDs: []string{"text-member"}, ImageAccountIDs: []string{"image-member"}},
			want:   localAPIClientMembership{Text: true, Active: true}, configured: 1, active: 1,
		},
		{
			name: "image member does not join text pool", accountID: "image-member",
			client: config.NamedAPIClient{AccountID: "primary", AccountIDs: []string{"text-member"}, ImageAccountIDs: []string{"image-member"}},
			want:   localAPIClientMembership{Image: true, Active: true}, configured: 1, active: 1,
		},
		{
			name: "disabled client retains management membership only", accountID: "primary",
			client: config.NamedAPIClient{AccountID: "primary", Disabled: true},
			want:   localAPIClientMembership{Primary: true, Text: true, Image: true, Disabled: true}, configured: 1,
		},
		{
			name: "disabled pure anchor is not configured membership", accountID: "primary",
			client: config.NamedAPIClient{AccountID: "primary", AccountIDs: []string{"text-member"}, ImageAccountIDs: []string{"image-member"}, Disabled: true},
			want:   localAPIClientMembership{Primary: true, Disabled: true},
		},
		{
			name: "quota only association cannot execute", accountID: "primary", quotaOnly: true,
			client: config.NamedAPIClient{AccountID: "primary"},
			want:   localAPIClientMembership{Primary: true, Text: true, Image: true}, configured: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.client.ID, test.client.Name = "fixture-client", "Fixture client"
			test.want.ClientID, test.want.ClientName = test.client.ID, test.client.Name
			got := localAPIServiceMembership(test.accountID, []config.NamedAPIClient{test.client}, test.quotaOnly)
			want := localAPIServiceMembershipDTO{Member: test.active > 0, ActiveClientCount: test.active, ConfiguredClientCount: test.configured, Clients: []localAPIClientMembership{test.want}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("membership = %+v; want %+v", got, want)
			}
		})
	}
}

func TestLocalAPIServiceMembershipCountsClientsWithoutCrossPoolInheritance(t *testing.T) {
	clients := []config.NamedAPIClient{
		{ID: "active-both", Name: "Both", AccountID: "member"},
		{ID: "active-image", Name: "Image", AccountID: "other", AccountIDs: []string{"other"}, ImageAccountIDs: []string{"member"}},
		{ID: "disabled", Name: "Disabled", AccountID: "member", Disabled: true},
		{ID: "anchor", Name: "Anchor", AccountID: "member", AccountIDs: []string{"other"}, ImageAccountIDs: []string{"other"}},
		{ID: "unrelated", Name: "Unrelated", AccountID: "other", AccountIDs: []string{"separate"}, ImageAccountIDs: []string{"separate"}},
	}
	got := localAPIServiceMembership("member", clients, false)
	if !got.Member || got.ActiveClientCount != 2 || got.ConfiguredClientCount != 3 || len(got.Clients) != 4 {
		t.Fatalf("double counted modality or inherited another pool: %+v", got)
	}
	for _, accountID := range []string{"member-of-same-local-pool", "not-associated"} {
		got := localAPIServiceMembership(accountID, clients, false)
		if got.Member || got.ActiveClientCount != 0 || got.ConfiguredClientCount != 0 || got.Clients == nil || len(got.Clients) != 0 {
			t.Fatalf("unrelated account inherited membership: %+v", got)
		}
	}
}

func TestLocalAccountsProjectsMembershipWithoutSecretsOrRuntimeClaims(t *testing.T) {
	s, h := newTestServer(t)
	s.d.Local.FindTools = func(context.Context, map[string]string) []launcher.Tool { return []launcher.Tool{} }
	const secret = "synthetic-membership-secret"
	const clientKey = "synthetic-membership-client-key"
	if err := s.d.Store.Update(func(c *config.Config) error {
		for _, id := range []string{"anchor", "text-member", "image-member", "same-pool-only"} {
			c.LocalAccounts = append(c.LocalAccounts, config.LocalAccount{ID: id, Name: id, Kind: "api", BaseURL: "https://fixture.invalid", Key: secret, PoolGroup: "same-local-pool", PoolEnabled: true})
		}
		c.CodexAPI.Enabled = false
		c.CodexAPI.Clients = []config.NamedAPIClient{
			{ID: "client-active", Name: "Active", AccountID: "anchor", AccountIDs: []string{"text-member"}, ImageAccountIDs: []string{"image-member"}, KeyDigest: config.APIClientDigest(clientKey)},
			{ID: "client-disabled", Name: "Disabled", AccountID: "anchor", Disabled: true, KeyDigest: config.APIClientDigest(clientKey + "-disabled")},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w := localRequest(s, h, http.MethodGet, "/api/local/accounts", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("accounts response: %d", w.Code)
	}
	for _, private := range []string{secret, clientKey, config.APIClientDigest(clientKey), `"keyDigest"`, `"authEpoch"`} {
		if strings.Contains(w.Body.String(), private) {
			t.Fatal("membership DTO exposed a private credential")
		}
	}
	var payload struct {
		Accounts []struct {
			ID         string                       `json:"id"`
			APIService localAPIServiceMembershipDTO `json:"apiService"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Accounts) != 4 {
		t.Fatal("membership projection lost managed accounts")
	}
	for _, row := range payload.Accounts {
		got := row.APIService
		switch row.ID {
		case "anchor":
			if got.Member || got.ActiveClientCount != 0 || got.ConfiguredClientCount != 1 || len(got.Clients) != 2 || !got.Clients[0].Primary || got.Clients[0].Text || got.Clients[0].Image {
				t.Fatalf("anchor/disabled projection = %+v", got)
			}
		case "text-member", "image-member":
			if !got.Member || got.ActiveClientCount != 1 || got.ConfiguredClientCount != 1 || len(got.Clients) != 1 || got.Clients[0].Primary || got.Clients[0].Text != (row.ID == "text-member") || got.Clients[0].Image != (row.ID == "image-member") {
				t.Fatalf("explicit member projection = %+v", got)
			}
		case "same-pool-only":
			if got.Member || len(got.Clients) != 0 {
				t.Fatalf("local pool widened named-client authorization: %+v", got)
			}
		}
	}
}

func TestLocalAccountsQuotaOnlyStaleAssociationIsNotActive(t *testing.T) {
	s, h := officialConsole(t)
	s.d.Local.FindTools = func(context.Context, map[string]string) []launcher.Tool { return []launcher.Tool{} }
	w := localRequest(s, h, http.MethodPost, "/api/local/codex/import", map[string]any{"credentials": `{"token_source_mode":"chatgpt_web_session","email":"fixture@example.test","tokens":{"access_token":"synthetic-web-session"}}`})
	if w.Code != http.StatusOK {
		t.Fatalf("synthetic web import: %d", w.Code)
	}
	rows := s.d.CodexAccounts.List()
	if len(rows) != 1 || rows[0].Mode != "chatgpt_web_session" {
		t.Fatal("fixture did not import a quota-only session")
	}
	if err := s.d.Store.Update(func(c *config.Config) error {
		// Model an old/stale association. Normal attach already prohibits it.
		c.LocalAccounts = []config.LocalAccount{{ID: "stale-web", Name: "Stale web", Kind: "api", SourceType: "official_account", ProviderID: "openai", CredentialRef: rows[0].ID, CredentialEpoch: rows[0].IdentityEpoch}}
		c.CodexAPI.Clients = []config.NamedAPIClient{{ID: "stale-client", Name: "Stale client", AccountID: "stale-web", KeyDigest: config.APIClientDigest("synthetic-stale-key")}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w = localRequest(s, h, http.MethodGet, "/api/local/accounts", nil)
	var payload struct {
		Accounts []struct {
			APIService localAPIServiceMembershipDTO `json:"apiService"`
		} `json:"accounts"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &payload) != nil || len(payload.Accounts) != 1 {
		t.Fatalf("stale web account response: %d", w.Code)
	}
	got := payload.Accounts[0].APIService
	if got.Member || got.ActiveClientCount != 0 || got.ConfiguredClientCount != 1 || len(got.Clients) != 1 || got.Clients[0].Active || !got.Clients[0].Primary {
		t.Fatalf("quota-only session projected as executable: %+v", got)
	}
	if strings.Contains(w.Body.String(), "synthetic-web-session") || strings.Contains(w.Body.String(), "synthetic-stale-key") {
		t.Fatal("quota-only membership leaked a synthetic credential")
	}
}
