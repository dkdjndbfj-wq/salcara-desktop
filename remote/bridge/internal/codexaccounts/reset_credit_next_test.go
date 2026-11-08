package codexaccounts

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func nextResetFixture(t *testing.T, consumer ResetCreditConsumer) (*Service, string, *atomic.Int64, Options) {
	t.Helper()
	clock := &atomic.Int64{}
	clock.Store(time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC).Unix())
	opts := Options{Path: filepath.Join(t.TempDir(), "reset.vault"), Protector: &testProtector{},
		Now: func() time.Time { return time.Unix(clock.Load(), 0).UTC() }, ResetCreditConsumer: consumer,
		UsageFetcher: UsageFetchFunc(func(context.Context, *coreauth.Auth) (UsageFetchResult, error) {
			credits := int64(2)
			return UsageFetchResult{ProviderAccountID: "synthetic-provider", Snapshot: UsageSnapshot{ResetCreditsAvailable: &credits}}, nil
		}),
		Refresher: RefreshFunc(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
			return &coreauth.Auth{Provider: "codex", Metadata: map[string]any{"access_token": "rotated-synthetic-access", "account_id": "synthetic-provider"}}, nil
		}),
	}
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	p, err := ParseImport([]byte(`{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","account_id":"synthetic-provider","email":"fixture@example.test"}`))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.Import(context.Background(), p, "")
	if err != nil || len(rows) != 1 {
		t.Fatalf("import: %v %+v", err, rows)
	}
	return s, rows[0].ID, clock, opts
}

func completeFirstReset(t *testing.T, s *Service, id string) ResetCreditRequest {
	t.Helper()
	prepared, err := s.PrepareResetCredit(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.ConsumeResetCredit(context.Background(), id, prepared.RedeemRequestID)
	if err != nil || result.State != resetStateSucceeded {
		t.Fatalf("first reset: %+v %v", result, err)
	}
	return result
}

func refreshAfterReset(t *testing.T, s *Service, id string, clock *atomic.Int64) {
	t.Helper()
	clock.Add(1)
	usage, err := s.RefreshUsage(context.Background(), id)
	if err != nil || !usage.Verified || usage.Status != UsageStatusFresh {
		t.Fatalf("usage: %+v %v", usage, err)
	}
}

func TestResetCreditNextRequiresExplicitPredecessorAndFreshUsage(t *testing.T) {
	var calls atomic.Int32
	s, id, clock, _ := nextResetFixture(t, ResetCreditConsumeFunc(func(context.Context, *coreauth.Auth, string) error {
		calls.Add(1)
		return nil
	}))
	first := completeFirstReset(t, s, id)
	if again, err := s.PrepareResetCredit(context.Background(), id); err != nil || again.RedeemRequestID != first.RedeemRequestID || again.State != resetStateSucceeded {
		t.Fatalf("legacy prepare changed completed operation: %+v %v", again, err)
	}
	if _, err := s.PrepareNextResetCredit(context.Background(), id, first.RedeemRequestID); ResetCreditErrorCode(err) != "usage_refresh_required" {
		t.Fatalf("missing observation authorized next: %v", err)
	}
	refreshAfterReset(t, s, id, clock)
	second, err := s.PrepareNextResetCredit(context.Background(), id, first.RedeemRequestID)
	if err != nil || second.State != resetStatePrepared || second.RedeemRequestID == first.RedeemRequestID || calls.Load() != 1 {
		t.Fatalf("second prepare: %+v %v calls=%d", second, err, calls.Load())
	}
	if _, err = s.ConsumeResetCredit(context.Background(), id, first.RedeemRequestID); !errors.Is(err, ErrConflict) || calls.Load() != 1 {
		t.Fatalf("old handle consumed successor: %v calls=%d", err, calls.Load())
	}
	second, err = s.ConsumeResetCredit(context.Background(), id, second.RedeemRequestID)
	if err != nil || second.State != resetStateSucceeded || calls.Load() != 2 {
		t.Fatalf("second consume: %+v %v calls=%d", second, err, calls.Load())
	}
	if retry, err := s.PrepareNextResetCredit(context.Background(), id, first.RedeemRequestID); err != nil || retry.RedeemRequestID != second.RedeemRequestID || retry.State != resetStateSucceeded {
		t.Fatalf("old predecessor skipped a CAS: %+v %v", retry, err)
	}
	if _, err = s.PrepareNextResetCredit(context.Background(), id, second.RedeemRequestID); ResetCreditErrorCode(err) != "usage_refresh_required" {
		t.Fatalf("pre-second usage authorized third: %v", err)
	}
	refreshAfterReset(t, s, id, clock)
	third, err := s.PrepareNextResetCredit(context.Background(), id, second.RedeemRequestID)
	if err != nil || third.RedeemRequestID == second.RedeemRequestID {
		t.Fatalf("third prepare: %+v %v", third, err)
	}
	if _, err = s.PrepareNextResetCredit(context.Background(), id, first.RedeemRequestID); !errors.Is(err, ErrConflict) || calls.Load() != 2 {
		t.Fatalf("stale predecessor accepted: %v calls=%d", err, calls.Load())
	}
}

func TestResetCreditNextConcurrentPreparationCoalesces(t *testing.T) {
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	s, id, clock, _ := nextResetFixture(t, ResetCreditConsumeFunc(func(context.Context, *coreauth.Auth, string) error {
		if calls.Add(1) == 2 {
			close(started)
			<-release
		}
		return nil
	}))
	first := completeFirstReset(t, s, id)
	refreshAfterReset(t, s, id, clock)
	const workers = 12
	results := make([]ResetCreditRequest, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = s.PrepareNextResetCredit(context.Background(), id, first.RedeemRequestID)
		}(i)
	}
	wg.Wait()
	secondID := results[0].RedeemRequestID
	for i, result := range results {
		if errs[i] != nil || result.RedeemRequestID != secondID || secondID == first.RedeemRequestID {
			t.Fatalf("concurrent next[%d]: %+v %v", i, result, errs[i])
		}
	}
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = s.ConsumeResetCredit(context.Background(), id, secondID)
		}(i)
	}
	<-started
	if _, err := s.PrepareNextResetCredit(context.Background(), id, secondID); !errors.Is(err, ErrResetUnknown) {
		close(release)
		t.Fatalf("in-flight successor could advance: %v", err)
	}
	if current, err := s.PrepareNextResetCredit(context.Background(), id, first.RedeemRequestID); err != nil || current.RedeemRequestID != secondID || current.State != resetStateConsuming {
		close(release)
		t.Fatalf("in-flight retry did not coalesce: %+v %v", current, err)
	}
	close(release)
	wg.Wait()
	for i, result := range results {
		if errs[i] != nil || result.RedeemRequestID != secondID || result.State != resetStateSucceeded {
			t.Fatalf("consume[%d]: %+v %v", i, result, errs[i])
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("duplicate upstream calls: %d", calls.Load())
	}
}

func TestResetCreditNextRejectsIneligibleUsage(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*usageEntry, time.Time)
		code string
	}{
		{"unverified", func(e *usageEntry, _ time.Time) { e.snapshot.Verified = false }, "usage_refresh_required"},
		{"import-hint", func(e *usageEntry, _ time.Time) { e.snapshot.Source = UsageSourceJWTHint }, "usage_refresh_required"},
		{"stale", func(e *usageEntry, _ time.Time) { e.snapshot.Status = UsageStatusStale }, "usage_refresh_required"},
		{"expired", func(e *usageEntry, now time.Time) { e.snapshot.FreshUntil = &now }, "usage_refresh_required"},
		{"old-credential", func(e *usageEntry, _ time.Time) { e.epoch++ }, "usage_refresh_required"},
		{"other-identity", func(e *usageEntry, _ time.Time) { e.identityEpoch++ }, "usage_refresh_required"},
		{"pre-reset-fetch", func(e *usageEntry, now time.Time) { prior := now.Add(-time.Second); e.snapshot.FetchStartedAt = &prior }, "usage_refresh_required"},
		{"future-observation", func(e *usageEntry, now time.Time) { future := now.Add(time.Second); e.snapshot.ObservedAt = &future }, "usage_refresh_required"},
		{"missing-credits", func(e *usageEntry, _ time.Time) { e.snapshot.ResetCreditsAvailable = nil }, "usage_refresh_required"},
		{"no-credits", func(e *usageEntry, _ time.Time) { zero := int64(0); e.snapshot.ResetCreditsAvailable = &zero }, "no_reset_credits"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			s, id, clock, _ := nextResetFixture(t, ResetCreditConsumeFunc(func(context.Context, *coreauth.Auth, string) error { calls.Add(1); return nil }))
			first := completeFirstReset(t, s, id)
			refreshAfterReset(t, s, id, clock)
			s.mu.Lock()
			entry := s.usage[id]
			tc.edit(&entry, s.now())
			s.usage[id] = entry
			s.mu.Unlock()
			if _, err := s.PrepareNextResetCredit(context.Background(), id, first.RedeemRequestID); ResetCreditErrorCode(err) != tc.code {
				t.Fatalf("usage gate: %v code=%s", err, ResetCreditErrorCode(err))
			}
			current, _ := s.ResetCreditStatus(id)
			if current.RedeemRequestID != first.RedeemRequestID || calls.Load() != 1 {
				t.Fatalf("rejection changed redemption: %+v calls=%d", current, calls.Load())
			}
		})
	}
}

func TestResetCreditNextAllowsSavedTokenRotation(t *testing.T) {
	s, id, clock, _ := nextResetFixture(t, ResetCreditConsumeFunc(func(context.Context, *coreauth.Auth, string) error { return nil }))
	first := completeFirstReset(t, s, id)
	before, _ := s.Get(id)
	after, err := s.Refresh(context.Background(), id)
	if err != nil || after.Epoch == before.Epoch || after.IdentityEpoch != before.IdentityEpoch {
		t.Fatalf("rotation fixture: %+v %v", after, err)
	}
	refreshAfterReset(t, s, id, clock)
	if next, err := s.PrepareNextResetCredit(context.Background(), id, first.RedeemRequestID); err != nil || next.RedeemRequestID == first.RedeemRequestID {
		t.Fatalf("current rotated usage rejected: %+v %v", next, err)
	}
}

func TestResetCreditNextRejectsLateObservationStartedBeforeReset(t *testing.T) {
	s, id, clock, _ := nextResetFixture(t, ResetCreditConsumeFunc(func(context.Context, *coreauth.Auth, string) error { return nil }))
	started, release := make(chan struct{}), make(chan struct{})
	s.mu.Lock()
	s.usageFetcher = UsageFetchFunc(func(context.Context, *coreauth.Auth) (UsageFetchResult, error) {
		close(started)
		<-release
		credits := int64(3)
		return UsageFetchResult{ProviderAccountID: "synthetic-provider", Snapshot: UsageSnapshot{ResetCreditsAvailable: &credits}}, nil
	})
	s.mu.Unlock()
	done := make(chan error, 1)
	go func() { _, err := s.RefreshUsage(context.Background(), id); done <- err }()
	<-started
	clock.Add(1)
	first := completeFirstReset(t, s, id)
	clock.Add(1)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	usage, _ := s.Usage(id)
	if !usage.ObservedAt.After(*first.CompletedAt) || usage.FetchStartedAt.After(*first.CompletedAt) {
		t.Fatalf("late fixture: %+v", usage)
	}
	if _, err := s.PrepareNextResetCredit(context.Background(), id, first.RedeemRequestID); ResetCreditErrorCode(err) != "usage_refresh_required" {
		t.Fatalf("pre-reset read authorized next: %v", err)
	}
}

func TestResetCreditLateSuccessAfterDeadlineStaysUnknown(t *testing.T) {
	s, id, _, _ := nextResetFixture(t, ResetCreditConsumeFunc(func(ctx context.Context, _ *coreauth.Auth, _ string) error {
		<-ctx.Done()
		return nil
	}))
	s.mu.Lock()
	s.resetTimeout = time.Millisecond
	s.mu.Unlock()
	first, _ := s.PrepareResetCredit(context.Background(), id)
	result, err := s.ConsumeResetCredit(context.Background(), id, first.RedeemRequestID)
	if !errors.Is(err, context.DeadlineExceeded) || result.State != resetStateUnknown {
		t.Fatalf("late success escaped deadline: %+v %v", result, err)
	}
	if _, err := s.ConsumeResetCredit(context.Background(), id, first.RedeemRequestID); !errors.Is(err, ErrResetUnknown) {
		t.Fatalf("late success became replayable: %v", err)
	}
}

func TestResetCreditNextFailureRetainsSameHandleAndUnknownCannotReplay(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "definite-failure", true: "unknown"}[unknown], func(t *testing.T) {
			var calls atomic.Int32
			s, id, clock, opts := nextResetFixture(t, ResetCreditConsumeFunc(func(context.Context, *coreauth.Auth, string) error {
				if calls.Add(1) == 1 {
					if unknown {
						return ErrResetUnknown
					}
					return resetError("rate_limited", ErrResetUnavailable)
				}
				return nil
			}))
			first, _ := s.PrepareResetCredit(context.Background(), id)
			result, err := s.ConsumeResetCredit(context.Background(), id, first.RedeemRequestID)
			if err == nil || unknown && result.State != resetStateUnknown || !unknown && result.State != resetStateFailed {
				t.Fatalf("first result: %+v %v", result, err)
			}
			refreshAfterReset(t, s, id, clock)
			if _, err := s.PrepareNextResetCredit(context.Background(), id, first.RedeemRequestID); err == nil {
				t.Fatal("unresolved operation advanced")
			}
			s.Close()
			restarted, err := New(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			result, err = restarted.ConsumeResetCredit(context.Background(), id, first.RedeemRequestID)
			if unknown {
				if !errors.Is(err, ErrResetUnknown) || calls.Load() != 1 {
					t.Fatalf("unknown replayed: %+v %v calls=%d", result, err, calls.Load())
				}
			} else if err != nil || result.State != resetStateSucceeded || result.RedeemRequestID != first.RedeemRequestID || calls.Load() != 2 {
				t.Fatalf("same-handle failure retry: %+v %v calls=%d", result, err, calls.Load())
			}
		})
	}
}

func TestResetCreditFinalSaveFailureNeverReplaysConsumingRecord(t *testing.T) {
	var calls atomic.Int32
	var protector *testProtector
	s, id, clock, opts := nextResetFixture(t, ResetCreditConsumeFunc(func(context.Context, *coreauth.Auth, string) error {
		calls.Add(1)
		protector.fail.Store(true)
		return nil
	}))
	protector = opts.Protector.(*testProtector)
	first, _ := s.PrepareResetCredit(context.Background(), id)
	if result, err := s.ConsumeResetCredit(context.Background(), id, first.RedeemRequestID); !errors.Is(err, ErrSaveFailed) || result.State != resetStateConsuming {
		t.Fatalf("failed save: %+v %v", result, err)
	}
	protector.fail.Store(false)
	refreshAfterReset(t, s, id, clock)
	if _, err := s.ConsumeResetCredit(context.Background(), id, first.RedeemRequestID); !errors.Is(err, ErrResetUnknown) {
		t.Fatalf("orphan consuming replay: %v", err)
	}
	if _, err := s.PrepareNextResetCredit(context.Background(), id, first.RedeemRequestID); !errors.Is(err, ErrResetUnknown) {
		t.Fatalf("orphan consuming advance: %v", err)
	}
	s.Close()
	restarted, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	status, err := restarted.ResetCreditStatus(id)
	if err != nil || status.State != resetStateUnknown {
		t.Fatalf("restart state: %+v %v", status, err)
	}
	if _, err := restarted.ConsumeResetCredit(context.Background(), id, first.RedeemRequestID); !errors.Is(err, ErrResetUnknown) || calls.Load() != 1 {
		t.Fatalf("restart replay: %v calls=%d", err, calls.Load())
	}
}

func TestResetCreditNextSurvivesRestartWithoutDuplicatingPreparation(t *testing.T) {
	var calls atomic.Int32
	s, id, clock, opts := nextResetFixture(t, ResetCreditConsumeFunc(func(context.Context, *coreauth.Auth, string) error { calls.Add(1); return nil }))
	first := completeFirstReset(t, s, id)
	refreshAfterReset(t, s, id, clock)
	second, err := s.PrepareNextResetCredit(context.Background(), id, first.RedeemRequestID)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	restarted, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	retry, err := restarted.PrepareNextResetCredit(context.Background(), id, first.RedeemRequestID)
	if err != nil || retry.RedeemRequestID != second.RedeemRequestID || retry.State != resetStatePrepared {
		t.Fatalf("restart predecessor: %+v %v", retry, err)
	}
	result, err := restarted.ConsumeResetCredit(context.Background(), id, retry.RedeemRequestID)
	if err != nil || result.State != resetStateSucceeded || calls.Load() != 2 {
		t.Fatalf("restart successor: %+v %v calls=%d", result, err, calls.Load())
	}
}

func TestResetCreditLateIdentityResultCannotBecomeReplacementOperation(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s, id, _, _ := nextResetFixture(t, ResetCreditConsumeFunc(func(context.Context, *coreauth.Auth, string) error {
		calls.Add(1)
		close(started)
		<-release
		return nil
	}))
	first, _ := s.PrepareResetCredit(context.Background(), id)
	done := make(chan struct{})
	var oldResult ResetCreditRequest
	var oldErr error
	go func() {
		oldResult, oldErr = s.ConsumeResetCredit(context.Background(), id, first.RedeemRequestID)
		close(done)
	}()
	<-started
	p, err := ParseImport([]byte(`{"access_token":"replacement-synthetic-access","account_id":"replacement-provider"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Import(context.Background(), p, id); err != nil {
		t.Fatal(err)
	}
	replacement, err := s.PrepareResetCredit(context.Background(), id)
	if err != nil || replacement.RedeemRequestID == first.RedeemRequestID {
		t.Fatalf("replacement prepare: %+v %v", replacement, err)
	}
	close(release)
	<-done
	if !errors.Is(oldErr, ErrConflict) || oldResult.RedeemRequestID == replacement.RedeemRequestID {
		t.Fatalf("old result adopted replacement: %+v %v", oldResult, oldErr)
	}
	if _, err = s.ConsumeResetCredit(context.Background(), id, first.RedeemRequestID); !errors.Is(err, ErrConflict) {
		t.Fatalf("old consume: %v", err)
	}
	if _, err = s.PrepareNextResetCredit(context.Background(), id, first.RedeemRequestID); !errors.Is(err, ErrConflict) || calls.Load() != 1 {
		t.Fatalf("old next: %v calls=%d", err, calls.Load())
	}
	current, _ := s.ResetCreditStatus(id)
	if current.RedeemRequestID != replacement.RedeemRequestID || current.State != resetStatePrepared {
		t.Fatalf("late result changed replacement: %+v", current)
	}
}
