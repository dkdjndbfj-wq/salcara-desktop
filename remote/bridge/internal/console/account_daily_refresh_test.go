package console

import (
	"context"
	"net/http"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"salcara/bridge/internal/codexaccounts"
	"salcara/bridge/internal/config"
)

func TestAccountDailyRefreshRequiresCurrentIdentity(t *testing.T) {
	calls := 0
	fetcher := codexaccounts.UsageFetchFunc(func(context.Context, *coreauth.Auth) (codexaccounts.UsageFetchResult, error) {
		calls++
		return codexaccounts.UsageFetchResult{ProviderAccountID: "fixture-provider-id", Snapshot: codexaccounts.UsageSnapshot{Plan: codexaccounts.UsagePlan{Raw: "plus"}}}, nil
	})
	s, h := officialConsoleWithUsage(t, fetcher)
	id := importOfficialConsole(t, s, h)
	row, _ := s.d.CodexAccounts.Get(id)
	for _, body := range []map[string]any{{"id": id}, {"id": id, "identityEpoch": row.IdentityEpoch + 1}, {"id": id, "identityEpoch": row.IdentityEpoch, "key": "rejected"}} {
		w := localRequest(s, h, "POST", "/api/local/accounts/daily-refresh", body)
		if w.Code != http.StatusBadRequest && w.Code != http.StatusConflict {
			t.Fatalf("accepted stale request: %d", w.Code)
		}
	}
	if calls != 0 {
		t.Fatal("invalid confirmation probed provider")
	}
	w := localRequest(s, h, "POST", "/api/local/accounts/daily-refresh", map[string]any{"id": id, "identityEpoch": row.IdentityEpoch})
	if w.Code != 200 || calls != 1 {
		t.Fatalf("valid refresh: %d calls=%d %s", w.Code, calls, w.Body)
	}
	assertNoOfficialSecrets(t, w.Body.String())
}

func TestAccountDailyUsageNeverReadsReplacedSource(t *testing.T) {
	s, h := accountUsageTestServer(t, 1)
	c := s.d.Store.Get()
	account, _ := c.LocalAccount("card-0")
	revision := c.AccountSourceRevision(account)
	calls := 0
	s.accountUsage.fetch = func(context.Context, string, string, int) (map[string]any, error) {
		calls++
		return map[string]any{"balance": 1, "currency": "USD"}, nil
	}
	if err := s.d.Store.Update(func(c *config.Config) error { c.LocalAccounts[0].Key = "synthetic-replacement"; return nil }); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"id": "card-0", "expectedSourceRevision": revision}
	if w := localRequest(s, h, "POST", "/api/local/accounts/daily-usage", body); w.Code != 409 || calls != 0 {
		t.Fatalf("old selection read replaced credential: %d calls=%d", w.Code, calls)
	}
	c = s.d.Store.Get()
	account, _ = c.LocalAccount("card-0")
	body["expectedSourceRevision"] = c.AccountSourceRevision(account)
	if w := localRequest(s, h, "POST", "/api/local/accounts/daily-usage", body); w.Code != 200 || calls != 1 {
		t.Fatalf("valid guarded usage: %d calls=%d", w.Code, calls)
	}
	delete(body, "expectedSourceRevision")
	if w := localRequest(s, h, "POST", "/api/local/accounts/daily-usage", body); w.Code != 400 || calls != 1 {
		t.Fatal("missing revision downgraded to legacy read")
	}
}
