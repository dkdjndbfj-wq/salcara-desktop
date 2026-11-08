package codexaccounts

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestPoolRoutingUsesOnlyCurrentVerifiedUsageAndCurrentSubscriptionHint(t *testing.T) {
	now := time.Date(2026, 10, 11, 6, 0, 0, 0, time.UTC)
	expires := now.Add(48 * time.Hour)
	fetcher := UsageFetchFunc(func(context.Context, *coreauth.Auth) (UsageFetchResult, error) {
		return UsageFetchResult{ProviderAccountID: "explicit-fixture-account", Snapshot: UsageSnapshot{
			Plan: UsagePlan{Raw: "self_serve_business_usage_based"}, RateLimit: quotaLimit(15, 45),
		}}, nil
	})
	s, err := New(Options{Path: filepath.Join(t.TempDir(), "vault"), Protector: &testProtector{}, UsageFetcher: fetcher, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	withSubscription := strings.TrimSuffix(fixture, "}") + `,"subscription_active_until":"` + expires.Format(time.RFC3339) + `"}`
	prepared, err := ParseImport([]byte(withSubscription))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.Import(context.Background(), prepared, "")
	if err != nil {
		t.Fatal(err)
	}
	account := rows[0]

	before := s.PoolRouting(account.ID, account.IdentityEpoch, "gpt-6-sol")
	if !before.Current || before.QuotaFresh || before.PlanFresh || !before.SubscriptionKnown || before.SubscriptionActiveUntil == nil || !before.SubscriptionActiveUntil.Equal(expires) {
		t.Fatalf("unqueried hints/usage were confused: %+v", before)
	}
	if _, err = s.RefreshUsage(context.Background(), account.ID); err != nil {
		t.Fatal(err)
	}
	got := s.PoolRouting(account.ID, account.IdentityEpoch, "gpt-6-sol")
	if !got.Current || !got.QuotaFresh || !got.QuotaAllowed || got.RemainingPercent != 55 || !got.PlanFresh || got.PlanFamily != "business" || !got.SubscriptionKnown {
		t.Fatalf("current routing observation incomplete: %+v", got)
	}
	if stale := s.PoolRouting(account.ID, account.IdentityEpoch+1, "gpt-6-sol"); stale.Current || stale.QuotaFresh || stale.PlanFresh || stale.SubscriptionKnown {
		t.Fatalf("wrong identity observed routing metadata: %+v", stale)
	}
}

func TestPoolRoutingFailsClosedAfterCredentialRefreshFailure(t *testing.T) {
	now := time.Date(2026, 10, 11, 6, 0, 0, 0, time.UTC)
	expires := now.Add(48 * time.Hour)
	fetcher := UsageFetchFunc(func(context.Context, *coreauth.Auth) (UsageFetchResult, error) {
		return UsageFetchResult{ProviderAccountID: "explicit-fixture-account", Snapshot: UsageSnapshot{
			Plan: UsagePlan{Raw: "pro"}, RateLimit: quotaLimit(15, 45),
		}}, nil
	})
	refresher := RefreshFunc(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
		return nil, errors.New("synthetic transient refresh failure")
	})
	s, err := New(Options{Path: filepath.Join(t.TempDir(), "vault"), Protector: &testProtector{}, Refresher: refresher, UsageFetcher: fetcher, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	withSubscription := strings.TrimSuffix(fixture, "}") + `,"subscription_active_until":"` + expires.Format(time.RFC3339) + `"}`
	prepared, err := ParseImport([]byte(withSubscription))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.Import(context.Background(), prepared, "")
	if err != nil {
		t.Fatal(err)
	}
	account := rows[0]
	if _, err = s.RefreshUsage(context.Background(), account.ID); err != nil {
		t.Fatal(err)
	}
	if before := s.PoolRouting(account.ID, account.IdentityEpoch, "gpt-6-sol"); !before.PlanFresh || !before.QuotaFresh {
		t.Fatalf("fixture did not begin with fresh metrics: %+v", before)
	}
	if refreshed, refreshErr := s.Refresh(context.Background(), account.ID); refreshErr != ErrRefreshFailed || refreshed.Status != StatusRefreshFailed {
		t.Fatalf("fixture did not enter refresh_failed: account=%+v err=%v", refreshed, refreshErr)
	}
	got := s.PoolRouting(account.ID, account.IdentityEpoch, "gpt-6-sol")
	if !got.Current || got.PlanFresh || got.QuotaFresh || !got.SubscriptionKnown {
		t.Fatalf("failed credential refresh left verified routing metrics usable: %+v", got)
	}
	if quota := s.PoolQuota(account.ID, account.IdentityEpoch, "gpt-6-sol"); quota.Fresh {
		t.Fatalf("failed credential refresh left reserve quota usable: %+v", quota)
	}
}

func TestPoolRoutingFailsClosedWhileCredentialRotationIsPendingSave(t *testing.T) {
	now := time.Date(2026, 10, 11, 6, 0, 0, 0, time.UTC)
	fetcher := UsageFetchFunc(func(context.Context, *coreauth.Auth) (UsageFetchResult, error) {
		return UsageFetchResult{ProviderAccountID: "explicit-fixture-account", Snapshot: UsageSnapshot{
			Plan: UsagePlan{Raw: "team"}, RateLimit: quotaLimit(15, 45),
		}}, nil
	})
	protector := &testProtector{}
	refresher := RefreshFunc(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
		return sdkFixture("synthetic-rotated-access", "synthetic-rotated-refresh"), nil
	})
	s, err := New(Options{Path: filepath.Join(t.TempDir(), "vault"), Protector: protector, Refresher: refresher, UsageFetcher: fetcher, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	account := importFixture(t, s)
	if _, err = s.RefreshUsage(context.Background(), account.ID); err != nil {
		t.Fatal(err)
	}
	protector.fail.Store(true)
	if refreshed, refreshErr := s.Refresh(context.Background(), account.ID); refreshErr != ErrSaveFailed || refreshed.Status != StatusSaveFailed {
		t.Fatalf("fixture did not retain pending rotation: account=%+v err=%v", refreshed, refreshErr)
	}
	got := s.PoolRouting(account.ID, account.IdentityEpoch, "gpt-6-sol")
	if !got.Current || got.PlanFresh || got.QuotaFresh {
		t.Fatalf("pending unsaved credential left verified routing metrics usable: %+v", got)
	}
	if quota := s.PoolQuota(account.ID, account.IdentityEpoch, "gpt-6-sol"); quota.Fresh {
		t.Fatalf("pending unsaved credential left reserve quota usable: %+v", quota)
	}
}

func TestPoolRoutingFailsClosedForNeedsLoginAndPendingReplacement(t *testing.T) {
	now := time.Date(2026, 10, 11, 6, 0, 0, 0, time.UTC)
	fetcher := UsageFetchFunc(func(context.Context, *coreauth.Auth) (UsageFetchResult, error) {
		return UsageFetchResult{ProviderAccountID: "explicit-fixture-account", Snapshot: UsageSnapshot{
			Plan: UsagePlan{Raw: "plus"}, RateLimit: quotaLimit(15, 45),
		}}, nil
	})
	s, err := New(Options{Path: filepath.Join(t.TempDir(), "vault"), Protector: &testProtector{}, UsageFetcher: fetcher, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	withoutRefresh := strings.Replace(fixture, `,"refresh_token":"synthetic-refresh-old"`, "", 1)
	prepared, err := ParseImport([]byte(withoutRefresh))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.Import(context.Background(), prepared, "")
	if err != nil {
		t.Fatal(err)
	}
	account := rows[0]
	if _, err = s.RefreshUsage(context.Background(), account.ID); err != nil {
		t.Fatal(err)
	}
	if refreshed, refreshErr := s.Refresh(context.Background(), account.ID); refreshErr != ErrNeedsLogin || refreshed.Status != StatusNeedsLogin {
		t.Fatalf("fixture did not enter needs_login: account=%+v err=%v", refreshed, refreshErr)
	}
	if got := s.PoolRouting(account.ID, account.IdentityEpoch, "gpt-6-sol"); !got.Current || got.PlanFresh || got.QuotaFresh {
		t.Fatalf("needs_login credential left verified routing metrics usable: %+v", got)
	}

	// A pending replacement normally fences the cache when it starts. Seed the
	// internal state to ensure the read boundary itself remains fail closed if a
	// future mutation publishes a cache before that fence.
	s.mu.Lock()
	c := s.vault.Accounts[account.ID]
	c.Status = StatusReady
	s.vault.Accounts[account.ID] = c
	s.logins["synthetic-pending-replacement"] = &loginState{
		session: LoginSession{ID: "synthetic-pending-replacement", Status: "pending"},
		request: LoginRequest{ReplaceID: account.ID},
		cancel:  func() {},
	}
	s.mu.Unlock()
	if got := s.PoolRouting(account.ID, account.IdentityEpoch, "gpt-6-sol"); !got.Current || got.PlanFresh || got.QuotaFresh {
		t.Fatalf("pending replacement left verified routing metrics usable: %+v", got)
	}
}
