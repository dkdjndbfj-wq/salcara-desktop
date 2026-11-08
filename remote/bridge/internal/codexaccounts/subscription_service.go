package codexaccounts

import (
	"context"
	"errors"
)

type subscriptionEntry struct {
	snapshot      SubscriptionMetadata
	epoch         uint64
	identityEpoch uint64
}

type subscriptionFlight struct {
	done           chan struct{}
	ctx            context.Context
	cancel         context.CancelFunc
	epoch          uint64
	identityEpoch  uint64
	waiters        int
	waiterContexts map[*subscriptionWaiter]struct{}
	cancelled      bool
	snapshot       SubscriptionMetadata
	err            error
}

type subscriptionWaiter struct{ ctx context.Context }

func cloneSubscription(snapshot SubscriptionMetadata) SubscriptionMetadata {
	if snapshot.ActiveUntil != nil {
		value := *snapshot.ActiveUntil
		snapshot.ActiveUntil = &value
	}
	if snapshot.ObservedAt != nil {
		value := *snapshot.ObservedAt
		snapshot.ObservedAt = &value
	}
	if snapshot.FreshUntil != nil {
		value := *snapshot.FreshUntil
		snapshot.FreshUntil = &value
	}
	if snapshot.LastAttemptAt != nil {
		value := *snapshot.LastAttemptAt
		snapshot.LastAttemptAt = &value
	}
	if snapshot.RemainingSeconds != nil {
		value := *snapshot.RemainingSeconds
		snapshot.RemainingSeconds = &value
	}
	return snapshot
}

func unknownSubscription() SubscriptionMetadata {
	return SubscriptionMetadata{Status: SubscriptionStatusUnknown, Source: SubscriptionSourceOfficial, Freshness: SubscriptionFreshnessUnavailable}
}

func (s *Service) subscriptionLocked(c credential) SubscriptionMetadata {
	snapshot := subscriptionMetadata(c, s.now())
	snapshot.CredentialEpoch, snapshot.IdentityEpoch = c.Epoch, c.IdentityEpoch
	entry, exists := s.subscriptions[c.ID]
	if !exists || entry.epoch != c.Epoch || entry.identityEpoch != c.IdentityEpoch {
		return snapshot
	}
	snapshot = cloneSubscription(entry.snapshot)
	now := s.now().UTC()
	if snapshot.Freshness != SubscriptionFreshnessFresh || snapshot.FreshUntil == nil || !now.Before(*snapshot.FreshUntil) || snapshot.ObservedAt == nil || now.Before(*snapshot.ObservedAt) {
		snapshot.Status, snapshot.Verified, snapshot.RemainingSeconds = SubscriptionStatusUnknown, false, nil
		if snapshot.ObservedAt != nil {
			snapshot.Freshness = SubscriptionFreshnessStale
		}
		return snapshot
	}
	if snapshot.ActiveUntil != nil {
		remaining := snapshot.ActiveUntil.Unix() - now.Unix()
		snapshot.Status = SubscriptionStatusActive
		if !now.Before(*snapshot.ActiveUntil) {
			snapshot.Status, remaining = SubscriptionStatusExpired, 0
		}
		snapshot.RemainingSeconds = &remaining
	}
	return snapshot
}

// Subscription reads only the local cache. Reopening the vault starts without
// authoritative observations; persisted import hints remain unverified.
func (s *Service) Subscription(id string) (SubscriptionMetadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return unknownSubscription(), ErrClosed
	}
	c, exists := s.vault.Accounts[id]
	if !exists {
		return unknownSubscription(), ErrNotFound
	}
	return s.subscriptionLocked(c), nil
}

func (s *Service) RefreshSubscription(ctx context.Context, id string) (SubscriptionMetadata, error) {
	return s.RefreshSubscriptionForIdentity(ctx, id, 0)
}

// RefreshSubscriptionForIdentity binds an explicit user action to the account
// version shown by the caller. Zero selects the identity current at entry for
// trusted backend callers. Every recovery/retry rechecks that same identity.
func (s *Service) RefreshSubscriptionForIdentity(ctx context.Context, id string, identityEpoch uint64) (SubscriptionMetadata, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return unknownSubscription(), ErrClosed
	}
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return unknownSubscription(), err
	}
	c, exists := s.vault.Accounts[id]
	if !exists {
		s.mu.Unlock()
		return unknownSubscription(), ErrNotFound
	}
	if identityEpoch != 0 && identityEpoch != c.IdentityEpoch {
		s.mu.Unlock()
		return unknownSubscription(), ErrConflict
	}
	identityEpoch = c.IdentityEpoch
	s.subscriptionWork[id]++
	s.bumpCleanupObservationLocked(id)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.subscriptionWork[id]--
		if s.subscriptionWork[id] == 0 {
			delete(s.subscriptionWork, id)
		}
		s.mu.Unlock()
	}()

	// Credential recovery remains under the existing durable rotation owner.
	// Its epoch checks prevent a delayed subscription action from refreshing a
	// replacement account. Token expiry is never used as a subscription term.
	if !c.ExpiresAt.IsZero() && !s.now().Before(c.ExpiresAt) {
		attempt := usageAttempt{epoch: c.Epoch, identityEpoch: identityEpoch}
		if err := s.rotateUsageCredentialAfterUnauthorized(ctx, id, attempt); err != nil {
			return s.subscriptionRecoveryFailure(id, attempt, err), err
		}
	}
	snapshot, attempt, err := s.refreshSubscriptionOnce(ctx, id, identityEpoch)
	if !errors.Is(err, ErrNeedsLogin) {
		return snapshot, err
	}
	if err = ctx.Err(); err == nil {
		err = s.rotateUsageCredentialAfterUnauthorized(ctx, id, attempt)
	}
	if err != nil {
		return s.subscriptionRecoveryFailure(id, attempt, err), err
	}
	// A second 401 is final for this action. It never starts another rotation.
	snapshot, _, err = s.refreshSubscriptionOnce(ctx, id, identityEpoch)
	return snapshot, err
}

func (s *Service) subscriptionRecoveryFailure(id string, attempt usageAttempt, err error) SubscriptionMetadata {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, exists := s.vault.Accounts[id]
	if s.closed || !exists || c.IdentityEpoch != attempt.identityEpoch {
		return unknownSubscription()
	}
	if c.Epoch != attempt.epoch {
		if _, exists := s.subscriptions[id]; exists {
			return s.subscriptionLocked(c)
		}
		snapshot := unknownSubscription()
		snapshot.CredentialEpoch, snapshot.IdentityEpoch = c.Epoch, c.IdentityEpoch
		snapshot.LastAttemptAt, snapshot.ErrorCode = timePtr(s.now().UTC()), subscriptionErrorCode(err)
		return snapshot
	}
	return s.failedSubscriptionLocked(c, err)
}

func (s *Service) refreshSubscriptionOnce(ctx context.Context, id string, identityEpoch uint64) (SubscriptionMetadata, usageAttempt, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return unknownSubscription(), usageAttempt{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return unknownSubscription(), usageAttempt{}, err
	}
	c, exists := s.vault.Accounts[id]
	if !exists {
		s.mu.Unlock()
		return unknownSubscription(), usageAttempt{}, ErrNotFound
	}
	attempt := usageAttempt{epoch: c.Epoch, identityEpoch: c.IdentityEpoch}
	if identityEpoch != c.IdentityEpoch {
		s.mu.Unlock()
		return unknownSubscription(), attempt, ErrConflict
	}
	for _, login := range s.logins {
		if login.session.Status == "pending" && login.request.ReplaceID == id {
			s.mu.Unlock()
			return unknownSubscription(), attempt, ErrConflict
		}
	}
	if s.pending[id].epoch != 0 {
		snapshot := s.failedSubscriptionLocked(c, ErrSaveFailed)
		s.mu.Unlock()
		return snapshot, attempt, ErrSaveFailed
	}
	f := s.subscriptionFlights[id]
	if f != nil && (f.cancelled || f.epoch != c.Epoch || f.identityEpoch != c.IdentityEpoch) {
		s.mu.Unlock()
		return unknownSubscription(), attempt, ErrConflict
	}
	if f == nil {
		fetchCtx, cancel := context.WithTimeout(context.Background(), s.subscriptionTimeout)
		f = &subscriptionFlight{done: make(chan struct{}), ctx: fetchCtx, cancel: cancel, epoch: c.Epoch, identityEpoch: c.IdentityEpoch, waiterContexts: make(map[*subscriptionWaiter]struct{})}
		s.subscriptionFlights[id] = f
		go s.runSubscriptionRefresh(id, c, f)
	}
	waiter := &subscriptionWaiter{ctx: ctx}
	f.waiterContexts[waiter] = struct{}{}
	f.waiters++
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		s.releaseSubscriptionWaiter(id, f, waiter, ctx.Err())
		return unknownSubscription(), attempt, ctx.Err()
	case <-f.ctx.Done():
		// A timeout releases callers even if an injected connector ignores it.
		// Keep the cancelled flight registered until it unwinds so it cannot
		// overlap a new read under the same credential version.
		s.releaseSubscriptionWaiter(id, f, waiter, f.ctx.Err())
		select {
		case <-f.done:
			return cloneSubscription(f.snapshot), attempt, f.err
		default:
			return unknownSubscription(), attempt, f.ctx.Err()
		}
	case <-f.done:
		return cloneSubscription(f.snapshot), attempt, f.err
	}
}

func (s *Service) releaseSubscriptionWaiter(id string, f *subscriptionFlight, waiter *subscriptionWaiter, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.subscriptionFlights[id] != f {
		return
	}
	delete(f.waiterContexts, waiter)
	f.waiters = len(f.waiterContexts)
	if f.waiters > 0 && f.ctx.Err() == nil {
		return
	}
	f.cancelled = true
	f.cancel()
	c, exists := s.vault.Accounts[id]
	if exists && !s.closed && c.Epoch == f.epoch && c.IdentityEpoch == f.identityEpoch {
		s.failedSubscriptionLocked(c, err)
	}
}

func (s *Service) runSubscriptionRefresh(id string, old credential, f *subscriptionFlight) {
	defer f.cancel()
	result, err := s.subscriptionFetcher.FetchSubscription(f.ctx, old.sdk())
	s.mu.Lock()
	defer s.mu.Unlock()
	defer close(f.done)
	c, exists := s.vault.Accounts[id]
	if s.closed || !exists || c.Epoch != f.epoch || c.IdentityEpoch != f.identityEpoch || s.subscriptionFlights[id] != f {
		f.snapshot, f.err = unknownSubscription(), ErrConflict
		if s.subscriptionFlights[id] == f {
			delete(s.subscriptionFlights, id)
		}
		return
	}
	delete(s.subscriptionFlights, id)
	// A caller cancellation is effective before its goroutine is scheduled to
	// leave the wait. Check the registered contexts as part of publication so
	// a response cannot win a race against the last caller's cancellation.
	activeWaiter := false
	for waiter := range f.waiterContexts {
		activeWaiter = activeWaiter || waiter.ctx.Err() == nil
	}
	if !activeWaiter {
		err = context.Canceled
	}
	if f.ctx.Err() != nil {
		err = f.ctx.Err()
	}
	if f.cancelled && err == nil {
		err = context.Canceled
	}
	if err == nil && (old.AccountID == "" || result.ProviderAccountID == "" || result.ProviderAccountID != old.AccountID) {
		err = subscriptionError("identity_mismatch", ErrConflict)
	}
	if err == nil && (result.ActiveUntil == nil || result.ActiveUntil.Unix() <= 0 || result.ActiveUntil.Unix() >= 253402300800) {
		err = subscriptionError("no_expiry", nil)
	}
	if err != nil {
		f.snapshot = s.failedSubscriptionLocked(c, err)
		switch {
		case errors.Is(err, ErrNeedsLogin):
			f.err = ErrNeedsLogin
		case errors.Is(err, ErrConflict):
			f.err = ErrConflict
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			f.err = err
		default:
			f.err = ErrSubscriptionUnavailable
		}
		return
	}
	now, expiry := s.now().UTC(), result.ActiveUntil.UTC()
	freshUntil := now.Add(s.subscriptionFreshFor)
	// Reaching a future term boundary requires another observation; renewal
	// may have happened since this read. Never infer expired from an old cache.
	if expiry.After(now) && expiry.Before(freshUntil) {
		freshUntil = expiry
	}
	snapshot := SubscriptionMetadata{
		Status: SubscriptionStatusActive, Source: SubscriptionSourceOfficial, Verified: true,
		ActiveUntil: &expiry, ObservedAt: timePtr(now), Freshness: SubscriptionFreshnessFresh,
		FreshUntil: &freshUntil, LastAttemptAt: timePtr(now), CredentialEpoch: c.Epoch, IdentityEpoch: c.IdentityEpoch,
	}
	remaining := expiry.Unix() - now.Unix()
	if !now.Before(expiry) {
		snapshot.Status, remaining = SubscriptionStatusExpired, 0
	}
	snapshot.RemainingSeconds = &remaining
	s.subscriptions[id] = subscriptionEntry{snapshot: cloneSubscription(snapshot), epoch: c.Epoch, identityEpoch: c.IdentityEpoch}
	f.snapshot = cloneSubscription(snapshot)
}

func (s *Service) failedSubscriptionLocked(c credential, err error) SubscriptionMetadata {
	snapshot := unknownSubscription()
	if entry, ok := s.subscriptions[c.ID]; ok && entry.epoch == c.Epoch && entry.identityEpoch == c.IdentityEpoch {
		snapshot = cloneSubscription(entry.snapshot)
		if snapshot.ObservedAt != nil {
			snapshot.Freshness = SubscriptionFreshnessStale
		}
	}
	snapshot.Status, snapshot.Verified, snapshot.RemainingSeconds = SubscriptionStatusUnknown, false, nil
	snapshot.LastAttemptAt, snapshot.ErrorCode = timePtr(s.now().UTC()), subscriptionErrorCode(err)
	snapshot.CredentialEpoch, snapshot.IdentityEpoch = c.Epoch, c.IdentityEpoch
	s.subscriptions[c.ID] = subscriptionEntry{snapshot: cloneSubscription(snapshot), epoch: c.Epoch, identityEpoch: c.IdentityEpoch}
	return snapshot
}

func (s *Service) fenceSubscriptionLocked(id string) {
	if f := s.subscriptionFlights[id]; f != nil {
		f.cancelled = true
		f.cancel()
		delete(s.subscriptionFlights, id)
	}
	delete(s.subscriptions, id)
}
