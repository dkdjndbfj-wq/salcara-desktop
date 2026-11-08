package codexaccounts

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestUsageFetchIntervalCoversSlowResponseAndOwnsItsTimestamps(t *testing.T) {
	base := time.Date(2026, 10, 13, 1, 0, 0, 0, time.UTC)
	var seconds atomic.Int64
	var fail atomic.Bool
	entered, release := make(chan struct{}), make(chan struct{})
	fetcher := UsageFetchFunc(func(context.Context, *coreauth.Auth) (UsageFetchResult, error) {
		if fail.Load() {
			return UsageFetchResult{}, ErrUsageUnavailable
		}
		close(entered)
		<-release
		return UsageFetchResult{ProviderAccountID: "explicit-fixture-account", Snapshot: UsageSnapshot{
			Plan: UsagePlan{Raw: "plus"}, RateLimit: quotaLimit(10, 20),
			FetchStartedAt: timePtr(base.Add(time.Hour)), ObservedAt: timePtr(base.Add(2 * time.Hour)),
		}}, nil
	})
	s, err := New(Options{Path: filepath.Join(t.TempDir(), "vault"), Protector: &testProtector{}, UsageFetcher: fetcher, Now: func() time.Time { return base.Add(time.Duration(seconds.Load()) * time.Second) }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	account := importFixture(t, s)
	done := make(chan UsageSnapshot, 1)
	errs := make(chan error, 1)
	go func() { result, err := s.RefreshUsage(context.Background(), account.ID); done <- result; errs <- err }()
	<-entered
	seconds.Store(30)
	close(release)
	result := <-done
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	assertUsageFetchInterval(t, result, base, base.Add(30*time.Second))
	// Both timestamps are detached at the public cache boundary.
	*result.FetchStartedAt = base.Add(3 * time.Hour)
	*result.ObservedAt = base.Add(4 * time.Hour)
	cached, err := s.Usage(account.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertUsageFetchInterval(t, cached, base, base.Add(30*time.Second))
	*cached.FetchStartedAt = base.Add(5 * time.Hour)
	listed := s.List()
	assertUsageFetchInterval(t, listed[0].Usage, base, base.Add(30*time.Second))
	// A failed attempt makes the old result stale, not a new observation.
	fail.Store(true)
	seconds.Store(60)
	stale, err := s.RefreshUsage(context.Background(), account.ID)
	if !errors.Is(err, ErrUsageUnavailable) || stale.Status != UsageStatusStale {
		t.Fatalf("failed refresh = %q, %v", stale.Status, err)
	}
	assertUsageFetchInterval(t, stale, base, base.Add(30*time.Second))
}

func TestUsageFetchIntervalIncludesUnauthorizedRotationAndRetry(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	var seconds atomic.Int64
	var calls atomic.Int32
	refresher := RefreshFunc(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
		seconds.Store(10)
		return &coreauth.Auth{Provider: "codex", Metadata: map[string]any{
			"type": "codex", "access_token": "synthetic-access-new", "refresh_token": "synthetic-refresh-new",
			"account_id": "explicit-fixture-account", "email": "fixture@example.test", "expired": "2099-01-01T00:00:00Z",
		}}, nil
	})
	fetcher := UsageFetchFunc(func(context.Context, *coreauth.Auth) (UsageFetchResult, error) {
		if calls.Add(1) == 1 {
			seconds.Store(5)
			return UsageFetchResult{}, ErrNeedsLogin
		}
		seconds.Store(20)
		return UsageFetchResult{ProviderAccountID: "explicit-fixture-account", Snapshot: UsageSnapshot{Plan: UsagePlan{Raw: "pro"}, RateLimit: quotaLimit(10, 20)}}, nil
	})
	s, err := New(Options{Path: filepath.Join(t.TempDir(), "vault"), Protector: &testProtector{}, Refresher: refresher, UsageFetcher: fetcher, Now: func() time.Time { return base.Add(time.Duration(seconds.Load()) * time.Second) }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	account := importFixture(t, s)
	result, err := s.RefreshUsage(context.Background(), account.ID)
	if err != nil || calls.Load() != 2 {
		t.Fatalf("usage retry: calls=%d err=%v", calls.Load(), err)
	}
	assertUsageFetchInterval(t, result, base, base.Add(20*time.Second))
	cached, err := s.Usage(account.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertUsageFetchInterval(t, cached, base, base.Add(20*time.Second))
}

func TestUsageFetchIntervalRetryJoinWidensSharedObservation(t *testing.T) {
	base := time.Date(2026, 10, 13, 3, 0, 0, 0, time.UTC)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	fetcher := UsageFetchFunc(func(context.Context, *coreauth.Auth) (UsageFetchResult, error) {
		calls.Add(1)
		close(entered)
		<-release
		return UsageFetchResult{ProviderAccountID: "explicit-fixture-account", Snapshot: UsageSnapshot{Plan: UsagePlan{Raw: "pro"}, RateLimit: quotaLimit(10, 20)}}, nil
	})
	s, err := New(Options{Path: filepath.Join(t.TempDir(), "vault"), Protector: &testProtector{}, UsageFetcher: fetcher, Now: func() time.Time { return base }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	account := importFixture(t, s)
	done := make(chan UsageSnapshot, 2)
	errs := make(chan error, 2)
	go func() { result, err := s.RefreshUsage(context.Background(), account.ID); done <- result; errs <- err }()
	<-entered
	priorStart := base.Add(-time.Minute)
	go func() {
		result, _, err := s.refreshUsageOnce(context.Background(), account.ID, &usageAttempt{identityEpoch: account.IdentityEpoch, fetchStartedAt: priorStart})
		done <- result
		errs <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	joined := false
	for time.Now().Before(deadline) {
		s.mu.Lock()
		joined = s.usageFlights[account.ID].fetchStartedAt.Equal(priorStart)
		s.mu.Unlock()
		if joined {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	if !joined {
		t.Fatal("retry did not join active observation")
	}
	for range 2 {
		result := <-done
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		assertUsageFetchInterval(t, result, priorStart, base)
	}
	if calls.Load() != 1 {
		t.Fatalf("coalesced observation made %d provider calls", calls.Load())
	}
}

func TestUsageFetchIntervalRetryRejectsReplacementIdentity(t *testing.T) {
	s, err := New(Options{Path: filepath.Join(t.TempDir(), "vault"), Protector: &testProtector{}, UsageFetcher: UsageFetchFunc(func(context.Context, *coreauth.Auth) (UsageFetchResult, error) {
		t.Error("replacement retry reached provider")
		return UsageFetchResult{}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	account := importFixture(t, s)
	_, _, err = s.refreshUsageOnce(context.Background(), account.ID, &usageAttempt{identityEpoch: account.IdentityEpoch + 1, fetchStartedAt: time.Now()})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong identity retry = %v", err)
	}
}

func assertUsageFetchInterval(t *testing.T, snapshot UsageSnapshot, start, end time.Time) {
	t.Helper()
	if snapshot.FetchStartedAt == nil || !snapshot.FetchStartedAt.Equal(start) || snapshot.ObservedAt == nil || !snapshot.ObservedAt.Equal(end) {
		t.Fatalf("local fetch interval = %v .. %v, want %v .. %v", snapshot.FetchStartedAt, snapshot.ObservedAt, start, end)
	}
}
