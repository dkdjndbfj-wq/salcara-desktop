package codexaccounts

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func newSubscriptionTestService(t *testing.T, opts Options) *Service {
	t.Helper()
	if opts.Path == "" {
		opts.Path = filepath.Join(t.TempDir(), "subscription.vault")
	}
	if opts.Protector == nil {
		opts.Protector = &testProtector{}
	}
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func syntheticSubscriptionResult(expiry time.Time) SubscriptionFetchResult {
	return SubscriptionFetchResult{ProviderAccountID: "explicit-fixture-account", ActiveUntil: timePtr(expiry)}
}

func waitSubscriptionCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("subscription condition did not complete")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSubscriptionServiceFreshnessFailureCloneAndRestart(t *testing.T) {
	base := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	var elapsed atomic.Int64
	var fail atomic.Bool
	path := filepath.Join(t.TempDir(), "vault")
	options := Options{Path: path, Now: func() time.Time { return base.Add(time.Duration(elapsed.Load()) * time.Second) }, SubscriptionFreshFor: 2 * time.Minute,
		SubscriptionFetcher: SubscriptionFetchFunc(func(context.Context, *coreauth.Auth) (SubscriptionFetchResult, error) {
			if fail.Load() {
				return SubscriptionFetchResult{}, errors.New("private synthetic-access-old explicit-fixture-account")
			}
			return syntheticSubscriptionResult(base.Add(time.Hour)), nil
		})}
	s := newSubscriptionTestService(t, options)
	account := importFixture(t, s)
	snapshot, err := s.RefreshSubscriptionForIdentity(context.Background(), account.ID, account.IdentityEpoch)
	if err != nil || !snapshot.Verified || snapshot.Status != SubscriptionStatusActive || snapshot.Freshness != SubscriptionFreshnessFresh || snapshot.RemainingSeconds == nil || *snapshot.RemainingSeconds != 3600 || snapshot.CredentialEpoch != account.Epoch || snapshot.IdentityEpoch != account.IdentityEpoch {
		t.Fatalf("verified snapshot=%+v err=%v", snapshot, err)
	}
	raw, _ := json.Marshal(snapshot)
	for _, secret := range []string{"synthetic-access", "synthetic-refresh", "explicit-fixture-account"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("snapshot leaked provider credential/identity")
		}
	}
	*snapshot.ActiveUntil = base.Add(24 * time.Hour)
	*snapshot.RemainingSeconds = 7777
	elapsed.Store(30)
	listed, _ := s.Get(account.ID)
	if *listed.Subscription.RemainingSeconds != 3570 || !listed.Subscription.ActiveUntil.Equal(base.Add(time.Hour)) {
		t.Fatal("cached countdown or pointer isolation failed")
	}
	fail.Store(true)
	failed, err := s.RefreshSubscription(context.Background(), account.ID)
	if !errors.Is(err, ErrSubscriptionUnavailable) || failed.Verified || failed.Status != SubscriptionStatusUnknown || failed.RemainingSeconds != nil || failed.Freshness != SubscriptionFreshnessStale || failed.ErrorCode != "unavailable" || failed.ObservedAt == nil || !failed.ObservedAt.Equal(base) {
		t.Fatalf("failed snapshot=%+v err=%v", failed, err)
	}
	fail.Store(false)
	if _, err = s.RefreshSubscription(context.Background(), account.ID); err != nil {
		t.Fatal(err)
	}
	elapsed.Store(151)
	stale, _ := s.Subscription(account.ID)
	if stale.Verified || stale.Status != SubscriptionStatusUnknown || stale.RemainingSeconds != nil || stale.Freshness != SubscriptionFreshnessStale {
		t.Fatalf("expired cache=%+v", stale)
	}
	s.Close()
	restarted := newSubscriptionTestService(t, options)
	unknown, _ := restarted.Subscription(account.ID)
	if unknown.Verified || unknown.ActiveUntil != nil || unknown.Status != SubscriptionStatusUnknown {
		t.Fatalf("restart restored authoritative cache: %+v", unknown)
	}
}

func TestSubscriptionTermBoundaryRequiresNewObservation(t *testing.T) {
	base := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	var elapsed atomic.Int64
	s := newSubscriptionTestService(t, Options{Now: func() time.Time { return base.Add(time.Duration(elapsed.Load()) * time.Second) }, SubscriptionFetcher: SubscriptionFetchFunc(func(context.Context, *coreauth.Auth) (SubscriptionFetchResult, error) {
		return syntheticSubscriptionResult(base.Add(time.Minute)), nil
	})})
	account := importFixture(t, s)
	if _, err := s.RefreshSubscription(context.Background(), account.ID); err != nil {
		t.Fatal(err)
	}
	elapsed.Store(60)
	stale, _ := s.Subscription(account.ID)
	if stale.Verified || stale.Status != SubscriptionStatusUnknown {
		t.Fatal("old term boundary became verified expiry without a new read")
	}
	expired, err := s.RefreshSubscription(context.Background(), account.ID)
	if err != nil || !expired.Verified || expired.Status != SubscriptionStatusExpired || *expired.RemainingSeconds != 0 {
		t.Fatalf("current expired observation=%+v err=%v", expired, err)
	}
}

func TestSubscriptionServiceUnknownResultCannotPromoteImportHint(t *testing.T) {
	for _, kind := range []string{"no_expiry", "missing_identity", "wrong_identity"} {
		t.Run(kind, func(t *testing.T) {
			s := newSubscriptionTestService(t, Options{SubscriptionFetcher: SubscriptionFetchFunc(func(context.Context, *coreauth.Auth) (SubscriptionFetchResult, error) {
				result := syntheticSubscriptionResult(time.Now().Add(time.Hour))
				switch kind {
				case "no_expiry":
					result.ActiveUntil = nil
				case "missing_identity":
					result.ProviderAccountID = ""
				case "wrong_identity":
					result.ProviderAccountID = "other"
				}
				return result, nil
			})})
			prepared, err := ParseImport([]byte(strings.Replace(fixture, `"type":"codex"`, `"type":"codex","subscription_active_until":"2098-01-01T00:00:00Z"`, 1)))
			if err != nil {
				t.Fatal(err)
			}
			accounts, err := s.Import(context.Background(), prepared, "")
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := s.RefreshSubscription(context.Background(), accounts[0].ID)
			if err == nil || snapshot.Verified || snapshot.ActiveUntil != nil || snapshot.RemainingSeconds != nil || snapshot.Status != SubscriptionStatusUnknown {
				t.Fatalf("unreliable result promoted hint: %+v %v", snapshot, err)
			}
		})
	}
}

func TestSubscriptionServiceUnauthorizedRotatesExactlyOnce(t *testing.T) {
	for _, mode := range []string{"success", "second_401", "save_failure", "expired_first"} {
		t.Run(mode, func(t *testing.T) {
			var fetches, rotations atomic.Int32
			protector := &testProtector{}
			s := newSubscriptionTestService(t, Options{Protector: protector,
				Refresher: RefreshFunc(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
					rotations.Add(1)
					if mode == "save_failure" {
						protector.fail.Store(true)
					}
					return sdkFixture("synthetic-access-new", "synthetic-refresh-new"), nil
				}),
				SubscriptionFetcher: SubscriptionFetchFunc(func(_ context.Context, a *coreauth.Auth) (SubscriptionFetchResult, error) {
					fetches.Add(1)
					if a.Metadata["access_token"] == "synthetic-access-old" {
						if mode == "expired_first" {
							t.Error("expired token reached subscription endpoint")
						}
						return SubscriptionFetchResult{}, ErrNeedsLogin
					}
					if mode == "second_401" {
						return SubscriptionFetchResult{}, ErrNeedsLogin
					}
					return syntheticSubscriptionResult(time.Now().Add(time.Hour)), nil
				})})
			account := importFixture(t, s)
			if mode == "expired_first" {
				s.mu.Lock()
				c := s.vault.Accounts[account.ID]
				c.ExpiresAt = time.Unix(1, 0)
				s.vault.Accounts[account.ID] = c
				s.mu.Unlock()
			}
			snapshot, err := s.RefreshSubscription(context.Background(), account.ID)
			wantCalls := int32(2)
			if mode == "expired_first" || mode == "save_failure" {
				wantCalls = 1
			}
			if rotations.Load() != 1 || fetches.Load() != wantCalls {
				t.Fatalf("fetches=%d rotations=%d", fetches.Load(), rotations.Load())
			}
			if mode == "second_401" {
				if !errors.Is(err, ErrNeedsLogin) || snapshot.Verified {
					t.Fatalf("second unauthorized result=%+v err=%v", snapshot, err)
				}
				return
			}
			if mode == "save_failure" {
				if !errors.Is(err, ErrSaveFailed) || snapshot.Verified {
					t.Fatalf("unsafe rotation result=%+v err=%v", snapshot, err)
				}
				return
			}
			if err != nil || !snapshot.Verified || snapshot.CredentialEpoch != account.Epoch+1 || snapshot.IdentityEpoch != account.IdentityEpoch {
				t.Fatalf("rotation result=%+v err=%v", snapshot, err)
			}
		})
	}
}

func TestSubscriptionServiceCancellationCoalescingAndTimeout(t *testing.T) {
	for _, mode := range []string{"one_waiter_cancelled", "all_waiters_cancelled", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var fetches atomic.Int32
			timeout := time.Second
			if mode == "timeout" {
				timeout = 30 * time.Millisecond
			}
			s := newSubscriptionTestService(t, Options{SubscriptionTimeout: timeout, SubscriptionFetcher: SubscriptionFetchFunc(func(context.Context, *coreauth.Auth) (SubscriptionFetchResult, error) {
				if fetches.Add(1) == 1 {
					close(entered)
					<-release
				}
				return syntheticSubscriptionResult(time.Now().Add(time.Hour)), nil
			})})
			defer close(release)
			account := importFixture(t, s)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := s.RefreshSubscription(ctx, account.ID); done <- err }()
			<-entered
			s.mu.Lock()
			flight := s.subscriptionFlights[account.ID]
			s.mu.Unlock()
			if mode == "one_waiter_cancelled" {
				other := make(chan error, 1)
				go func() { _, err := s.RefreshSubscription(context.Background(), account.ID); other <- err }()
				waitSubscriptionCondition(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return flight.waiters == 2 })
				cancel()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled waiter=%v", err)
				}
				// Release in a separate goroutine so the deferred close stays the
				// sole channel close even if this assertion fails.
				go func() { release <- struct{}{} }()
				if err := <-other; err != nil {
					t.Fatalf("other waiter lost shared result: %v", err)
				}
				snapshot, _ := s.Subscription(account.ID)
				if !snapshot.Verified || fetches.Load() != 1 {
					t.Fatal("coalesced request did not preserve active waiter")
				}
				return
			}
			if mode == "all_waiters_cancelled" {
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("waiter=%v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("ignoring connector held caller after timeout/cancel")
			}
			if _, err := s.RefreshSubscription(context.Background(), account.ID); !errors.Is(err, ErrConflict) {
				t.Fatalf("cancelled flight overlapped new request: %v", err)
			}
			go func() { release <- struct{}{} }()
			<-flight.done
			snapshot, _ := s.Subscription(account.ID)
			if snapshot.Verified || snapshot.Status != SubscriptionStatusUnknown {
				t.Fatal("late success survived cancellation")
			}
		})
	}
}

func TestSubscriptionServiceFencesReplacementDeletionRotationAndClose(t *testing.T) {
	for _, action := range []string{"replace", "delete", "rotate", "close"} {
		t.Run(action, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			s := newSubscriptionTestService(t, Options{Refresher: RefreshFunc(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
				return sdkFixture("new-access", "new-refresh"), nil
			}), SubscriptionFetcher: SubscriptionFetchFunc(func(context.Context, *coreauth.Auth) (SubscriptionFetchResult, error) {
				close(entered)
				<-release
				return syntheticSubscriptionResult(time.Now().Add(time.Hour)), nil
			})})
			defer close(release)
			account := importFixture(t, s)
			done := make(chan error, 1)
			go func() { _, err := s.RefreshSubscription(context.Background(), account.ID); done <- err }()
			<-entered
			s.mu.Lock()
			flight := s.subscriptionFlights[account.ID]
			s.mu.Unlock()
			var closeDone chan struct{}
			switch action {
			case "replace":
				p, _ := ParseImport([]byte(strings.Replace(fixture, "synthetic-access-old", "replacement-access", 1)))
				if _, err := s.Import(context.Background(), p, account.ID); err != nil {
					t.Fatal(err)
				}
			case "delete":
				if err := s.Delete(context.Background(), account.ID); err != nil {
					t.Fatal(err)
				}
			case "rotate":
				if _, err := s.Refresh(context.Background(), account.ID); err != nil {
					t.Fatal(err)
				}
			case "close":
				closeDone = make(chan struct{})
				go func() { s.Close(); close(closeDone) }()
				waitSubscriptionCondition(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.closed })
			}
			go func() { release <- struct{}{} }()
			<-flight.done
			if err := <-done; err == nil {
				t.Fatal("late fenced success accepted")
			}
			if closeDone != nil {
				<-closeDone
			}
			current, err := s.Subscription(account.ID)
			if err == nil && current.Verified {
				t.Fatal("fenced cache survived")
			}
		})
	}
}

func TestUsageAndSubscriptionIdentityPinnedBeforeNetwork(t *testing.T) {
	var called atomic.Int32
	s := newSubscriptionTestService(t, Options{UsageFetcher: UsageFetchFunc(func(context.Context, *coreauth.Auth) (UsageFetchResult, error) {
		called.Add(1)
		return UsageFetchResult{}, nil
	}), SubscriptionFetcher: SubscriptionFetchFunc(func(context.Context, *coreauth.Auth) (SubscriptionFetchResult, error) {
		called.Add(1)
		return SubscriptionFetchResult{}, nil
	})})
	account := importFixture(t, s)
	for _, epoch := range []uint64{0, account.IdentityEpoch + 1} {
		if _, err := s.RefreshUsageForIdentity(context.Background(), account.ID, epoch); !errors.Is(err, ErrConflict) {
			t.Fatalf("usage identity=%d err=%v", epoch, err)
		}
	}
	if _, err := s.RefreshSubscriptionForIdentity(context.Background(), account.ID, account.IdentityEpoch+1); !errors.Is(err, ErrConflict) {
		t.Fatalf("subscription identity: %v", err)
	}
	if called.Load() != 0 {
		t.Fatal("stale identity made a network request")
	}
}

func TestSubscriptionCancelledCallerCannotPublishOrStartRecovery(t *testing.T) {
	for _, unauthorized := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		var rotations atomic.Int32
		s := newSubscriptionTestService(t, Options{
			Refresher: RefreshFunc(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
				rotations.Add(1)
				return sdkFixture("new-access", "new-refresh"), nil
			}),
			SubscriptionFetcher: SubscriptionFetchFunc(func(context.Context, *coreauth.Auth) (SubscriptionFetchResult, error) {
				cancel() // Cancellation already happened before the provider result.
				if unauthorized {
					return SubscriptionFetchResult{}, ErrNeedsLogin
				}
				return syntheticSubscriptionResult(time.Now().Add(time.Hour)), nil
			}),
		})
		account := importFixture(t, s)
		result, err := s.RefreshSubscription(ctx, account.ID)
		if !errors.Is(err, context.Canceled) || result.Verified {
			t.Fatalf("cancelled result=%+v error=%v", result, err)
		}
		waitSubscriptionCondition(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.subscriptionFlights[account.ID] == nil })
		cached, _ := s.Subscription(account.ID)
		if cached.Verified || rotations.Load() != 0 {
			t.Fatal("late result published or started token rotation after cancellation")
		}
	}
}

func TestUsageAndSubscriptionReplacementDuringCredentialRecovery(t *testing.T) {
	for _, kind := range []string{"usage", "subscription"} {
		t.Run(kind, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var fetched atomic.Int32
			s := newSubscriptionTestService(t, Options{
				Refresher: RefreshFunc(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
					close(entered)
					<-release
					return sdkFixture("new-access", "new-refresh"), nil
				}),
				UsageFetcher: UsageFetchFunc(func(context.Context, *coreauth.Auth) (UsageFetchResult, error) {
					fetched.Add(1)
					return UsageFetchResult{}, nil
				}),
				SubscriptionFetcher: SubscriptionFetchFunc(func(context.Context, *coreauth.Auth) (SubscriptionFetchResult, error) {
					fetched.Add(1)
					return SubscriptionFetchResult{}, nil
				}),
			})
			defer close(release)
			account := importFixture(t, s)
			s.mu.Lock()
			c := s.vault.Accounts[account.ID]
			c.ExpiresAt = time.Unix(1, 0)
			s.vault.Accounts[account.ID] = c
			s.mu.Unlock()
			done := make(chan error, 1)
			go func() {
				var err error
				if kind == "usage" {
					_, err = s.RefreshUsageForIdentity(context.Background(), account.ID, account.IdentityEpoch)
				} else {
					_, err = s.RefreshSubscriptionForIdentity(context.Background(), account.ID, account.IdentityEpoch)
				}
				done <- err
			}()
			<-entered
			prepared, _ := ParseImport([]byte(strings.Replace(fixture, "synthetic-access-old", "replacement-access", 1)))
			if _, err := s.Import(context.Background(), prepared, account.ID); err != nil {
				t.Fatal(err)
			}
			go func() { release <- struct{}{} }()
			if err := <-done; !errors.Is(err, ErrConflict) {
				t.Fatalf("replacement crossed recovery: %v", err)
			}
			if fetched.Load() != 0 {
				t.Fatal("a delayed action fetched replacement account")
			}
		})
	}
}
