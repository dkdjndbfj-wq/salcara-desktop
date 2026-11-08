package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"salcara/bridge/internal/codexaccounts"
	"salcara/bridge/internal/config"
)

// No real auth files, account tokens, provider requests or browser login.
const officialFixture = `{"type":"codex","email":"fixture@example.test","access_token":"synthetic-access-private","refresh_token":"synthetic-refresh-private","account_id":"fixture-provider-id","expired":"2099-01-01T00:00:00Z"}`

type consoleProtector struct{}

func (consoleProtector) Protect(b []byte) ([]byte, error)   { return append([]byte(nil), b...), nil }
func (consoleProtector) Unprotect(b []byte) ([]byte, error) { return append([]byte(nil), b...), nil }

func officialConsole(t *testing.T) (*Server, http.Handler) {
	return officialConsoleWithUsage(t, nil)
}

func officialConsoleWithUsage(t *testing.T, usage codexaccounts.UsageFetcher) (*Server, http.Handler) {
	return officialConsoleWithUsageAndReset(t, usage, nil)
}

func officialConsoleWithUsageAndReset(t *testing.T, usage codexaccounts.UsageFetcher, reset codexaccounts.ResetCreditConsumer) (*Server, http.Handler) {
	t.Helper()
	s, _ := newTestServer(t)
	vault, err := codexaccounts.New(codexaccounts.Options{Path: filepath.Join(t.TempDir(), "synthetic.vault"), Protector: consoleProtector{}, Login: codexaccounts.LoginFunc(func(context.Context) (*coreauth.Auth, error) {
		var meta map[string]any
		_ = json.Unmarshal([]byte(officialFixture), &meta)
		return &coreauth.Auth{Provider: "codex", Metadata: meta}, nil
	}), UsageFetcher: usage, ResetCreditConsumer: reset})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(vault.Close)
	s.d.CodexAccounts = vault
	return s, s.Handler()
}

func TestOfficialUsageRefreshReturnsVerifiedSanitizedState(t *testing.T) {
	fail := false
	fetcher := codexaccounts.UsageFetchFunc(func(context.Context, *coreauth.Auth) (codexaccounts.UsageFetchResult, error) {
		if fail {
			return codexaccounts.UsageFetchResult{}, errors.New("private body synthetic-access-private fixture-provider-id")
		}
		used, remaining := 25.0, 75.0
		return codexaccounts.UsageFetchResult{ProviderAccountID: "fixture-provider-id", Snapshot: codexaccounts.UsageSnapshot{
			Plan: codexaccounts.UsagePlan{Raw: "team"}, RateLimit: &codexaccounts.UsageLimit{ID: "codex", Primary: &codexaccounts.UsageWindow{UsedPercent: &used, RemainingPercent: &remaining}},
		}}, nil
	})
	s, h := officialConsoleWithUsage(t, fetcher)
	id := importOfficialConsole(t, s, h)
	w := localRequest(s, h, "POST", "/api/local/codex/usage/refresh", map[string]string{"id": id})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ok":true`) || !strings.Contains(w.Body.String(), `"family":"team"`) || !strings.Contains(w.Body.String(), `"remainingPercent":75`) {
		t.Fatalf("verified usage response: %d %s", w.Code, w.Body)
	}
	assertNoOfficialSecrets(t, w.Body.String())
	fail = true
	w = localRequest(s, h, "POST", "/api/local/codex/usage/refresh", map[string]string{"id": id})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ok":false`) || !strings.Contains(w.Body.String(), `"status":"stale"`) {
		t.Fatalf("stale usage response: %d %s", w.Code, w.Body)
	}
	assertNoOfficialSecrets(t, w.Body.String())
}

func assertNoOfficialSecrets(t *testing.T, body string) {
	t.Helper()
	for _, secret := range []string{"synthetic-access-private", "synthetic-refresh-private", "fixture-provider-id", `"keyDigest"`, `"access_token"`, `"refresh_token"`} {
		if strings.Contains(body, secret) {
			t.Fatalf("private credential field in HTTP DTO: %q", secret)
		}
	}
}

func importOfficialConsole(t *testing.T, s *Server, h http.Handler) string {
	t.Helper()
	w := localRequest(s, h, "POST", "/api/local/codex/import", map[string]any{"credentials": officialFixture})
	if w.Code != 200 {
		t.Fatalf("import: %d %s", w.Code, w.Body)
	}
	assertNoOfficialSecrets(t, w.Body.String())
	rows := s.d.CodexAccounts.List()
	if len(rows) != 1 {
		t.Fatal("import not persisted")
	}
	return rows[0].ID
}

func TestOfficialImportExactFileAndSafeDTOs(t *testing.T) {
	s, h := officialConsole(t)
	id := importOfficialConsole(t, s, h)
	for _, path := range []string{"/api/local/codex/accounts", "/api/local/accounts"} {
		w := localRequest(s, h, "GET", path, nil)
		if w.Code != 200 {
			t.Fatal(w.Body)
		}
		assertNoOfficialSecrets(t, w.Body.String())
	}
	card, ok := s.d.Store.Get().LocalAccount("official-" + id)
	row, _ := s.d.CodexAccounts.Get(id)
	if !ok || card.CredentialRef != id || card.CredentialEpoch != row.IdentityEpoch || card.Key != "protected-"+id {
		t.Fatal("protected association lost")
	}
	bad := strings.Replace(officialFixture, `"access_token":"synthetic-access-private"`, `"access_token":"first","access_token":"second"`, 1)
	w := localRequest(s, h, "POST", "/api/local/codex/import/preview", map[string]any{"credentials": bad})
	if w.Code != 400 {
		t.Fatal("exact file duplicate erased or accepted")
	}
	for _, raw := range []string{`{"credentials":{},"credentials":{}}`, `{"Credentials":{}}`, `{"credentials":{}} {}`, `{"credentials":null}`} {
		w := do(h, "POST", "/api/local/codex/import/preview", "127.0.0.1:47831", map[string]string{"Cookie": CookieName + "=" + s.Token(), "Content-Type": "application/json"}, raw)
		if w.Code != 400 {
			t.Fatalf("ambiguous envelope accepted: %s", raw)
		}
	}
}

func TestOfficialDurableImportAndDeleteWithConfigFailure(t *testing.T) {
	s, h := officialConsole(t)
	path := s.d.Store.Path()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	w := localRequest(s, h, "POST", "/api/local/codex/import", map[string]any{"credentials": officialFixture})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"saved":true`) {
		t.Fatalf("durable partial import: %d %s", w.Code, w.Body)
	}
	rows := s.d.CodexAccounts.List()
	if len(rows) != 1 {
		t.Fatal("config error discarded protected import")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	w = localRequest(s, h, "POST", "/api/local/codex/attach", map[string]string{"id": rows[0].ID})
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	w = localRequest(s, h, "POST", "/api/local/codex/delete", map[string]string{"id": rows[0].ID})
	if w.Code != 500 {
		t.Fatal("fixture did not exercise two-file failure")
	}
	if _, err := s.d.CodexAccounts.SDKRecord(context.Background(), rows[0].ID); err == nil {
		t.Fatal("failed config cleanup left executable credentials")
	}
}

func TestCompletedLoginCannotResurrectOrOverwriteIdentity(t *testing.T) {
	for _, action := range []string{"delete", "replace"} {
		t.Run(action, func(t *testing.T) {
			s, h := officialConsole(t)
			login, err := s.d.CodexAccounts.StartLogin(context.Background(), codexaccounts.LoginRequest{})
			if err != nil {
				t.Fatal(err)
			}
			var done codexaccounts.LoginSession
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				done, err = s.d.CodexAccounts.LoginStatus(login.ID)
				if err != nil {
					t.Fatal(err)
				}
				if done.Status != "pending" {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if done.Account == nil {
				t.Fatalf("login did not complete: %+v", done)
			}
			w := localRequest(s, h, "POST", "/api/local/codex/login/status", map[string]string{"id": login.ID})
			if w.Code != 200 {
				t.Fatal(w.Body)
			}
			assertNoOfficialSecrets(t, w.Body.String())
			if action == "delete" {
				w = localRequest(s, h, "POST", "/api/local/codex/delete", map[string]string{"id": done.Account.ID})
			} else {
				w = localRequest(s, h, "POST", "/api/local/codex/import", map[string]any{"credentials": officialFixture, "replaceId": done.Account.ID})
			}
			if w.Code != 200 {
				t.Fatal(w.Body)
			}
			w = localRequest(s, h, "POST", "/api/local/codex/login/status", map[string]string{"id": login.ID})
			if w.Code != 409 {
				t.Fatal("stale completed login was reused")
			}
			card, found := s.d.Store.Get().LocalAccount("official-" + done.Account.ID)
			if action == "delete" && found {
				t.Fatal("deleted card resurrected")
			}
			if action == "replace" && (!found || card.CredentialEpoch == done.Account.IdentityEpoch) {
				t.Fatal("replacement identity overwritten")
			}
		})
	}
}

func TestNamedAPIClientKeyLifecycleNeverExposesDigest(t *testing.T) {
	s, h := officialConsole(t)
	id := importOfficialConsole(t, s, h)
	w := localRequest(s, h, "POST", "/api/local/codex-api/clients", map[string]any{"name": "fixture client", "accountId": "official-" + id, "modelAliases": map[string]string{"friendly": "gpt-fixture"}, "maxConcurrency": 2})
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	var created struct{ ID, Key string }
	if json.Unmarshal(w.Body.Bytes(), &created) != nil || !strings.HasPrefix(created.Key, "sk-salcara-client-") {
		t.Fatal("no new client key")
	}
	oldDigest := s.d.Store.Get().CodexAPI.Clients[0].KeyDigest
	w = localRequest(s, h, "GET", "/api/local/codex-api", nil)
	if strings.Contains(w.Body.String(), created.Key) || strings.Contains(w.Body.String(), oldDigest) {
		t.Fatal("key or digest in status")
	}
	assertNoOfficialSecrets(t, w.Body.String())
	w = localRequest(s, h, "POST", "/api/local/codex-api/clients", map[string]any{"id": created.ID, "name": "renamed", "accountId": "official-" + id})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"key":""`) || s.d.Store.Get().CodexAPI.Clients[0].KeyDigest != oldDigest {
		t.Fatal("edit rotated key or redisplayed it")
	}
	w = localRequest(s, h, "POST", "/api/local/codex-api/clients", map[string]any{"id": created.ID, "name": "renamed", "accountId": "official-" + id, "rotate": true})
	if w.Code != 200 || s.d.Store.Get().CodexAPI.Clients[0].KeyDigest == oldDigest {
		t.Fatal("key did not rotate")
	}
	w = localRequest(s, h, "POST", "/api/local/codex-api/clients/revoke", map[string]string{"id": created.ID})
	if w.Code != 200 || len(s.d.Store.Get().CodexAPI.Clients) != 0 {
		t.Fatal("client not revoked")
	}
}

func TestNamedAPIClientPolicyRoundTripAndValidation(t *testing.T) {
	s, h := officialConsole(t)
	id := importOfficialConsole(t, s, h)
	w := localRequest(s, h, "POST", "/api/local/codex-api/clients", map[string]any{"name": "policy fixture", "accountId": "official-" + id, "disabled": true, "modelPrefix": "team-a/", "modelAllowlist": []string{"allowed", "denied"}, "modelDenylist": []string{"denied"}})
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	client := s.d.Store.Get().CodexAPI.Clients[0]
	if !client.Disabled || client.ModelPrefix != "team-a/" || len(client.ModelAllowlist) != 2 || len(client.ModelDenylist) != 1 {
		t.Fatal("client policy was not saved")
	}
	w = localRequest(s, h, "GET", "/api/local/codex-api", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"disabled":true`) || !strings.Contains(w.Body.String(), `"modelPrefix":"team-a/"`) || !strings.Contains(w.Body.String(), `"modelDenylist":["denied"]`) {
		t.Fatal("client policy missing from status")
	}
	assertNoOfficialSecrets(t, w.Body.String())
	w = localRequest(s, h, "POST", "/api/local/codex-api/clients", map[string]any{"id": client.ID, "name": "invalid", "accountId": "official-" + id, "modelAllowlist": []string{"duplicate", "duplicate"}})
	if w.Code != 400 || !s.d.Store.Get().CodexAPI.Clients[0].Disabled {
		t.Fatal("invalid update replaced client policy")
	}
	w = localRequest(s, h, "POST", "/api/local/codex-api/clients", map[string]any{"id": client.ID, "name": "invalid prefix", "accountId": "official-" + id, "disabled": true, "modelPrefix": "missing-slash"})
	if w.Code != 400 || s.d.Store.Get().CodexAPI.Clients[0].ModelPrefix != "team-a/" {
		t.Fatal("invalid model namespace replaced client policy")
	}
}

func TestCodexAPICardMembershipCASAndPrimaryGuard(t *testing.T) {
	s, h := officialConsole(t)
	id := importOfficialConsole(t, s, h)
	secondary := "secondary-api"
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.LocalAccounts = append(c.LocalAccounts, config.LocalAccount{ID: secondary, Name: "Secondary", Kind: "api", SourceType: "api_key", BaseURL: "https://provider.fixture", Key: "synthetic-secondary", Model: "gpt-fixture", Wire: "responses"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w := localRequest(s, h, "POST", "/api/local/codex-api/clients", map[string]any{"name": "membership", "accountId": "official-" + id, "accountIds": []string{"official-" + id}, "imageAccountIds": []string{"official-" + id}})
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	var created struct{ ID string }
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatal("missing client id")
	}
	w = localRequest(s, h, "POST", "/api/local/codex-api/members", map[string]any{"clientId": created.ID, "accountId": secondary, "mode": "both", "enabled": true})
	if w.Code != 200 {
		t.Fatalf("add member: %d %s", w.Code, w.Body)
	}
	client, _ := s.d.Store.Get().APIClient(created.ID)
	if !containsString(client.AccountIDs, secondary) || !containsString(client.ImageAccountIDs, secondary) {
		t.Fatalf("member not added: %+v", client)
	}
	w = localRequest(s, h, "POST", "/api/local/codex-api/members", map[string]any{"clientId": created.ID, "accountId": secondary, "mode": "both", "enabled": false})
	if w.Code != 200 {
		t.Fatalf("remove member: %d %s", w.Code, w.Body)
	}
	client, _ = s.d.Store.Get().APIClient(created.ID)
	if containsString(client.AccountIDs, secondary) || containsString(client.ImageAccountIDs, secondary) {
		t.Fatal("member still present after removal")
	}
	// Deleting a local account must also fence any stale named-client member
	// references; otherwise the old API key would retain an unusable account ID.
	if w = localRequest(s, h, "POST", "/api/local/codex-api/members", map[string]any{"clientId": created.ID, "accountId": secondary, "mode": "both", "enabled": true}); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w = localRequest(s, h, "POST", "/api/local/accounts/delete", map[string]string{"id": secondary}); w.Code != 200 {
		t.Fatal(w.Body)
	}
	client, _ = s.d.Store.Get().APIClient(created.ID)
	if containsString(client.AccountIDs, secondary) || containsString(client.ImageAccountIDs, secondary) {
		t.Fatal("deleted account remained in API client membership")
	}
	w = localRequest(s, h, "POST", "/api/local/codex-api/members", map[string]any{"clientId": created.ID, "accountId": "official-" + id, "mode": "both", "enabled": false})
	if w.Code != http.StatusConflict {
		t.Fatalf("primary removal was not guarded: %d %s", w.Code, w.Body)
	}
	status := localRequest(s, h, "GET", "/api/local/accounts", nil)
	if status.Code != 200 || !strings.Contains(status.Body.String(), `"apiService"`) {
		t.Fatal("account card DTO omitted API service membership")
	}
}

func TestOfficialUsageResetIsLocalOnly(t *testing.T) {
	fetcher := codexaccounts.UsageFetchFunc(func(context.Context, *coreauth.Auth) (codexaccounts.UsageFetchResult, error) {
		used, remaining := 10.0, 90.0
		return codexaccounts.UsageFetchResult{ProviderAccountID: "fixture-provider-id", Snapshot: codexaccounts.UsageSnapshot{Plan: codexaccounts.UsagePlan{Raw: "pro"}, RateLimit: &codexaccounts.UsageLimit{ID: "codex", Primary: &codexaccounts.UsageWindow{UsedPercent: &used, RemainingPercent: &remaining}}}}, nil
	})
	s, h := officialConsoleWithUsage(t, fetcher)
	id := importOfficialConsole(t, s, h)
	if w := localRequest(s, h, "POST", "/api/local/codex/usage/refresh", map[string]string{"id": id}); w.Code != 200 {
		t.Fatal(w.Body)
	}
	w := localRequest(s, h, "POST", "/api/local/codex/usage/reset", map[string]string{"id": id})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) || !strings.Contains(w.Body.String(), "不会被本地重置") {
		t.Fatalf("reset response: %d %s", w.Code, w.Body)
	}
	row, err := s.d.CodexAccounts.Get(id)
	if err != nil || row.Usage.Verified || row.Usage.Status == codexaccounts.UsageStatusFresh {
		t.Fatalf("usage cache not reset: %+v %v", row.Usage, err)
	}
}

func TestOfficialResetCreditRoutesRequireExplicitConsumeAndAreIdempotent(t *testing.T) {
	calls := 0
	var redeemID string
	consumer := codexaccounts.ResetCreditConsumeFunc(func(_ context.Context, _ *coreauth.Auth, id string) error {
		calls++
		if redeemID != "" && redeemID != id {
			t.Fatalf("redeem id changed: %q -> %q", redeemID, id)
		}
		redeemID = id
		return nil
	})
	s, h := officialConsoleWithUsageAndReset(t, nil, consumer)
	id := importOfficialConsole(t, s, h)
	prepared := localRequest(s, h, "POST", "/api/local/codex/reset-credit/prepare", map[string]string{"id": id})
	if prepared.Code != http.StatusOK || !strings.Contains(prepared.Body.String(), "redeemRequestId") {
		t.Fatalf("prepare: %d %s", prepared.Code, prepared.Body)
	}
	var envelope struct {
		Request codexaccounts.ResetCreditRequest `json:"request"`
	}
	if err := json.Unmarshal(prepared.Body.Bytes(), &envelope); err != nil || envelope.Request.RedeemRequestID == "" {
		t.Fatalf("prepare DTO: %v %s", err, prepared.Body)
	}
	body := map[string]string{"id": id, "redeemRequestId": envelope.Request.RedeemRequestID}
	consumed := localRequest(s, h, "POST", "/api/local/codex/reset-credit/consume", body)
	if consumed.Code != http.StatusOK || !strings.Contains(consumed.Body.String(), `"ok":true`) || calls != 1 {
		t.Fatalf("consume: %d %s calls=%d", consumed.Code, consumed.Body, calls)
	}
	repeated := localRequest(s, h, "POST", "/api/local/codex/reset-credit/consume", body)
	if repeated.Code != http.StatusOK || !strings.Contains(repeated.Body.String(), `"ok":true`) || calls != 1 {
		t.Fatalf("idempotent consume: %d %s calls=%d", repeated.Code, repeated.Body, calls)
	}
	assertNoOfficialSecrets(t, consumed.Body.String())
}
