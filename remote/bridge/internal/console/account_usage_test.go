package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/config"
)

func accountUsageTestServer(t *testing.T, count int) (*Server, http.Handler) {
	t.Helper()
	s, h := newTestServer(t)
	s.d.Store.SetRuntimeGatewayKey("synthetic-runtime-not-a-real-key")
	if err := s.d.Store.Update(func(c *config.Config) error {
		for i := range count {
			c.LocalAccounts = append(c.LocalAccounts, config.LocalAccount{ID: fmt.Sprint("card-", i), Name: "same display name", Kind: "api", SourceType: "api_key", BaseURL: "https://same.example.test/v1", Key: fmt.Sprint("synthetic-key-", i)})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return s, h
}
func accountUsageRequest(s *Server, h http.Handler, id string) *httptest.ResponseRecorder {
	return do(h, http.MethodGet, "/api/local/accounts/usage?id="+id, "127.0.0.1:47831", map[string]string{"Cookie": CookieName + "=" + s.Token()}, "")
}
func accountUsageDecode(t *testing.T, w *httptest.ResponseRecorder) accountUsageDTO {
	t.Helper()
	var data accountUsageDTO
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &data) != nil {
		t.Fatalf("usage response = %d %s", w.Code, w.Body.String())
	}
	return data
}

func TestAccountUsageExactCardCredentialAndTypedResponse(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/sub2api/billing" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		calls.Add(1)
		if r.URL.Path != "/v1/usage" || r.URL.Query().Get("days") != "1" {
			t.Error("wrong usage adapter route")
		}
		balance := 10
		if r.Header.Get("Authorization") == "Bearer synthetic-key-1" {
			balance = 20
		} else if r.Header.Get("Authorization") != "Bearer synthetic-key-0" {
			t.Error("wrong exact card credential")
		}
		_, _ = fmt.Fprintf(w, `{"balance":%d,"unit":"USD","usage":{"today":{"requests":0,"total_tokens":42}},"secret":"private synthetic-key-0"}`, balance)
	}))
	defer upstream.Close()
	s, h := accountUsageTestServer(t, 2)
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.RelayRoot, c.AccountKey = upstream.URL, "different-relay-key"
		for i := range c.LocalAccounts {
			c.LocalAccounts[i].BaseURL = upstream.URL + "/v1"
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		w := accountUsageRequest(s, h, fmt.Sprint("card-", i))
		data := accountUsageDecode(t, w)
		if data.Balance == nil || *data.Balance.Amount != float64((i+1)*10) || data.Today == nil || *data.Today.Requests != 0 || *data.Today.Tokens != 42 || data.Today.Basis != "provider_today" || data.SourceRevision == "" {
			t.Fatalf("wrong per-card display: %+v", data)
		}
		if strings.Contains(w.Body.String(), "synthetic") || strings.Contains(w.Body.String(), "secret") || data.FreshUntil <= data.ObservedAt || data.FreshUntil-data.ObservedAt > 60000 {
			t.Fatal("unsafe or unbounded card DTO")
		}
		accountUsageDecode(t, accountUsageRequest(s, h, fmt.Sprint("card-", i)))
	}
	if calls.Load() != 2 {
		t.Fatalf("same host mixed keys or ignored cache: %d calls", calls.Load())
	}
}

func TestAccountUsageSingleflightAndConcurrencyBound(t *testing.T) {
	s, h := accountUsageTestServer(t, 9)
	entered, release := make(chan struct{}, 9), make(chan struct{})
	var active, maximum, calls atomic.Int32
	s.accountUsage.fetch = func(ctx context.Context, _, _ string, _ int) (map[string]any, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			previous := maximum.Load()
			if current <= previous || maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		calls.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return map[string]any{"balance": float64(1), "unit": "USD"}, nil
	}
	var wg sync.WaitGroup
	errs := make(chan int, 24)
	for range 16 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- accountUsageRequest(s, h, "card-0").Code }()
	}
	for i := 1; i < 9; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); errs <- accountUsageRequest(s, h, fmt.Sprint("card-", i)).Code }(i)
	}
	for range 4 {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("provider slots did not start")
		}
	}
	close(release)
	wg.Wait()
	close(errs)
	for code := range errs {
		if code != http.StatusOK {
			t.Fatalf("coalesced response = %d", code)
		}
	}
	if maximum.Load() > 4 || calls.Load() != 9 {
		t.Fatalf("concurrency=%d calls=%d", maximum.Load(), calls.Load())
	}
}

func TestAccountUsageCacheRetainsNoCredentialAndFetchIsBounded(t *testing.T) {
	// All retained target state must be safe routing/version metadata. Raw
	// credentials are obtained from a current config snapshot only at dispatch.
	typ := reflect.TypeOf(accountUsageTarget{})
	allowed := map[string]bool{"id": true, "revision": true, "sourceEpoch": true, "root": true, "day": true}
	for i := range typ.NumField() {
		if !allowed[typ.Field(i).Name] || typ.Field(i).Type.Kind() != reflect.String {
			t.Fatalf("unexpected retained usage target field: %s", typ.Field(i).Name)
		}
	}
	s, h := accountUsageTestServer(t, 1)
	s.accountUsage.fetch = func(ctx context.Context, _, key string, _ int) (map[string]any, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 8*time.Second || key != "synthetic-key-0" {
			t.Error("provider dispatch missing bounded context or current credential")
		}
		return nil, nil
	}
	accountUsageDecode(t, accountUsageRequest(s, h, "card-0"))
	if strings.Contains(fmt.Sprintf("%+v", s.accountUsage.entries["card-0"].target), "synthetic-key") {
		t.Fatal("retained credential in usage cache")
	}
}

func TestAccountUsageChangedDeletedOrProtectedAccountFencesFlight(t *testing.T) {
	for _, operation := range []string{"edit", "delete", "official", "aba"} {
		t.Run(operation, func(t *testing.T) {
			s, h := accountUsageTestServer(t, 1)
			entered, release := make(chan struct{}), make(chan struct{})
			s.accountUsage.fetch = func(context.Context, string, string, int) (map[string]any, error) {
				close(entered)
				<-release
				return map[string]any{"balance": float64(999), "unit": "USD"}, nil
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- accountUsageRequest(s, h, "card-0") }()
			<-entered
			if err := s.d.Store.Update(func(c *config.Config) error {
				switch operation {
				case "edit", "aba":
					c.LocalAccounts[0].Key = "synthetic-replacement"
				case "delete":
					c.LocalAccounts = nil
				case "official":
					c.LocalAccounts[0].SourceType = "official_account"
					c.LocalAccounts[0].CredentialRef = "synthetic-vault-ref"
					c.LocalAccounts[0].CredentialEpoch = 1
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if operation == "aba" {
				if err := s.d.Store.Update(func(c *config.Config) error { c.LocalAccounts[0].Key = "synthetic-key-0"; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			close(release)
			w := <-done
			if w.Code != http.StatusConflict || strings.Contains(w.Body.String(), "999") {
				t.Fatalf("retired observation published: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestAccountUsageDayRolloverAndExplicitExpiry(t *testing.T) {
	s, h := accountUsageTestServer(t, 1)
	base := time.Date(2026, 10, 13, 23, 59, 50, 0, time.Local)
	var stamp atomic.Int64
	stamp.Store(base.UnixNano())
	s.accountUsage.now = func() time.Time { return time.Unix(0, stamp.Load()).In(time.Local) }
	var calls atomic.Int32
	s.accountUsage.fetch = func(context.Context, string, string, int) (map[string]any, error) {
		calls.Add(1)
		return map[string]any{"today": map[string]any{"requests": float64(1)}}, nil
	}
	first := accountUsageDecode(t, accountUsageRequest(s, h, "card-0"))
	if first.FreshUntil != time.Date(2026, 10, 14, 0, 0, 0, 0, time.Local).UnixMilli() {
		t.Fatal("cache outlived local day")
	}
	stamp.Store(base.Add(20 * time.Second).UnixNano())
	second := accountUsageDecode(t, accountUsageRequest(s, h, "card-0"))
	if second.Today.Date != "2026-10-14" || calls.Load() != 2 {
		t.Fatal("yesterday was reused")
	}
	stamp.Store(base.Add(81 * time.Second).UnixNano())
	accountUsageDecode(t, accountUsageRequest(s, h, "card-0"))
	if calls.Load() != 3 {
		t.Fatal("expired entry was reused")
	}
}

func TestAccountUsageSafeUnknownAndStrictConsoleContract(t *testing.T) {
	s, h := accountUsageTestServer(t, 1)
	var calls atomic.Int32
	s.accountUsage.fetch = func(context.Context, string, string, int) (map[string]any, error) {
		calls.Add(1)
		return map[string]any{"balance": float64(999), "unit": "USD"}, errors.New("private synthetic-key-0 upstream diagnostic")
	}
	w := accountUsageRequest(s, h, "card-0")
	data := accountUsageDecode(t, w)
	if data.Today != nil || data.Balance != nil || strings.Contains(w.Body.String(), "private") || strings.Contains(w.Body.String(), "synthetic") {
		t.Fatal("failed provider result leaked fields")
	}
	for _, target := range []string{"/api/local/accounts/usage", "/api/local/accounts/usage?id=card-0&id=card-1", "/api/local/accounts/usage?id=card-0&root=https://other.test", "/api/local/accounts/usage?id=card-0&refresh=1"} {
		if out := do(h, http.MethodGet, target, "127.0.0.1:47831", map[string]string{"Cookie": CookieName + "=" + s.Token()}, ""); out.Code != http.StatusBadRequest {
			t.Errorf("invalid query accepted: %s = %d", target, out.Code)
		}
	}
	if out := do(h, http.MethodGet, "/api/local/accounts/usage?id=card-0", "127.0.0.1:47831", nil, ""); out.Code != http.StatusUnauthorized {
		t.Fatal("missing console authentication")
	}
	if out := do(h, http.MethodPost, "/api/local/accounts/usage?id=card-0", "127.0.0.1:47831", map[string]string{"Cookie": CookieName + "=" + s.Token(), "Content-Type": "application/json"}, "{}"); out.Code != http.StatusMethodNotAllowed {
		t.Fatalf("wrong method response = %d", out.Code)
	}
	if calls.Load() != 1 {
		t.Fatal("invalid request reached provider")
	}
}

func TestAccountUsageCacheBoundEvictsCompletedEntries(t *testing.T) {
	s, h := accountUsageTestServer(t, 1)
	s.accountUsage.fetch = func(context.Context, string, string, int) (map[string]any, error) { return nil, nil }
	for i := range accountUsageCacheLimit {
		id := fmt.Sprint("retired-", i)
		s.accountUsage.entries[id] = &accountUsageEntry{target: accountUsageTarget{id: id}, at: time.Now().Add(-time.Hour)}
	}
	accountUsageDecode(t, accountUsageRequest(s, h, "card-0"))
	if len(s.accountUsage.entries) != accountUsageCacheLimit {
		t.Fatal("unbounded per-account cache")
	}
}

func TestAccountUsageNormalizeUnknownZeroCurrencyDateAndScope(t *testing.T) {
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.Local)
	for _, tc := range []struct {
		name, payload  string
		balance, today bool
	}{
		{"undated provider today", `{"unit":"USD","balance":-1.5,"usage":{"today":{"requests":0,"total_tokens":0}}}`, true, true},
		{"exact daily", `{"currency":"EUR","remaining":0,"daily_usage":[{"date":"2026-10-13","requests":2,"tokens":3}]}`, true, true},
		{"unknown units", `{"balance":42,"today":{"requests":0}}`, false, true},
		{"credits are not dollars", `{"unit":"credits","balance":42,"credits":{"balance":9}}`, false, false},
		{"lifetime is not today", `{"unit":"USD","balance":0,"total_tokens":999,"requests":99}`, true, false},
		{"wrong dated today", `{"today":{"date":"2026-10-12","requests":99}}`, false, false},
		{"invalid date is not undated", `{"today":{"date":123,"requests":99}}`, false, false},
		{"empty date is not undated", `{"today":{"date":"","requests":99}}`, false, false},
		{"conflicting date aliases", `{"today":{"date":"2026-10-13","day":"2026-10-12","requests":99}}`, false, false},
		{"duplicate dated row", `{"daily_usage":[{"date":"2026-10-13","tokens":1},{"date":"2026-10-13","tokens":2}]}`, false, false},
		{"provider global balance, not per-key traffic", `{"scope":"global","unit":"USD","balance":3,"today":{"requests":99}}`, true, false},
		{"account wide", `{"usage":{"account_wide":true,"today":{"requests":99}}}`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var raw map[string]any
			if err := json.Unmarshal([]byte(tc.payload), &raw); err != nil {
				t.Fatal(err)
			}
			got := normalizeAccountUsage(raw, "fixture", "revision", now)
			if (got.Balance != nil) != tc.balance || (got.Today != nil) != tc.today {
				t.Fatalf("wrong normalized facts: %+v", got)
			}
			if got.Today != nil && got.Today.Date != "2026-10-13" {
				t.Fatal("DTO day differs from local read day")
			}
		})
	}
	var raw map[string]any
	_ = json.Unmarshal([]byte(`{"today":{"requests":-1,"tokens":9007199254740992}}`), &raw)
	got := normalizeAccountUsage(raw, "fixture", "revision", now)
	if got.Today == nil || got.Today.Requests != nil || got.Today.Tokens != nil {
		t.Fatal("invalid counts manufactured valid numbers")
	}
	_ = json.Unmarshal([]byte(`{"today":{"input_tokens":5}}`), &raw)
	got = normalizeAccountUsage(raw, "fixture", "revision", now)
	if got.Today.Tokens != nil || got.Today.Requests != nil {
		t.Fatal("partial/missing counts manufactured zero or total")
	}
}

func TestAccountUsageProviderBalanceScopeDoesNotBroadenKeyTraffic(t *testing.T) {
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.Local)
	for _, tc := range []struct {
		name, scope    string
		balance, today bool
	}{
		{"no explicit scope retains provider contract", ``, true, true},
		{"key", `"scope":"api_key",`, true, true},
		{"account money not key traffic", `"scope":"account",`, true, false},
		{"account wide money", `"scope":"account-wide",`, true, false},
		{"account wide flag", `"account_wide":true,`, true, false},
		{"user money", `"scope":"user",`, true, false},
		{"global provider balance", `"scope":"global",`, true, false},
		{"site provider balance", `"scope":"site",`, true, false},
		{"all keys balance", `"scope":"all_keys",`, true, false},
		{"all keys flag", `"all_keys":true,`, true, false},
		{"global flag", `"is_global":true,`, true, false},
		{"global stats scope", `"stats_scope":"GLOBAL",`, true, false},
		{"global usage scope", `"usage_scope":"site",`, true, false},
		{"global balance only", `"balance_scope":"global",`, true, true},
		{"scoped balance only", `"balance_scope":"account",`, true, true},
		{"site balance with key traffic", `"scope":"key","balance_scope":"site",`, true, true},
		{"unknown traffic scope", `"scope":"future-scope",`, true, false},
		{"malformed traffic scope", `"scope":null,`, true, false},
		{"malformed traffic flag", `"is_global":"false",`, true, false},
		{"malformed account flag", `"account_wide":"true",`, true, false},
		{"nested usage global does not broaden account money", `"usage":{"scope":"global","today":{"requests":99}},`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, field := range []string{"balance", "remaining"} {
				var raw map[string]any
				payload := `{` + tc.scope + `"unit":"USD","` + field + `":3,"today":{"requests":0,"tokens":0}}`
				if err := json.Unmarshal([]byte(payload), &raw); err != nil {
					t.Fatal(err)
				}
				got := normalizeAccountUsage(raw, "fixture", "revision", now)
				if (got.Balance != nil) != tc.balance || (got.Today != nil) != tc.today {
					t.Fatalf("%s scope: balance=%v today=%v", field, got.Balance != nil, got.Today != nil)
				}
			}
		})
	}
}

func TestAccountUsageSharedProviderBalanceKeepsDifferentBillingAccountsIsolated(t *testing.T) {
	s, h := accountUsageTestServer(t, 3)
	var calls atomic.Int32
	s.accountUsage.fetch = func(_ context.Context, root, key string, days int) (map[string]any, error) {
		calls.Add(1)
		if root != "https://same.example.test" || days != 1 {
			t.Error("unexpected provider query")
		}
		balance := float64(38)
		switch key {
		case "synthetic-key-0", "synthetic-key-1": // Two keys share one billing account.
		case "synthetic-key-2": // Same hostname, different billing account.
			balance = 71
		default:
			t.Error("wrong credential")
		}
		return map[string]any{"scope": "global", "unit": "USD", "balance": balance, "today": map[string]any{"requests": float64(999)}}, nil
	}
	for i, expected := range []float64{38, 38, 71} {
		id := fmt.Sprint("card-", i)
		data := accountUsageDecode(t, accountUsageRequest(s, h, id))
		if data.ID != id || data.Balance == nil || *data.Balance.Amount != expected || data.Today != nil {
			t.Fatalf("provider balance/key traffic mixed for %s: %+v", id, data)
		}
		accountUsageDecode(t, accountUsageRequest(s, h, id))
	}
	if calls.Load() != 3 {
		t.Fatalf("host-sharing or per-card cache broken: calls=%d", calls.Load())
	}
}
