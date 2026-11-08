package codexaccounts

import (
	"context"
	"errors"
)

const (
	resetStatePrepared  = "prepared"
	resetStateConsuming = "consuming"
	resetStateSucceeded = "succeeded"
	resetStateFailed    = "failed"
	resetStateUnknown   = "unknown"
)

type resetCreditFlight struct {
	done          chan struct{}
	cancel        context.CancelFunc
	identityEpoch uint64
	redeemID      string
	result        ResetCreditRequest
	err           error
}

func (s *Service) fenceResetCreditLocked(id string) {
	if f := s.resetFlights[id]; f != nil {
		f.cancel()
		delete(s.resetFlights, id)
	}
}

func validResetRedemptionState(state string) bool {
	switch state {
	case resetStatePrepared, resetStateConsuming, resetStateSucceeded, resetStateFailed, resetStateUnknown:
		return true
	default:
		return false
	}
}

func resetRequestFromRecord(r resetRedemption) ResetCreditRequest {
	result := ResetCreditRequest{RedeemRequestID: r.ID, IdentityEpoch: r.IdentityEpoch, State: r.State, CreatedAt: r.CreatedAt}
	if r.CompletedAt != nil {
		completed := *r.CompletedAt
		result.CompletedAt = &completed
	}
	return result
}

// PrepareResetCredit creates an explicit, durable one-use redemption handle.
// It does not contact the provider and is safe to call repeatedly.
func (s *Service) PrepareResetCredit(ctx context.Context, id string) (ResetCreditRequest, error) {
	return s.prepareResetCredit(ctx, id, "")
}

// PrepareNextResetCredit explicitly starts a distinct redemption after a
// successful one. The predecessor is a compare-and-swap token, not permission
// to replay an ambiguous POST. Retrying it returns only the same successor.
func (s *Service) PrepareNextResetCredit(ctx context.Context, id, previousRedeemID string) (ResetCreditRequest, error) {
	if !validRedeemRequestID(previousRedeemID) {
		return ResetCreditRequest{}, ErrConflict
	}
	return s.prepareResetCredit(ctx, id, previousRedeemID)
}

func (s *Service) prepareResetCredit(ctx context.Context, id, previousRedeemID string) (ResetCreditRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ResetCreditRequest{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return ResetCreditRequest{}, err
	}
	c, ok := s.vault.Accounts[id]
	if !ok {
		return ResetCreditRequest{}, ErrNotFound
	}
	if c.AccessToken == "" {
		return ResetCreditRequest{}, ErrNeedsLogin
	}
	if c.TokenSourceMode == "chatgpt_web_session" {
		return ResetCreditRequest{}, ErrResetUnavailable
	}
	existing, exists := s.vault.ResetRedemptions[id]
	exists = exists && existing.IdentityEpoch == c.IdentityEpoch
	if previousRedeemID == "" {
		if exists {
			return resetRequestFromRecord(existing), nil
		}
	} else {
		if !exists {
			return ResetCreditRequest{}, ErrConflict
		}
		if existing.PreviousID == previousRedeemID {
			return resetRequestFromRecord(existing), nil
		}
		if existing.ID != previousRedeemID {
			return ResetCreditRequest{}, ErrConflict
		}
		if existing.State == resetStateUnknown || existing.State == resetStateConsuming {
			return resetRequestFromRecord(existing), ErrResetUnknown
		}
		if existing.State != resetStateSucceeded || existing.CompletedAt == nil || s.resetFlights[id] != nil {
			return resetRequestFromRecord(existing), resetError("previous_not_completed", ErrResetUnavailable)
		}
		if err := s.nextResetCreditUsageLocked(c, existing); err != nil {
			return resetRequestFromRecord(existing), err
		}
	}
	redeemID, err := newRedeemRequestID()
	if err != nil {
		return ResetCreditRequest{}, err
	}
	next := cloneVault(s.vault)
	next.ResetRedemptions[id] = resetRedemption{ID: redeemID, PreviousID: previousRedeemID, IdentityEpoch: c.IdentityEpoch, State: resetStatePrepared, CreatedAt: s.now().UTC()}
	if err := writeVault(s.path, s.protector, next); err != nil {
		return ResetCreditRequest{}, err
	}
	s.vault = next
	return resetRequestFromRecord(next.ResetRedemptions[id]), nil
}

// Only an authenticated observation that started after the previous POST
// completed can authorize another handle. A late pre-reset response, import
// hint or snapshot from a replaced/rotated credential cannot do so.
func (s *Service) nextResetCreditUsageLocked(c credential, previous resetRedemption) error {
	entry, ok := s.usage[c.ID]
	if !ok || entry.epoch != c.Epoch || entry.identityEpoch != c.IdentityEpoch || !s.poolMetricsAvailableLocked(c) {
		return resetError("usage_refresh_required", ErrResetUnavailable)
	}
	usage, now := entry.snapshot, s.now()
	if usage.Status != UsageStatusFresh || !usage.Verified || usage.Source != UsageSourceOfficial || usage.FreshUntil == nil || !now.Before(*usage.FreshUntil) || usage.ObservedAt == nil || usage.FetchStartedAt == nil || previous.CompletedAt == nil || !usage.FetchStartedAt.After(*previous.CompletedAt) || usage.ObservedAt.Before(*usage.FetchStartedAt) || usage.ObservedAt.After(now) {
		return resetError("usage_refresh_required", ErrResetUnavailable)
	}
	if usage.ResetCreditsAvailable == nil {
		return resetError("usage_refresh_required", ErrResetUnavailable)
	}
	if *usage.ResetCreditsAvailable <= 0 {
		return resetError("no_reset_credits", ErrResetUnavailable)
	}
	return nil
}

// ResetCreditStatus reads the durable operation state and never performs a
// network call. A restarted process converts an interrupted POST to unknown.
func (s *Service) ResetCreditStatus(id string) (ResetCreditRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ResetCreditRequest{}, ErrClosed
	}
	c, ok := s.vault.Accounts[id]
	if !ok {
		return ResetCreditRequest{}, ErrNotFound
	}
	r, ok := s.vault.ResetRedemptions[id]
	if !ok || r.IdentityEpoch != c.IdentityEpoch {
		return ResetCreditRequest{}, ErrNotFound
	}
	return resetRequestFromRecord(r), nil
}

// ConsumeResetCredit atomically binds a prepared handle to the current
// identity. It coalesces concurrent calls and refreshes a 401 exactly once,
// reusing the same handle so provider idempotency protects the user.
func (s *Service) ConsumeResetCredit(ctx context.Context, id, redeemID string) (ResetCreditRequest, error) {
	if !validRedeemRequestID(redeemID) {
		return ResetCreditRequest{}, ErrResetUnavailable
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ResetCreditRequest{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return ResetCreditRequest{}, err
	}
	c, ok := s.vault.Accounts[id]
	if !ok {
		s.mu.Unlock()
		return ResetCreditRequest{}, ErrNotFound
	}
	if c.TokenSourceMode == "chatgpt_web_session" {
		s.mu.Unlock()
		return ResetCreditRequest{}, ErrResetUnavailable
	}
	r, ok := s.vault.ResetRedemptions[id]
	if !ok || r.ID != redeemID || r.IdentityEpoch != c.IdentityEpoch {
		s.mu.Unlock()
		return ResetCreditRequest{}, ErrConflict
	}
	if r.State == resetStateSucceeded {
		s.mu.Unlock()
		return resetRequestFromRecord(r), nil
	}
	if r.State == resetStateUnknown {
		s.mu.Unlock()
		return resetRequestFromRecord(r), ErrResetUnknown
	}
	if f := s.resetFlights[id]; f != nil {
		if f.redeemID != redeemID {
			s.mu.Unlock()
			return resetRequestFromRecord(r), ErrConflict
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return ResetCreditRequest{}, ctx.Err()
		case <-f.done:
		}
		return cloneResetCreditResult(f.result), f.err
	}
	if r.State == resetStateConsuming {
		// A completed POST whose final vault save failed has no active worker.
		// The durable consuming marker must never become another network call.
		s.mu.Unlock()
		return resetRequestFromRecord(r), ErrResetUnknown
	}
	next := cloneVault(s.vault)
	r.State = resetStateConsuming
	next.ResetRedemptions[id] = r
	if err := writeVault(s.path, s.protector, next); err != nil {
		s.mu.Unlock()
		return ResetCreditRequest{}, err
	}
	s.vault = next
	workCtx, cancel := context.WithTimeout(context.Background(), s.resetTimeout)
	f := &resetCreditFlight{done: make(chan struct{}), cancel: cancel, identityEpoch: c.IdentityEpoch, redeemID: redeemID}
	s.resetFlights[id] = f
	s.mu.Unlock()
	go s.runResetCredit(workCtx, id, c, f)
	select {
	case <-ctx.Done():
		return ResetCreditRequest{}, ctx.Err()
	case <-f.done:
	}
	return cloneResetCreditResult(f.result), f.err
}

func cloneResetCreditResult(result ResetCreditRequest) ResetCreditRequest {
	if result.CompletedAt != nil {
		completed := *result.CompletedAt
		result.CompletedAt = &completed
	}
	return result
}

func (s *Service) runResetCredit(ctx context.Context, id string, old credential, f *resetCreditFlight) {
	defer f.cancel()
	err := s.resetConsumer.ConsumeResetCredit(ctx, old.sdk(), f.redeemID)
	if errors.Is(err, ErrNeedsLogin) {
		attempt := &usageAttempt{epoch: old.Epoch, identityEpoch: old.IdentityEpoch}
		if refreshed := s.rotateUsageCredentialAfterUnauthorized(ctx, id, *attempt); refreshed == nil {
			s.mu.Lock()
			latest, ok := s.vault.Accounts[id]
			active := ok && latest.IdentityEpoch == f.identityEpoch && s.resetFlights[id] == f && !s.closed && ctx.Err() == nil
			s.mu.Unlock()
			if active {
				err = s.resetConsumer.ConsumeResetCredit(ctx, latest.sdk(), f.redeemID)
			} else {
				err = ErrConflict
			}
		} else {
			err = refreshed
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer close(f.done)
	current, exists := s.vault.Accounts[id]
	redemption, redOk := s.vault.ResetRedemptions[id]
	if s.closed || !exists || !redOk || current.IdentityEpoch != f.identityEpoch || redemption.ID != f.redeemID || s.resetFlights[id] != f {
		f.err = ErrConflict
		if s.resetFlights[id] == f {
			delete(s.resetFlights, id)
		}
		return
	}
	delete(s.resetFlights, id)
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err == nil {
		redemption.State = resetStateSucceeded
	} else if ResetCreditErrorCode(err) == "result_unknown" {
		redemption.State = resetStateUnknown
	} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		redemption.State = resetStateUnknown
	} else {
		redemption.State = resetStateFailed
	}
	now := s.now().UTC()
	redemption.CompletedAt = &now
	next := cloneVault(s.vault)
	next.ResetRedemptions[id] = redemption
	if saveErr := writeVault(s.path, s.protector, next); saveErr != nil {
		f.result = resetRequestFromRecord(s.vault.ResetRedemptions[id])
		f.err = ErrSaveFailed
		return
	}
	s.vault = next
	f.result = resetRequestFromRecord(redemption)
	f.err = err
	if err != nil && errors.Is(err, ErrNeedsLogin) {
		f.err = ErrNeedsLogin
	}
}
