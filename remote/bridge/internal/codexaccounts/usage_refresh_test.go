package codexaccounts

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type usageRefreshClock struct{ nanos atomic.Int64 }

func newUsageRefreshClock() *usageRefreshClock {
	c := &usageRefreshClock{}
	c.nanos.Store(time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC).UnixNano())
	return c
}
func (c *usageRefreshClock) now() time.Time { return time.Unix(0, c.nanos.Load()).UTC() }
func (c *usageRefreshClock) advance(s *Service, duration time.Duration) {
	c.nanos.Add(int64(duration))
	s.mu.Lock()
	s.wakeUsageRefreshLocked()
	s.mu.Unlock()
}

func usageRefreshTestService(t *testing.T, clock *usageRefreshClock, fetcher UsageFetcher, refresher Refresher) *Service {
	t.Helper()
	s, err := New(Options{Path: filepath.Join(t.TempDir(), "synthetic.vault"), Protector: &testProtector{}, Now: clock.now,
		UsageFetcher: fetcher, Refresher: refresher, UsageFreshFor: time.Minute, UsageTimeout: time.Second, RefreshTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func freshUsageFixture() UsageFetchResult {
	remaining := 75.0
	return UsageFetchResult{Snapshot: UsageSnapshot{Plan: UsagePlan{Raw: "pro"}, RateLimit: &UsageLimit{Primary: &UsageWindow{RemainingPercent: &remaining}}}}
}

func waitUsageCondition(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !condition() {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal(description)
		}
	}
}

func waitUsageCall(t *testing.T, calls <-chan string) string {
	t.Helper()
	select {
	case id := <-calls:
		return id
	case <-time.After(2 * time.Second):
		t.Fatal("background usage did not start")
		return ""
	}
}

func noUsageCall(t *testing.T, calls <-chan string) {
	t.Helper()
	select {
	case id := <-calls:
		t.Fatalf("unexpected background provider call for %q", id)
	case <-time.After(30 * time.Millisecond):
	}
}

func activeUsageTarget(account Account) UsageRefreshTarget {
	return UsageRefreshTarget{ID: account.ID, IdentityEpoch: account.IdentityEpoch}
}

func waitUsageIdle(t *testing.T, s *Service) {
	waitUsageCondition(t, "background usage did not settle", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.usageRefresh == nil || len(s.usageRefresh.jobs) == 0
	})
}

func TestUsageMaintenanceOnlyRefreshesEnabledTargetsAndBeforeExpiry(t *testing.T) {
	clock := newUsageRefreshClock()
	calls := make(chan string, 10)
	s := usageRefreshTestService(t, clock, UsageFetchFunc(func(_ context.Context, a *coreauth.Auth) (UsageFetchResult, error) {
		calls <- a.ID
		return freshUsageFixture(), nil
	}), nil)
	account := importFixture(t, s)
	noUsageCall(t, calls)
	s.SetUsageRefreshTargets([]UsageRefreshTarget{{ID: account.ID, IdentityEpoch: account.IdentityEpoch + 1}})
	noUsageCall(t, calls)
	s.SetUsageRefreshTargets([]UsageRefreshTarget{activeUsageTarget(account), activeUsageTarget(account)})
	waitUsageCall(t, calls)
	waitUsageIdle(t, s)
	if quota := s.PoolQuota(account.ID, account.IdentityEpoch, ""); !quota.Fresh || quota.RemainingPercent != 75 {
		t.Fatalf("missing maintained quota: %+v", quota)
	}
	clock.advance(s, 44*time.Second)
	noUsageCall(t, calls)
	clock.advance(s, 2*time.Second)
	waitUsageCall(t, calls)
	waitUsageIdle(t, s)
	s.SetUsageRefreshTargets(nil)
	clock.advance(s, 10*time.Minute)
	noUsageCall(t, calls)
	if s.PoolQuota(account.ID, account.IdentityEpoch, "").Fresh {
		t.Fatal("stopped maintenance relaxed stale quota admission")
	}
}

func TestUsageMaintenanceUsesProviderResetBoundary(t *testing.T) {
	clock := newUsageRefreshClock()
	calls := make(chan string, 10)
	s := usageRefreshTestService(t, clock, UsageFetchFunc(func(_ context.Context, a *coreauth.Auth) (UsageFetchResult, error) {
		calls <- a.ID
		result := freshUsageFixture()
		reset := clock.now().Add(30 * time.Second)
		result.Snapshot.RateLimit.Primary.ResetsAt = &reset
		return result, nil
	}), nil)
	account := importFixture(t, s)
	s.SetUsageRefreshTargets([]UsageRefreshTarget{activeUsageTarget(account)})
	waitUsageCall(t, calls)
	waitUsageIdle(t, s)
	clock.advance(s, 23*time.Second)
	waitUsageCall(t, calls)
	waitUsageIdle(t, s)
	if !s.PoolQuota(account.ID, account.IdentityEpoch, "").Fresh {
		t.Fatal("quota was not maintained ahead of provider reset")
	}
}

func TestUsageMaintenanceNearAbsoluteResetDoesNotAddMinimumIntervalGap(t *testing.T) {
	clock := newUsageRefreshClock()
	calls := make(chan string, 10)
	absoluteReset := clock.now().Add(5 * time.Second)
	s := usageRefreshTestService(t, clock, UsageFetchFunc(func(_ context.Context, a *coreauth.Auth) (UsageFetchResult, error) {
		calls <- a.ID
		result := freshUsageFixture()
		reset := absoluteReset
		if !clock.now().Before(reset) {
			reset = absoluteReset.Add(time.Hour)
		}
		result.Snapshot.RateLimit.Primary.ResetsAt = &reset
		return result, nil
	}), nil)
	account := importFixture(t, s)
	s.SetUsageRefreshTargets([]UsageRefreshTarget{activeUsageTarget(account)})
	waitUsageCall(t, calls)
	waitUsageIdle(t, s)
	clock.advance(s, 4*time.Second)
	noUsageCall(t, calls)
	clock.advance(s, time.Second)
	waitUsageCall(t, calls)
	waitUsageIdle(t, s)
	if !s.PoolQuota(account.ID, account.IdentityEpoch, "").Fresh {
		t.Fatal("minimum refresh interval left quota stale past a near reset")
	}
}

func TestUsageMaintenanceBackoffAndFailureNeverPermitsStaleQuota(t *testing.T) {
	clock := newUsageRefreshClock()
	calls := make(chan string, 10)
	var failing atomic.Bool
	s := usageRefreshTestService(t, clock, UsageFetchFunc(func(_ context.Context, a *coreauth.Auth) (UsageFetchResult, error) {
		calls <- a.ID
		if failing.Load() {
			return UsageFetchResult{}, errors.New("synthetic provider unavailable")
		}
		return freshUsageFixture(), nil
	}), nil)
	account := importFixture(t, s)
	s.SetUsageRefreshTargets([]UsageRefreshTarget{activeUsageTarget(account)})
	waitUsageCall(t, calls)
	waitUsageIdle(t, s)
	failing.Store(true)
	clock.advance(s, 46*time.Second)
	waitUsageCall(t, calls)
	waitUsageIdle(t, s)
	if s.PoolQuota(account.ID, account.IdentityEpoch, "").Fresh {
		t.Fatal("failed background refresh retained executable quota")
	}
	clock.advance(s, 14*time.Second)
	noUsageCall(t, calls)
	clock.advance(s, time.Second)
	waitUsageCall(t, calls)
	waitUsageIdle(t, s)
	clock.advance(s, 29*time.Second)
	noUsageCall(t, calls)
	failing.Store(false)
	clock.advance(s, time.Second)
	waitUsageCall(t, calls)
	waitUsageIdle(t, s)
	if !s.PoolQuota(account.ID, account.IdentityEpoch, "").Fresh {
		t.Fatal("quota did not recover after backoff")
	}
}

func TestUsageMaintenanceTokenRotationImmediatelyRefillsClearedCache(t *testing.T) {
	clock := newUsageRefreshClock()
	calls := make(chan string, 10)
	release := make(chan struct{})
	s := usageRefreshTestService(t, clock, UsageFetchFunc(func(ctx context.Context, a *coreauth.Auth) (UsageFetchResult, error) {
		token, _ := a.Metadata["access_token"].(string)
		calls <- token
		if token == "synthetic-access-new" {
			select {
			case <-release:
			case <-ctx.Done():
				return UsageFetchResult{}, ctx.Err()
			}
		}
		return freshUsageFixture(), nil
	}), RefreshFunc(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
		return sdkFixture("synthetic-access-new", "synthetic-refresh-new"), nil
	}))
	account := importFixture(t, s)
	s.SetUsageRefreshTargets([]UsageRefreshTarget{activeUsageTarget(account)})
	waitUsageCall(t, calls)
	waitUsageIdle(t, s)
	rotated, err := s.Refresh(context.Background(), account.ID)
	if err != nil || rotated.Epoch == account.Epoch || rotated.IdentityEpoch != account.IdentityEpoch {
		t.Fatalf("normal token rotation failed: account=%+v err=%v", rotated, err)
	}
	if got := waitUsageCall(t, calls); got != "synthetic-access-new" {
		t.Fatalf("background fetch used an old token after rotation: %q", got)
	}
	if s.PoolQuota(account.ID, account.IdentityEpoch, "").Fresh {
		t.Fatal("old quota remained eligible during rotation refill")
	}
	close(release)
	waitUsageIdle(t, s)
	if !s.PoolQuota(account.ID, account.IdentityEpoch, "").Fresh {
		t.Fatal("rotated credential remained blocked until manual quota refresh")
	}
}

func TestUsageMaintenanceRemovalCancelsOnlyBackgroundOwnedFlight(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(map[bool]string{false: "background", true: "manual-joined"}[manual], func(t *testing.T) {
			clock := newUsageRefreshClock()
			started := make(chan context.Context, 1)
			release := make(chan struct{})
			s := usageRefreshTestService(t, clock, UsageFetchFunc(func(ctx context.Context, _ *coreauth.Auth) (UsageFetchResult, error) {
				started <- ctx
				select {
				case <-release:
					return freshUsageFixture(), nil
				case <-ctx.Done():
					return UsageFetchResult{}, ctx.Err()
				}
			}), nil)
			account := importFixture(t, s)
			s.SetUsageRefreshTargets([]UsageRefreshTarget{activeUsageTarget(account)})
			var providerCtx context.Context
			select {
			case providerCtx = <-started:
			case <-time.After(time.Second):
				t.Fatal("background fetch did not start")
			}
			manualDone := make(chan error, 1)
			if manual {
				go func() { _, err := s.RefreshUsage(context.Background(), account.ID); manualDone <- err }()
				waitUsageCondition(t, "manual refresh did not join", func() bool {
					s.mu.Lock()
					defer s.mu.Unlock()
					f := s.usageFlights[account.ID]
					return f != nil && f.background == nil
				})
			}
			s.SetUsageRefreshTargets(nil)
			if manual {
				if providerCtx.Err() != nil {
					t.Fatal("removing target cancelled a user's coalesced refresh")
				}
				close(release)
				select {
				case err := <-manualDone:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("manual refresh did not finish")
				}
			} else {
				select {
				case <-providerCtx.Done():
				case <-time.After(time.Second):
					t.Fatal("removed background quota request was not cancelled")
				}
			}
			waitUsageIdle(t, s)
			if s.PoolQuota(account.ID, account.IdentityEpoch, "").Fresh != manual {
				t.Fatal("wrong publication after background target removal")
			}
		})
	}
}

func TestUsageMaintenanceConcurrencyIncludesCancelledUnwindingWork(t *testing.T) {
	clock := newUsageRefreshClock()
	calls := make(chan string, 10)
	release := make(chan struct{})
	var active, peak atomic.Int64
	s := usageRefreshTestService(t, clock, UsageFetchFunc(func(_ context.Context, a *coreauth.Auth) (UsageFetchResult, error) {
		count := active.Add(1)
		defer active.Add(-1)
		for prior := peak.Load(); count > prior && !peak.CompareAndSwap(prior, count); prior = peak.Load() {
		}
		calls <- a.ID
		<-release // Deliberately ignore cancellation until the transport unwinds.
		return freshUsageFixture(), nil
	}), nil)
	var targets []UsageRefreshTarget
	for _, suffix := range []string{"one", "two", "three", "four"} {
		raw := strings.ReplaceAll(strings.ReplaceAll(fixture, "fixture", "fixture-"+suffix), "synthetic", "synthetic-"+suffix)
		prepared, err := ParseImport([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		rows, err := s.Import(context.Background(), prepared, "")
		if err != nil {
			t.Fatal(err)
		}
		targets = append(targets, activeUsageTarget(rows[0]))
	}
	s.SetUsageRefreshTargets(targets)
	waitUsageCall(t, calls)
	waitUsageCall(t, calls)
	noUsageCall(t, calls)
	s.SetUsageRefreshTargets(nil)
	s.SetUsageRefreshTargets(targets)
	noUsageCall(t, calls)
	close(release)
	waitUsageCondition(t, "all re-enabled targets did not recover", func() bool {
		for _, target := range targets {
			if !s.PoolQuota(target.ID, target.IdentityEpoch, "").Fresh {
				return false
			}
		}
		return true
	})
	if peak.Load() > usageRefreshConcurrency {
		t.Fatalf("background provider concurrency escaped bound: %d", peak.Load())
	}
}

func TestUsageMaintenanceReplacementAndCloseFenceLateWork(t *testing.T) {
	clock := newUsageRefreshClock()
	calls := make(chan string, 10)
	release := make(chan struct{})
	s := usageRefreshTestService(t, clock, UsageFetchFunc(func(_ context.Context, a *coreauth.Auth) (UsageFetchResult, error) {
		calls <- a.ID
		<-release
		return freshUsageFixture(), nil
	}), nil)
	account := importFixture(t, s)
	s.SetUsageRefreshTargets([]UsageRefreshTarget{activeUsageTarget(account)})
	waitUsageCall(t, calls)
	prepared, err := ParseImport([]byte(strings.ReplaceAll(fixture, "fixture", "replacement")))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.Import(context.Background(), prepared, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	waitUsageIdle(t, s)
	noUsageCall(t, calls)
	if s.PoolQuota(rows[0].ID, rows[0].IdentityEpoch, "").Fresh {
		t.Fatal("old identity quota published after replacement")
	}
	s.SetUsageRefreshTargets([]UsageRefreshTarget{activeUsageTarget(rows[0])})
	waitUsageCall(t, calls)
	waitUsageIdle(t, s)
	s.Close()
	s.SetUsageRefreshTargets([]UsageRefreshTarget{activeUsageTarget(rows[0])})
	clock.advance(s, 10*time.Minute)
	noUsageCall(t, calls)
}

func TestUsageMaintenanceCacheResetAndEarlierManualObservationReschedule(t *testing.T) {
	clock := newUsageRefreshClock()
	calls := make(chan string, 10)
	var shortReset atomic.Bool
	s := usageRefreshTestService(t, clock, UsageFetchFunc(func(_ context.Context, a *coreauth.Auth) (UsageFetchResult, error) {
		calls <- a.ID
		result := freshUsageFixture()
		if shortReset.Load() {
			reset := clock.now().Add(20 * time.Second)
			result.Snapshot.RateLimit.Primary.ResetsAt = &reset
		}
		return result, nil
	}), nil)
	account := importFixture(t, s)
	s.SetUsageRefreshTargets([]UsageRefreshTarget{activeUsageTarget(account)})
	waitUsageCall(t, calls)
	waitUsageIdle(t, s)
	if _, err := s.ResetUsage(account.ID); err != nil {
		t.Fatal(err)
	}
	waitUsageCall(t, calls)
	waitUsageIdle(t, s)
	shortReset.Store(true)
	clock.advance(s, time.Second)
	if _, err := s.RefreshUsage(context.Background(), account.ID); err != nil {
		t.Fatal(err)
	}
	waitUsageCall(t, calls)
	waitUsageCondition(t, "manual observation did not advance the scheduled refresh", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		state := s.usageRefresh.targets[account.ID]
		return state != nil && state.next.Equal(clock.now().Add(15*time.Second))
	})
	clock.advance(s, 15*time.Second)
	waitUsageCall(t, calls)
	waitUsageIdle(t, s)
}

func TestUsageMaintenanceFailedRecoveryKeepsBackoffAfterTokenRotation(t *testing.T) {
	clock := newUsageRefreshClock()
	calls := make(chan string, 20)
	var rotations atomic.Int64
	s := usageRefreshTestService(t, clock, UsageFetchFunc(func(_ context.Context, a *coreauth.Auth) (UsageFetchResult, error) {
		calls <- a.ID
		return UsageFetchResult{}, ErrNeedsLogin
	}), RefreshFunc(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
		rotations.Add(1)
		return sdkFixture("synthetic-rotated-access", "synthetic-rotated-refresh"), nil
	}))
	account := importFixture(t, s)
	s.SetUsageRefreshTargets([]UsageRefreshTarget{activeUsageTarget(account)})
	waitUsageCall(t, calls)
	waitUsageCall(t, calls)
	waitUsageIdle(t, s)
	noUsageCall(t, calls)
	if rotations.Load() != 1 {
		t.Fatalf("unauthorized recovery looped without backoff: %d", rotations.Load())
	}
	clock.advance(s, 15*time.Second)
	waitUsageCall(t, calls)
	waitUsageCall(t, calls)
	waitUsageIdle(t, s)
	clock.advance(s, 29*time.Second)
	noUsageCall(t, calls)
	if rotations.Load() != 2 {
		t.Fatalf("failed recovery lost exponential backoff: %d", rotations.Load())
	}
}

func TestUsageMaintenanceDisableLetsIssuedTokenRotationSave(t *testing.T) {
	clock := newUsageRefreshClock()
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	calls := make(chan string, 10)
	s := usageRefreshTestService(t, clock, UsageFetchFunc(func(_ context.Context, a *coreauth.Auth) (UsageFetchResult, error) {
		token, _ := a.Metadata["access_token"].(string)
		calls <- token
		return freshUsageFixture(), nil
	}), RefreshFunc(func(ctx context.Context, _ *coreauth.Auth) (*coreauth.Auth, error) {
		started <- ctx
		<-release
		return sdkFixture("synthetic-access-rotated", "synthetic-refresh-rotated"), nil
	}))
	prepared, err := ParseImport([]byte(strings.ReplaceAll(fixture, "2099-01-01", "2000-01-01")))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.Import(context.Background(), prepared, "")
	if err != nil {
		t.Fatal(err)
	}
	account := rows[0]
	s.SetUsageRefreshTargets([]UsageRefreshTarget{activeUsageTarget(account)})
	var refreshCtx context.Context
	select {
	case refreshCtx = <-started:
	case <-time.After(time.Second):
		t.Fatal("expired account did not begin credential recovery")
	}
	s.SetUsageRefreshTargets(nil)
	if refreshCtx.Err() != nil {
		t.Fatal("target removal abandoned an issued refresh token rotation")
	}
	close(release)
	waitUsageIdle(t, s)
	noUsageCall(t, calls)
	record, err := s.SDKRecord(context.Background(), account.ID)
	if err != nil || record.Metadata["refresh_token"] != "synthetic-refresh-rotated" {
		t.Fatalf("issued token rotation was not retained: err=%v", err)
	}
	s.SetUsageRefreshTargets([]UsageRefreshTarget{activeUsageTarget(account)})
	if got := waitUsageCall(t, calls); got != "synthetic-access-rotated" {
		t.Fatalf("re-enabled maintenance did not use durable rotation: %q", got)
	}
	waitUsageIdle(t, s)
}

func TestUsageMaintenanceCloseCancelsAndTimeoutRejectsLateSuccess(t *testing.T) {
	for _, closeService := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "close"}[closeService], func(t *testing.T) {
			clock := newUsageRefreshClock()
			started := make(chan context.Context, 1)
			s := usageRefreshTestService(t, clock, UsageFetchFunc(func(ctx context.Context, _ *coreauth.Auth) (UsageFetchResult, error) {
				started <- ctx
				<-ctx.Done()
				return freshUsageFixture(), nil // A late success is still inconclusive.
			}), nil)
			s.usageTimeout = 40 * time.Millisecond
			account := importFixture(t, s)
			s.SetUsageRefreshTargets([]UsageRefreshTarget{activeUsageTarget(account)})
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("background request did not start")
			}
			if closeService {
				s.Close()
			}
			waitUsageIdle(t, s)
			if s.PoolQuota(account.ID, account.IdentityEpoch, "").Fresh {
				t.Fatal("timed out or closed background work published usable quota")
			}
		})
	}
}
