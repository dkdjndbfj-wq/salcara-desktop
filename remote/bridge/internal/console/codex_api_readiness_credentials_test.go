package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"salcara/bridge/internal/codexaccounts"
	"salcara/bridge/internal/config"
)

type readinessProtector struct{ fail atomic.Bool }

func (p *readinessProtector) Protect(raw []byte) ([]byte, error) {
	if p.fail.Load() {
		return nil, errors.New("synthetic private save failure")
	}
	return append([]byte(nil), raw...), nil
}
func (*readinessProtector) Unprotect(raw []byte) ([]byte, error) {
	return append([]byte(nil), raw...), nil
}

func TestReadinessSeparatesOfficialCredentialActionFromUsableOldAccess(t *testing.T) {
	for _, test := range []struct {
		name, access, expiry, action, state, credentialState string
		ready                                                bool
	}{
		{"opaque-valid", "synthetic-old-access", "", "", "configured", "ready", true},
		{"expired", "synthetic-old-access", "2000-01-01T00:00:00Z", "", "credential_refresh_required", "refresh_required", false},
		{"refresh-only", "", "2099-01-01T00:00:00Z", "", "credential_refresh_required", "refresh_required", false},
		{"refresh-only-failed", "", "", "transient", "credential_refresh_required", "refresh_required", false},
		{"refresh-only-no-refresher", "", "", "unavailable", "credential_refresh_failed", "refresh_failed", false},
		{"needs-login-expired", "synthetic-old-access", "2000-01-01T00:00:00Z", "login", "credential_login_required", "login_required", false},
		{"needs-login-valid-old", "synthetic-old-access", "2099-01-01T00:00:00Z", "login", "configured", "login_required", true},
		{"save-pending-expired", "synthetic-old-access", "2000-01-01T00:00:00Z", "save", "credential_save_pending", "save_pending", false},
		{"save-pending-valid-old", "synthetic-old-access", "2099-01-01T00:00:00Z", "save", "configured", "save_pending", true},
		{"refresh-failed-valid-old", "synthetic-old-access", "2099-01-01T00:00:00Z", "transient", "configured", "refresh_failed", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, _ := newTestServer(t)
			protector := &readinessProtector{}
			var refreshes atomic.Int32
			var refresher codexaccounts.Refresher = codexaccounts.RefreshFunc(func(_ context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
				refreshes.Add(1)
				if test.action == "login" {
					return nil, codexaccounts.ErrNeedsLogin
				}
				if test.action == "transient" {
					return nil, errors.New("synthetic transient exchange failure")
				}
				r := a.Clone()
				r.Metadata["access_token"] = "synthetic-new-access"
				r.Metadata["refresh_token"] = "synthetic-new-refresh"
				r.Metadata["expired"] = "2099-01-01T00:00:00Z"
				return r, nil
			})
			if test.action == "unavailable" {
				refresher = nil
			}
			accounts, err := codexaccounts.New(codexaccounts.Options{Path: filepath.Join(t.TempDir(), "vault.json"), Protector: protector, Refresher: refresher})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(accounts.Close)
			raw, _ := json.Marshal(map[string]string{"access_token": test.access, "refresh_token": "synthetic-old-refresh", "expired": test.expiry})
			p, err := codexaccounts.ParseImport(raw)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := accounts.Import(context.Background(), p, "")
			if err != nil {
				t.Fatal(err)
			}
			if test.action == "save" {
				protector.fail.Store(true)
			}
			if test.action != "" && test.action != "unavailable" {
				if _, err = accounts.Refresh(context.Background(), rows[0].ID); err == nil {
					t.Fatal("failure-state setup succeeded")
				}
			}
			s.d.CodexAccounts = accounts
			s.d.OfficialGateway = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
				t.Fatal("readiness dispatched inference")
				return nil, nil
			}
			if err = s.d.Store.Update(func(c *config.Config) error {
				c.LocalAccounts = []config.LocalAccount{{ID: "availability-account", Kind: "api", Name: "Synthetic display", SourceType: "official_account", ProviderID: "openai", CredentialRef: rows[0].ID, CredentialEpoch: rows[0].IdentityEpoch, BaseURL: "https://chatgpt.com/backend-api/codex", Key: "protected-private-marker", Wire: "responses"}}
				c.CodexAPI.Clients = []config.NamedAPIClient{{ID: "availability-client", Name: "Client display", AccountID: "availability-account", KeyDigest: config.APIClientDigest("synthetic-private-client")}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			h := s.Handler()
			beforeAccounts, beforeConfig, beforeRefreshes := accounts.List(), s.d.Store.Get(), refreshes.Load()
			out := readReadiness(t, s, h)
			wantReady := 0
			if test.ready {
				wantReady = 1
			}
			if len(out.Clients) != 1 || out.Clients[0].State != test.state || out.Clients[0].CredentialState != test.credentialState || out.ReadyClients != wantReady || out.UpstreamVerified {
				t.Fatalf("incorrect credential readiness: %+v", out)
			}
			w := localRequest(s, h, "POST", "/api/local/codex-api/key-check", map[string]string{"key": "synthetic-private-client"})
			var key struct {
				Authenticated          bool
				State, CredentialState string
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &key) != nil || !key.Authenticated || key.State != test.state || key.CredentialState != test.credentialState {
				t.Fatal("client authentication confused with usable credentials", w.Body)
			}
			if strings.Contains(w.Body.String(), "synthetic-old-") || strings.Contains(w.Body.String(), "synthetic-private-") || strings.Contains(w.Body.String(), "protected-private-") {
				t.Fatal("credential-state response leaked secret")
			}
			if !reflect.DeepEqual(beforeAccounts, accounts.List()) || !reflect.DeepEqual(beforeConfig, s.d.Store.Get()) || refreshes.Load() != beforeRefreshes {
				t.Fatal("readiness changed credentials, authorization or refreshed an upstream")
			}
		})
	}
}
