package codexaccounts

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestOfficialUsageFetcherUsesImportedCredentialAndParsesSchema(t *testing.T) {
	observed := time.Date(2026, 10, 11, 4, 5, 6, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/backend-api/wham/usage" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer synthetic-access" {
			t.Errorf("authorization mismatch: %q", got)
		}
		if got := r.Header.Get("ChatGPT-Account-Id"); got != "provider-account" {
			t.Errorf("account header mismatch: %q", got)
		}
		if got := r.Header.Get("originator"); got != "salcara" || !strings.HasPrefix(r.Header.Get("User-Agent"), "salcara-bridge/") {
			t.Errorf("client provenance missing: originator=%q ua=%q", got, r.Header.Get("User-Agent"))
		}
		for _, forbidden := range []string{"Referer", "OpenAI-Beta", "Sec-Fetch-Site"} {
			if r.Header.Get(forbidden) != "" {
				t.Errorf("browser impersonation header present: %s", forbidden)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
          "account_id":"provider-account",
          "plan_type":"self_serve_business_usage_based",
          "rate_limit":{"allowed":true,"limit_reached":false,
            "primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":0,"reset_at":1791680706},
            "secondary_window":{"used_percent":80,"limit_window_seconds":604800,"reset_after_seconds":60,"reset_at":1791680766}},
          "code_review_rate_limit":{"allowed":false,"primary_window":{"used_percent":100,"limit_window_seconds":86400,"reset_at":1791680800}},
          "additional_rate_limits":[{"limit_name":"GPT-6 Reserve","metered_feature":"codex_luna_reserve","normal_model_slug":"gpt-6-luna","rate_limit":{"allowed":true,"primary_window":{"used_percent":25.5,"limit_window_seconds":900,"reset_at":1791680900}}}],
          "credits":{"has_credits":true,"unlimited":false,"balance":"9.99"},
          "spend_control":{"reached":false,"individual_limit":{"source":"workspace","limit":"25000","used":"8000","remaining":"17000","used_percent":32,"remaining_percent":68,"reset_after_seconds":3600,"reset_at":1791684306}},
          "rate_limit_reset_credits":{"available_count":2},
          "rate_limit_reached_type":{"kind":"workspace_member_credits_depleted"}
        }`))
	}))
	defer server.Close()
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	fetcher := &officialUsageFetcher{client: client, endpoint: server.URL + "/backend-api/wham/usage", allowLoopback: true, now: func() time.Time { return observed }}
	result, err := fetcher.FetchUsage(context.Background(), &coreauth.Auth{Provider: "codex", Metadata: map[string]any{"access_token": "synthetic-access", "account_id": "provider-account"}})
	if err != nil {
		t.Fatal(err)
	}
	usage := result.Snapshot
	if result.ProviderAccountID != "provider-account" || usage.Source != UsageSourceOfficial || usage.Plan.Raw != "self_serve_business_usage_based" || usage.Plan.Family != "business" || !usage.Plan.Verified {
		t.Fatalf("provider metadata not normalized: %+v / %q", usage.Plan, result.ProviderAccountID)
	}
	if usage.RateLimit == nil || usage.RateLimit.Primary == nil || usage.RateLimit.Primary.UsedPercent == nil || *usage.RateLimit.Primary.UsedPercent != 0 || *usage.RateLimit.Primary.RemainingPercent != 100 {
		t.Fatalf("zero usage was confused with missing data: %+v", usage.RateLimit)
	}
	if usage.RateLimit.Secondary == nil || *usage.RateLimit.Secondary.RemainingPercent != 20 || usage.RateLimit.Primary.WindowMinutes == nil || *usage.RateLimit.Primary.WindowMinutes != 300 {
		t.Fatalf("quota windows lost: %+v", usage.RateLimit)
	}
	if usage.CodeReviewRateLimit == nil || usage.CodeReviewRateLimit.Allowed == nil || *usage.CodeReviewRateLimit.Allowed || len(usage.AdditionalRateLimits) != 1 || usage.AdditionalRateLimits[0].NormalModelSlug != "gpt-6-luna" {
		t.Fatalf("named limits lost: code=%+v additional=%+v", usage.CodeReviewRateLimit, usage.AdditionalRateLimits)
	}
	if usage.Credits == nil || usage.Credits.Balance != "9.99" || usage.SpendControl == nil || usage.SpendControl.IndividualLimit == nil || usage.SpendControl.IndividualLimit.Remaining != "17000" || usage.ResetCreditsAvailable == nil || *usage.ResetCreditsAvailable != 2 {
		t.Fatalf("credits/spend controls lost: %+v %+v", usage.Credits, usage.SpendControl)
	}
	if usage.RateLimitReachedType != "workspace_member_credits_depleted" {
		t.Fatalf("reached condition lost: %q", usage.RateLimitReachedType)
	}
	raw, _ := json.Marshal(usage)
	if strings.Contains(string(raw), "synthetic-access") || strings.Contains(string(raw), "provider-account") {
		t.Fatalf("usage DTO leaked routing identity or token: %s", raw)
	}
}

func TestOfficialUsageFetcherRejectsRedirectDuplicateAndOversize(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		header func(http.Header)
	}{
		{name: "redirect", status: http.StatusFound, header: func(h http.Header) { h.Set("Location", "https://example.invalid/capture") }},
		{name: "duplicate", status: http.StatusOK, body: `{"plan_type":"pro","plan_type":"team"}`},
		{name: "oversize", status: http.StatusOK, body: `{"plan_type":"` + strings.Repeat("x", maxUsageBodyBytes) + `"}`},
		{name: "empty", status: http.StatusOK, body: `{}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.header != nil {
					tc.header(w.Header())
				}
				if tc.status == http.StatusOK {
					w.Header().Set("Content-Type", "application/json")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client := server.Client()
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			fetcher := &officialUsageFetcher{client: client, endpoint: server.URL + "/usage", allowLoopback: true}
			_, err := fetcher.FetchUsage(context.Background(), &coreauth.Auth{Provider: "codex", Metadata: map[string]any{"access_token": "synthetic"}})
			if !errors.Is(err, ErrUsageUnavailable) {
				t.Fatalf("unsafe response accepted: %v", err)
			}
			if tc.body != "" && strings.Contains(err.Error(), tc.body) {
				t.Fatal("raw provider body entered error")
			}
		})
	}
}

func TestUsagePlanClassificationUsesVerifiedKnownFamilies(t *testing.T) {
	cases := map[string]string{
		"free": "free", "go": "go", "plus": "plus", "pro": "pro", "prolite": "pro", "promax": "pro", "team": "team",
		"self_serve_business_prolite": "business", "business": "business", "ent26": "enterprise", "enterprise_cbp_usage_based": "enterprise",
		"enterprise": "enterprise", "hc": "enterprise", "education": "edu", "edu": "edu", "edu_plus": "edu", "mystery_safe_tier": "unknown",
	}
	for raw, family := range cases {
		gotRaw, gotFamily := normalizePlan(raw)
		if gotRaw != raw || gotFamily != family {
			t.Errorf("normalizePlan(%q)=(%q,%q), want (%q,%q)", raw, gotRaw, gotFamily, raw, family)
		}
	}
	if raw, family := normalizePlan("unknown tier\nprovider-secret"); raw != "" || family != "unknown" {
		t.Fatalf("unsafe raw plan exposed: %q %q", raw, family)
	}
}

func TestUsageServiceFreshnessPoolQuotaAndFailureState(t *testing.T) {
	now := time.Date(2026, 10, 11, 5, 0, 0, 0, time.UTC)
	var fail atomic.Bool
	fetcher := UsageFetchFunc(func(context.Context, *coreauth.Auth) (UsageFetchResult, error) {
		if fail.Load() {
			return UsageFetchResult{}, usageError("upstream_status", errors.New("private synthetic-access response body"))
		}
		parsed, err := parseOfficialUsage([]byte(`{"account_id":"explicit-fixture-account","plan_type":"team","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":25,"limit_window_seconds":18000,"reset_at":1893456000},"secondary_window":{"used_percent":80,"limit_window_seconds":604800,"reset_at":1893456000}}}`), now)
		return parsed, err
	})
	s, err := New(Options{Path: filepath.Join(t.TempDir(), "vault"), Protector: &testProtector{}, UsageFetcher: fetcher, Now: func() time.Time { return now }, UsageFreshFor: 2 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	account := importFixture(t, s)
	if account.Usage.Status != UsageStatusUnknown || account.Usage.Plan.Verified {
		t.Fatalf("unqueried JWT/import hint marked verified: %+v", account.Usage)
	}
	usage, err := s.RefreshUsage(context.Background(), account.ID)
	if err != nil || usage.Status != UsageStatusFresh || usage.Plan.Family != "team" || !usage.Verified {
		t.Fatalf("usage refresh: %+v %v", usage, err)
	}
	quota := s.PoolQuota(account.ID, account.IdentityEpoch, "")
	if !quota.Fresh || !quota.Allowed || quota.RemainingPercent != 20 {
		t.Fatalf("pool quota did not use restrictive window: %+v", quota)
	}
	if bad := s.PoolQuota(account.ID, account.IdentityEpoch+1, ""); bad.Fresh {
		t.Fatalf("identity epoch bypassed: %+v", bad)
	}

	fail.Store(true)
	failed, err := s.RefreshUsage(context.Background(), account.ID)
	if !errors.Is(err, ErrUsageUnavailable) || failed.Status != UsageStatusStale || failed.ErrorCode != "upstream_status" || !failed.Verified {
		t.Fatalf("failed refresh did not preserve clearly stale observation: %+v %v", failed, err)
	}
	raw, _ := json.Marshal(failed)
	if strings.Contains(string(raw), "private") || strings.Contains(string(raw), "synthetic-access") || s.PoolQuota(account.ID, account.IdentityEpoch, "").Fresh {
		t.Fatalf("failure leaked error or remained routable: %s", raw)
	}

	fail.Store(false)
	if _, err = s.RefreshUsage(context.Background(), account.ID); err != nil {
		t.Fatal(err)
	}
	now = now.Add(3 * time.Minute)
	stale, err := s.Usage(account.ID)
	if err != nil || stale.Status != UsageStatusStale || s.PoolQuota(account.ID, account.IdentityEpoch, "").Fresh {
		t.Fatalf("freshness expiry not enforced: %+v %v", stale, err)
	}
}

func TestUsageRefreshIsFencedByReplacementAndProviderIdentity(t *testing.T) {
	for _, operation := range []string{"replace", "identity_mismatch"} {
		t.Run(operation, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			fetcher := UsageFetchFunc(func(context.Context, *coreauth.Auth) (UsageFetchResult, error) {
				close(entered)
				<-release
				providerID := "explicit-fixture-account"
				if operation == "identity_mismatch" {
					providerID = "another-provider-account"
				}
				return UsageFetchResult{ProviderAccountID: providerID, Snapshot: UsageSnapshot{Plan: UsagePlan{Raw: "pro"}, RateLimit: quotaLimit(10, 20)}}, nil
			})
			s, err := New(Options{Path: filepath.Join(t.TempDir(), "vault"), Protector: &testProtector{}, UsageFetcher: fetcher})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			account := importFixture(t, s)
			done := make(chan error, 1)
			go func() { _, err := s.RefreshUsage(context.Background(), account.ID); done <- err }()
			<-entered
			if operation == "replace" {
				prepared, _ := ParseImport([]byte(strings.Replace(fixture, "synthetic-access-old", "replacement-access", 1)))
				if _, err = s.Import(context.Background(), prepared, account.ID); err != nil {
					t.Fatal(err)
				}
			}
			close(release)
			if err = <-done; !errors.Is(err, ErrConflict) {
				t.Fatalf("stale/wrong-identity usage accepted: %v", err)
			}
			current, err := s.Usage(account.ID)
			if err != nil || current.Verified || current.Status == UsageStatusFresh {
				t.Fatalf("rejected observation published: %+v %v", current, err)
			}
		})
	}
}

func TestUsageRefreshRenewsExpiredCredentialBeforeProviderCall(t *testing.T) {
	now := time.Date(2026, 10, 11, 5, 0, 0, 0, time.UTC)
	var refreshCalls atomic.Int32
	refresher := RefreshFunc(func(_ context.Context, record *coreauth.Auth) (*coreauth.Auth, error) {
		refreshCalls.Add(1)
		if record.Metadata["access_token"] != "synthetic-access-old" {
			t.Fatalf("unexpected refresh input")
		}
		return &coreauth.Auth{Provider: "codex", Metadata: map[string]any{
			"type": "codex", "access_token": "synthetic-access-new", "refresh_token": "synthetic-refresh-new",
			"account_id": "explicit-fixture-account", "email": "fixture@example.test", "expired": "2099-01-01T00:00:00Z",
		}}, nil
	})
	fetcher := UsageFetchFunc(func(_ context.Context, record *coreauth.Auth) (UsageFetchResult, error) {
		if got := record.Metadata["access_token"]; got != "synthetic-access-new" {
			return UsageFetchResult{}, errors.New("usage called with expired credential")
		}
		return UsageFetchResult{ProviderAccountID: "explicit-fixture-account", Snapshot: UsageSnapshot{Plan: UsagePlan{Raw: "plus"}, RateLimit: quotaLimit(10, 20)}}, nil
	})
	s, err := New(Options{Path: filepath.Join(t.TempDir(), "vault"), Protector: &testProtector{}, Refresher: refresher, UsageFetcher: fetcher, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	prepared, err := ParseImport([]byte(strings.Replace(fixture, "2099-01-01T00:00:00Z", "2000-01-01T00:00:00Z", 1)))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.Import(context.Background(), prepared, "")
	if err != nil {
		t.Fatal(err)
	}
	usage, err := s.RefreshUsage(context.Background(), rows[0].ID)
	if err != nil || usage.Status != UsageStatusFresh || refreshCalls.Load() != 1 {
		t.Fatalf("expired credential was not refreshed before usage: usage=%+v err=%v refreshes=%d", usage, err, refreshCalls.Load())
	}
}

func TestUsageRefreshRetriesUnauthorizedAfterCredentialRotation(t *testing.T) {
	var refreshCalls, usageCalls atomic.Int32
	refresher := RefreshFunc(func(_ context.Context, _ *coreauth.Auth) (*coreauth.Auth, error) {
		refreshCalls.Add(1)
		return &coreauth.Auth{Provider: "codex", Metadata: map[string]any{
			"type": "codex", "access_token": "synthetic-access-new", "refresh_token": "synthetic-refresh-new",
			"account_id": "explicit-fixture-account", "email": "fixture@example.test", "expired": "2099-01-01T00:00:00Z",
		}}, nil
	})
	fetcher := UsageFetchFunc(func(_ context.Context, record *coreauth.Auth) (UsageFetchResult, error) {
		usageCalls.Add(1)
		if record.Metadata["access_token"] == "synthetic-access-old" {
			return UsageFetchResult{}, usageError("needs_login", ErrNeedsLogin)
		}
		return UsageFetchResult{ProviderAccountID: "explicit-fixture-account", Snapshot: UsageSnapshot{Plan: UsagePlan{Raw: "pro"}, RateLimit: quotaLimit(10, 20)}}, nil
	})
	s, err := New(Options{Path: filepath.Join(t.TempDir(), "vault"), Protector: &testProtector{}, Refresher: refresher, UsageFetcher: fetcher})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	account := importFixture(t, s)
	usage, err := s.RefreshUsage(context.Background(), account.ID)
	if err != nil || usage.Status != UsageStatusFresh || refreshCalls.Load() != 1 || usageCalls.Load() != 2 {
		t.Fatalf("401 was not retried through credential owner: usage=%+v err=%v refreshes=%d usageCalls=%d", usage, err, refreshCalls.Load(), usageCalls.Load())
	}
}

func TestUsageFreshnessStopsAtProviderWindowReset(t *testing.T) {
	now := time.Date(2026, 10, 11, 5, 0, 0, 0, time.UTC)
	reset := now.Add(time.Minute)
	fetcher := UsageFetchFunc(func(context.Context, *coreauth.Auth) (UsageFetchResult, error) {
		limit := quotaLimit(10, 20)
		limit.Primary.ResetsAt = &reset
		return UsageFetchResult{ProviderAccountID: "explicit-fixture-account", Snapshot: UsageSnapshot{Plan: UsagePlan{Raw: "team"}, RateLimit: limit}}, nil
	})
	s, err := New(Options{Path: filepath.Join(t.TempDir(), "vault"), Protector: &testProtector{}, UsageFetcher: fetcher, Now: func() time.Time { return now }, UsageFreshFor: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	account := importFixture(t, s)
	usage, err := s.RefreshUsage(context.Background(), account.ID)
	if err != nil || usage.FreshUntil == nil || !usage.FreshUntil.Equal(reset) {
		t.Fatalf("provider reset did not cap freshness: %+v %v", usage, err)
	}
	now = reset
	if quota := s.PoolQuota(account.ID, account.IdentityEpoch, ""); quota.Fresh {
		t.Fatalf("pre-reset percentages survived reset boundary: %+v", quota)
	}
}

func TestPoolQuotaRejectsReachedSpendControl(t *testing.T) {
	reached := true
	fetcher := UsageFetchFunc(func(context.Context, *coreauth.Auth) (UsageFetchResult, error) {
		return UsageFetchResult{ProviderAccountID: "explicit-fixture-account", Snapshot: UsageSnapshot{
			Plan: UsagePlan{Raw: "team"}, RateLimit: quotaLimit(10, 20), SpendControl: &UsageSpendControl{Reached: &reached},
		}}, nil
	})
	s, err := New(Options{Path: filepath.Join(t.TempDir(), "vault"), Protector: &testProtector{}, UsageFetcher: fetcher})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	account := importFixture(t, s)
	if _, err = s.RefreshUsage(context.Background(), account.ID); err != nil {
		t.Fatal(err)
	}
	if quota := s.PoolQuota(account.ID, account.IdentityEpoch, ""); !quota.Fresh || quota.Allowed {
		t.Fatalf("reached spend control remained routable: %+v", quota)
	}
}

func TestPoolQuotaConstrainsOnlyExactModelLimit(t *testing.T) {
	truncated := false
	fetcher := UsageFetchFunc(func(context.Context, *coreauth.Auth) (UsageFetchResult, error) {
		return UsageFetchResult{ProviderAccountID: "explicit-fixture-account", Snapshot: UsageSnapshot{
			Plan:                      UsagePlan{Raw: "team"},
			RateLimit:                 quotaLimit(10, 20),
			AdditionalLimitsTruncated: truncated,
			AdditionalRateLimits: []UsageLimit{
				{ID: "spark", NormalModelSlug: "gpt-6-luna", Primary: quotaLimit(95, 0).Primary},
			},
		}}, nil
	})
	s, err := New(Options{Path: filepath.Join(t.TempDir(), "vault"), Protector: &testProtector{}, UsageFetcher: fetcher})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	account := importFixture(t, s)
	if _, err = s.RefreshUsage(context.Background(), account.ID); err != nil {
		t.Fatal(err)
	}
	if quota := s.PoolQuota(account.ID, account.IdentityEpoch, "gpt-6-luna"); !quota.Fresh || !quota.Allowed || quota.RemainingPercent != 5 {
		t.Fatalf("exact model quota was ignored: %+v", quota)
	}
	if quota := s.PoolQuota(account.ID, account.IdentityEpoch, "gpt-6-sol"); !quota.Fresh || !quota.Allowed || quota.RemainingPercent != 80 {
		t.Fatalf("unrelated model quota was guessed: %+v", quota)
	}
	truncated = true
	if _, err = s.RefreshUsage(context.Background(), account.ID); err != nil {
		t.Fatal(err)
	}
	if quota := s.PoolQuota(account.ID, account.IdentityEpoch, "gpt-6-sol"); quota.Fresh {
		t.Fatalf("truncated model limits were treated as complete: %+v", quota)
	}
}

func quotaLimit(primaryUsed, secondaryUsed float64) *UsageLimit {
	primaryRemaining, secondaryRemaining := 100-primaryUsed, 100-secondaryUsed
	return &UsageLimit{ID: "codex", Primary: &UsageWindow{UsedPercent: &primaryUsed, RemainingPercent: &primaryRemaining}, Secondary: &UsageWindow{UsedPercent: &secondaryUsed, RemainingPercent: &secondaryRemaining}}
}
