package console

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"salcara/bridge/internal/codexaccounts"
)

func TestOfficialSubscriptionRefreshHTTPIdentityAndSafeProjection(t *testing.T) {
	var fail atomic.Bool
	var calls atomic.Int32
	expiry := time.Date(2090, 1, 1, 0, 0, 0, 0, time.UTC)
	s, _ := newTestServer(t)
	vault, err := codexaccounts.New(codexaccounts.Options{Path: filepath.Join(t.TempDir(), "subscription.vault"), Protector: consoleProtector{}, SubscriptionFetcher: codexaccounts.SubscriptionFetchFunc(func(context.Context, *coreauth.Auth) (codexaccounts.SubscriptionFetchResult, error) {
		calls.Add(1)
		if fail.Load() {
			return codexaccounts.SubscriptionFetchResult{}, errors.New("private synthetic-access-private fixture-provider-id")
		}
		return codexaccounts.SubscriptionFetchResult{ProviderAccountID: "fixture-provider-id", ActiveUntil: &expiry}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(vault.Close)
	s.d.CodexAccounts = vault
	h := s.Handler()
	id := importOfficialConsole(t, s, h)
	account, _ := vault.Get(id)
	w := localRequest(s, h, "POST", "/api/local/codex/subscription/refresh", map[string]any{"id": id, "identityEpoch": account.IdentityEpoch})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ok":true`) || !strings.Contains(w.Body.String(), `"verified":true`) || !strings.Contains(w.Body.String(), `"freshness":"fresh"`) || !strings.Contains(w.Body.String(), `"activeUntil":"2090-01-01T00:00:00Z"`) {
		t.Fatalf("subscription response: %d %s", w.Code, w.Body)
	}
	assertNoOfficialSecrets(t, w.Body.String())
	listed := localRequest(s, h, "GET", "/api/local/codex/accounts", nil)
	if !strings.Contains(listed.Body.String(), `"source":"openai_subscription_api"`) || !strings.Contains(listed.Body.String(), `"activeUntil":"2090-01-01T00:00:00Z"`) {
		t.Fatalf("account list omitted observation: %s", listed.Body)
	}
	assertNoOfficialSecrets(t, listed.Body.String())
	for _, body := range []map[string]any{{"id": id}, {"id": id, "identityEpoch": 0}, {"id": id, "identityEpoch": account.IdentityEpoch, "url": "https://example.invalid"}} {
		bad := localRequest(s, h, "POST", "/api/local/codex/subscription/refresh", body)
		if bad.Code != http.StatusBadRequest {
			t.Fatalf("bad request accepted: %d %s", bad.Code, bad.Body)
		}
	}
	stale := localRequest(s, h, "POST", "/api/local/codex/subscription/refresh", map[string]any{"id": id, "identityEpoch": account.IdentityEpoch + 1})
	if !strings.Contains(stale.Body.String(), `"ok":false`) || calls.Load() != 1 {
		t.Fatalf("stale identity sent request: %s calls=%d", stale.Body, calls.Load())
	}
	fail.Store(true)
	w = localRequest(s, h, "POST", "/api/local/codex/subscription/refresh", map[string]any{"id": id, "identityEpoch": account.IdentityEpoch})
	if !strings.Contains(w.Body.String(), `"ok":false`) || !strings.Contains(w.Body.String(), `"status":"unknown"`) || !strings.Contains(w.Body.String(), `"verified":false`) || !strings.Contains(w.Body.String(), `"freshness":"stale"`) || strings.Contains(w.Body.String(), `"remainingSeconds"`) || strings.Contains(w.Body.String(), "private") {
		t.Fatalf("failure retained a claimed lifetime or leaked error: %s", w.Body)
	}
	assertNoOfficialSecrets(t, w.Body.String())
}
