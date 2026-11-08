package codexaccounts

import (
	"context"
	"errors"
	"strings"
	"time"
)

type usageEntry struct {
	snapshot      UsageSnapshot
	epoch         uint64
	identityEpoch uint64
	// A first 401 with a refresh credential is only intermediate evidence.
	// This bit is scoped to this exact observation and credential version.
	finalUnauthorized bool
}

type usageFlight struct {
	done                    chan struct{}
	cancel                  context.CancelFunc
	epoch                   uint64
	identityEpoch           uint64
	snapshot                UsageSnapshot
	err                     error
	cancelled               bool
	fetchStartedAt          time.Time // guarded by Service.mu until done closes
	afterCredentialRecovery bool      // guarded by Service.mu until done closes
	background              *usageRefreshJob
}

type usageAttempt struct {
	epoch          uint64
	identityEpoch  uint64
	fetchStartedAt time.Time
}

// PoolQuotaStatus is a current, non-sensitive routing observation. Fresh=false
// means a quota reserve cannot be enforced and pool routing must fail closed.
type PoolQuotaStatus struct {
	Fresh            bool
	Allowed          bool
	RemainingPercent float64
}

// PoolRoutingStatus is a memory-only, non-sensitive scheduling observation.
// Quota and plan are usable only when their Fresh flags are true. Subscription
// metadata is an unverified import/JWT hint and is exposed here only for stable
// ordering; it never grants access or makes an account eligible.
type PoolRoutingStatus struct {
	Current                 bool
	QuotaFresh              bool
	QuotaAllowed            bool
	RemainingPercent        float64
	PlanFresh               bool
	PlanFamily              string
	SubscriptionKnown       bool
	SubscriptionSource      string
	SubscriptionObservedAt  *time.Time
	SubscriptionActiveUntil *time.Time
}

func (s *Service) accountLocked(c credential) Account {
	account := c.account(s.now())
	account.Subscription = s.subscriptionLocked(c)
	account.Cleanup = s.cleanupStatusLocked(c)
	entry, ok := s.usage[c.ID]
	if !ok || entry.epoch != c.Epoch || entry.identityEpoch != c.IdentityEpoch {
		return account
	}
	usage := cloneUsageSnapshot(entry.snapshot)
	if usage.Status == UsageStatusFresh && (usage.FreshUntil == nil || !s.now().Before(*usage.FreshUntil)) {
		usage.Status = UsageStatusStale
	}
	account.Usage = usage
	return account
}

// Usage returns the last sanitized provider observation without doing network
// work. An observation becomes stale according to the service clock.
func (s *Service) Usage(id string) (UsageSnapshot, error) {
	return s.usageForIdentity(id, 0)
}

func (s *Service) usageForIdentity(id string, identityEpoch uint64) (UsageSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return UsageSnapshot{}, ErrClosed
	}
	c, ok := s.vault.Accounts[id]
	if !ok {
		return UsageSnapshot{}, ErrNotFound
	}
	if identityEpoch != 0 && identityEpoch != c.IdentityEpoch {
		return UsageSnapshot{}, ErrConflict
	}
	return s.accountLocked(c).Usage, nil
}

// ResetUsage discards the locally cached usage observation for one account.
// It deliberately does not call the provider or attempt to reset upstream
// credits (the official API has no supported client-side credit reset). The
// next explicit or enabled background refresh fetches a new observation under
// the current credential epoch.
func (s *Service) ResetUsage(id string) (UsageSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return UsageSnapshot{}, ErrClosed
	}
	c, ok := s.vault.Accounts[id]
	if !ok {
		return UsageSnapshot{}, ErrNotFound
	}
	s.fenceUsageLocked(id)
	return s.accountLocked(c).Usage, nil
}

// RefreshUsage explicitly reads the provider's account usage endpoint. Calls
// for one credential epoch are coalesced, while deletion, replacement, login,
// token rotation and service close all fence the result before publication.
func (s *Service) RefreshUsage(ctx context.Context, id string) (UsageSnapshot, error) {
	return s.refreshUsageForIdentity(ctx, id, 0)
}

// RefreshUsageForIdentity rejects a delayed UI action after replacement. The
// identity stays pinned through expiry recovery and a single 401 retry.
func (s *Service) RefreshUsageForIdentity(ctx context.Context, id string, identityEpoch uint64) (UsageSnapshot, error) {
	if identityEpoch == 0 {
		return UsageSnapshot{}, ErrConflict
	}
	return s.refreshUsageForIdentity(ctx, id, identityEpoch)
}

func (s *Service) refreshUsageForIdentity(ctx context.Context, id string, identityEpoch uint64) (UsageSnapshot, error) {
	// Include the gap between the first 401 and refresh/retry in cleanup's
	// busy scope; the intermediate unauthorized observation is not a final
	// failure while credential recovery is still in progress.
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return UsageSnapshot{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return UsageSnapshot{}, err
	}
	c, ok := s.vault.Accounts[id]
	if !ok {
		s.mu.Unlock()
		return UsageSnapshot{}, ErrNotFound
	}
	if identityEpoch != 0 && identityEpoch != c.IdentityEpoch {
		s.mu.Unlock()
		return UsageSnapshot{}, ErrConflict
	}
	identityEpoch = c.IdentityEpoch
	if !s.backgroundUsageCurrentLocked(ctx, c) {
		s.mu.Unlock()
		return UsageSnapshot{}, ErrConflict
	}
	if s.cleanupUsageWork == nil {
		s.cleanupUsageWork = make(map[string]int)
	}
	s.cleanupUsageWork[id]++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.cleanupUsageWork[id]--
		if s.cleanupUsageWork[id] == 0 {
			delete(s.cleanupUsageWork, id)
		}
		s.mu.Unlock()
	}()
	if snapshot, refreshed, err := s.refreshExpiredUsageCredential(ctx, id, identityEpoch); err != nil {
		return snapshot, err
	} else if refreshed {
		// Refresh publishes a new credential epoch. Capture it only after the
		// durable rotation completed; no usage request may use the expired token.
		if err := ctx.Err(); err != nil {
			return UsageSnapshot{}, err
		}
	}

	snapshot, attempt, err := s.refreshUsageOnceForIdentity(ctx, id, identityEpoch, nil)
	if !errors.Is(err, ErrNeedsLogin) {
		return snapshot, err
	}
	if err = s.rotateUsageCredentialAfterUnauthorized(ctx, id, attempt); err != nil {
		if current, currentErr := s.usageForIdentity(id, identityEpoch); currentErr == nil {
			return current, err
		}
		return snapshot, err
	}
	// Retry the provider read exactly once. A second 401 remains needs_login;
	// it cannot recurse into an unbounded refresh loop.
	snapshot, _, err = s.refreshUsageOnceForIdentity(ctx, id, identityEpoch, &attempt)
	return snapshot, err
}

// refreshExpiredUsageCredential keeps explicit expiry handling under the
// existing credential refresh owner. It does not inspect native Codex files.
func (s *Service) refreshExpiredUsageCredential(ctx context.Context, id string, identityEpoch uint64) (UsageSnapshot, bool, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return UsageSnapshot{}, false, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return UsageSnapshot{}, false, err
	}
	c, ok := s.vault.Accounts[id]
	if !ok {
		s.mu.Unlock()
		return UsageSnapshot{}, false, ErrNotFound
	}
	if c.IdentityEpoch != identityEpoch {
		s.mu.Unlock()
		return UsageSnapshot{}, false, ErrConflict
	}
	expired := !c.ExpiresAt.IsZero() && !s.now().Before(c.ExpiresAt)
	attempt := usageAttempt{epoch: c.Epoch, identityEpoch: c.IdentityEpoch}
	current := s.accountLocked(c).Usage
	s.mu.Unlock()
	if !expired {
		return current, false, nil
	}
	account, err := s.refreshCredential(ctx, id, &attempt)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			s.mu.Lock()
			latest, ok := s.vault.Accounts[id]
			sameIdentityAdvanced := ok && latest.IdentityEpoch == attempt.identityEpoch && latest.Epoch != attempt.epoch
			s.mu.Unlock()
			if sameIdentityAdvanced {
				return current, true, nil
			}
		}
		if latest, usageErr := s.usageForIdentity(id, identityEpoch); usageErr == nil {
			current = latest
		}
		return current, false, err
	}
	if account.IdentityEpoch != attempt.identityEpoch {
		return current, false, ErrConflict
	}
	return account.Usage, true, nil
}

// rotateUsageCredentialAfterUnauthorized refreshes only when the failed
// usage request still names the current credential. If another owner already
// rotated it, the caller simply retries with that newer durable epoch.
func (s *Service) rotateUsageCredentialAfterUnauthorized(ctx context.Context, id string, attempt usageAttempt) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	current, ok := s.vault.Accounts[id]
	if !ok {
		s.mu.Unlock()
		return ErrNotFound
	}
	if current.IdentityEpoch != attempt.identityEpoch {
		s.mu.Unlock()
		return ErrConflict
	}
	needsRefresh := current.Epoch == attempt.epoch
	s.mu.Unlock()
	if !needsRefresh {
		return nil
	}
	account, err := s.refreshCredential(ctx, id, &attempt)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			s.mu.Lock()
			latest, ok := s.vault.Accounts[id]
			sameIdentityAdvanced := ok && latest.IdentityEpoch == attempt.identityEpoch && latest.Epoch != attempt.epoch
			s.mu.Unlock()
			if sameIdentityAdvanced {
				return nil
			}
		}
		return err
	}
	if account.IdentityEpoch != attempt.identityEpoch {
		return ErrConflict
	}
	return nil
}

func (s *Service) refreshUsageOnce(ctx context.Context, id string, retry *usageAttempt) (UsageSnapshot, usageAttempt, error) {
	return s.refreshUsageOnceForIdentity(ctx, id, 0, retry)
}

func (s *Service) refreshUsageOnceForIdentity(ctx context.Context, id string, identityEpoch uint64, retry *usageAttempt) (UsageSnapshot, usageAttempt, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return UsageSnapshot{}, usageAttempt{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return UsageSnapshot{}, usageAttempt{}, err
	}
	c, ok := s.vault.Accounts[id]
	if !ok {
		s.mu.Unlock()
		return UsageSnapshot{}, usageAttempt{}, ErrNotFound
	}
	attempt := usageAttempt{epoch: c.Epoch, identityEpoch: c.IdentityEpoch}
	if identityEpoch != 0 && c.IdentityEpoch != identityEpoch {
		s.mu.Unlock()
		return UsageSnapshot{}, attempt, ErrConflict
	}
	if !s.backgroundUsageCurrentLocked(ctx, c) {
		s.mu.Unlock()
		return UsageSnapshot{}, attempt, ErrConflict
	}
	if retry != nil && retry.identityEpoch != c.IdentityEpoch {
		s.mu.Unlock()
		return UsageSnapshot{}, attempt, ErrConflict
	}
	for _, login := range s.logins {
		if login.session.Status == "pending" && login.request.ReplaceID == id {
			usage := s.accountLocked(c).Usage
			s.mu.Unlock()
			return usage, attempt, ErrConflict
		}
	}
	if s.pending[id].epoch != 0 {
		usage := s.accountLocked(c).Usage
		s.mu.Unlock()
		return usage, attempt, ErrSaveFailed
	}
	if s.usageFetcher == nil {
		usage := s.accountLocked(c).Usage
		s.mu.Unlock()
		return usage, attempt, ErrUsageUnavailable
	}
	f := s.usageFlights[id]
	if f != nil && f.cancelled {
		usage := s.accountLocked(c).Usage
		s.mu.Unlock()
		return usage, attempt, ErrConflict
	}
	if f == nil {
		s.bumpCleanupObservationLocked(id)
		fetchCtx, cancel := context.WithTimeout(context.Background(), s.usageTimeout)
		f = &usageFlight{done: make(chan struct{}), cancel: cancel, epoch: c.Epoch, identityEpoch: c.IdentityEpoch, background: backgroundUsageJob(ctx)}
		if f.background != nil {
			f.background.work = append(f.background.work, f.done)
		}
		s.usageFlights[id] = f
		go s.runUsageRefresh(fetchCtx, id, c, f)
	} else if backgroundUsageJob(ctx) == nil {
		f.background = nil
	}
	// A retry may join a newer credential's already-running observation. Keep
	// the earliest local start for every waiter and the shared cached result.
	if retry != nil {
		f.afterCredentialRecovery = true
	}
	if retry != nil && !retry.fetchStartedAt.IsZero() && (f.fetchStartedAt.IsZero() || retry.fetchStartedAt.Before(f.fetchStartedAt)) {
		f.fetchStartedAt = retry.fetchStartedAt
	}
	s.mu.Unlock()

	select {
	case <-ctx.Done():
		return UsageSnapshot{}, attempt, ctx.Err()
	case <-f.done:
		return cloneUsageSnapshot(f.snapshot), usageAttempt{epoch: f.epoch, identityEpoch: f.identityEpoch, fetchStartedAt: f.fetchStartedAt}, f.err
	}
}

func (s *Service) runUsageRefresh(ctx context.Context, id string, old credential, f *usageFlight) {
	defer f.cancel()
	s.mu.Lock()
	startedAt := s.now().UTC()
	if f.fetchStartedAt.IsZero() || startedAt.Before(f.fetchStartedAt) {
		f.fetchStartedAt = startedAt
	}
	s.mu.Unlock()
	result, err := s.usageFetcher.FetchUsage(ctx, old.sdk())
	s.mu.Lock()
	defer s.mu.Unlock()
	defer close(f.done)
	defer s.wakeUsageRefreshLocked()
	current, exists := s.vault.Accounts[id]
	if s.closed || !exists || current.Epoch != f.epoch || current.IdentityEpoch != f.identityEpoch || s.usageFlights[id] != f || f.cancelled {
		f.snapshot = unverifiedUsage(old.Plan)
		f.err = ErrConflict
		if s.usageFlights[id] == f {
			delete(s.usageFlights, id)
		}
		return
	}
	delete(s.usageFlights, id)
	s.bumpCleanupObservationLocked(id)
	// Once the shared operation itself timed out/cancelled, a connector's
	// late response is inconclusive even if it happens to report an auth
	// rejection. Never convert a network timeout into cleanup evidence.
	// This is distinct from a cancelled caller: other coalesced waiters may
	// still receive the shared operation's timely result.
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err == nil && old.AccountID != "" && result.ProviderAccountID != "" && old.AccountID != result.ProviderAccountID {
		err = usageError("identity_mismatch", ErrConflict)
	}
	now := s.now().UTC()
	if err != nil {
		f.snapshot = s.failedUsageLocked(current, err, now)
		finalUnauthorized := f.snapshot.ErrorCode == "needs_login" && (old.RefreshToken == "" || old.TokenSourceMode == "chatgpt_web_session" || f.afterCredentialRecovery)
		s.usage[id] = usageEntry{snapshot: cloneUsageSnapshot(f.snapshot), epoch: current.Epoch, identityEpoch: current.IdentityEpoch, finalUnauthorized: finalUnauthorized}
		if errors.Is(err, ErrConflict) {
			f.err = ErrConflict
		} else if errors.Is(err, ErrNeedsLogin) {
			f.err = ErrNeedsLogin
		} else {
			f.err = ErrUsageUnavailable
		}
		return
	}

	snapshot := result.Snapshot
	rawPlan, family := normalizePlan(snapshot.Plan.Raw)
	snapshot.Status = UsageStatusFresh
	snapshot.Verified = true
	snapshot.Source = UsageSourceOfficial
	snapshot.Plan = UsagePlan{Raw: rawPlan, Family: family, Source: UsageSourceOfficial, Verified: true}
	snapshot.ErrorCode = ""
	snapshot.ObservedAt = timePtr(now)
	// Never trust fetcher/provider timestamps to narrow the actual local fetch
	// interval. Failed refreshes preserve the previous observation's interval.
	snapshot.FetchStartedAt = timePtr(f.fetchStartedAt)
	snapshot.LastAttemptAt = timePtr(now)
	freshUntil := usageFreshUntil(snapshot, now, s.usageFreshFor)
	snapshot.FreshUntil = &freshUntil
	if !now.Before(freshUntil) {
		snapshot.Status = UsageStatusStale
	}
	f.snapshot = cloneUsageSnapshot(snapshot)
	s.usage[id] = usageEntry{snapshot: cloneUsageSnapshot(snapshot), epoch: current.Epoch, identityEpoch: current.IdentityEpoch}
}

func (s *Service) failedUsageLocked(current credential, err error, now time.Time) UsageSnapshot {
	snapshot := unverifiedUsage(current.Plan)
	if entry, ok := s.usage[current.ID]; ok && entry.epoch == current.Epoch && entry.identityEpoch == current.IdentityEpoch {
		snapshot = cloneUsageSnapshot(entry.snapshot)
	}
	snapshot.LastAttemptAt = timePtr(now)
	snapshot.ErrorCode = usageErrorCode(err)
	if snapshot.Verified {
		snapshot.Status = UsageStatusStale
	} else {
		snapshot.Status = UsageStatusError
		snapshot.Source = UsageSourceOfficial
	}
	return snapshot
}

func (s *Service) fenceUsageLocked(id string) {
	s.bumpCleanupObservationLocked(id)
	if f := s.usageFlights[id]; f != nil {
		f.cancelled = true
		f.cancel()
		delete(s.usageFlights, id)
	}
	delete(s.usage, id)
	if r := s.usageRefresh; r != nil {
		if state := r.targets[id]; state != nil {
			state.next, state.observed, state.freshUntil = time.Time{}, time.Time{}, time.Time{}
			if state.job == nil {
				state.failures = 0
			}
		}
	}
	s.wakeUsageRefreshLocked()
}

// PoolQuota reads only the in-memory verified snapshot. It never triggers a
// network request, so AccountPool can safely call it during selection.
func (s *Service) PoolQuota(id string, identityEpoch uint64, model string) PoolQuotaStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return PoolQuotaStatus{}
	}
	c, ok := s.vault.Accounts[id]
	if !ok || c.IdentityEpoch != identityEpoch {
		return PoolQuotaStatus{}
	}
	if !s.poolMetricsAvailableLocked(c) {
		return PoolQuotaStatus{}
	}
	return s.poolQuotaLocked(c, model)
}

// PoolRouting reads the same current credential and verified usage cache as
// PoolQuota. It performs no network request and validates both the public
// identity epoch and the private credential epoch before publishing metrics.
func (s *Service) PoolRouting(id string, identityEpoch uint64, model string) PoolRoutingStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return PoolRoutingStatus{}
	}
	c, ok := s.vault.Accounts[id]
	if !ok || c.IdentityEpoch != identityEpoch {
		return PoolRoutingStatus{}
	}
	result := PoolRoutingStatus{Current: true}
	if !c.SubscriptionActiveUntil.IsZero() && (c.SubscriptionSource == SubscriptionSourceImportHint || c.SubscriptionSource == SubscriptionSourceJWTHint) {
		expiry := c.SubscriptionActiveUntil.UTC()
		result.SubscriptionKnown = true
		result.SubscriptionSource = c.SubscriptionSource
		result.SubscriptionActiveUntil = &expiry
		if !c.SubscriptionObservedAt.IsZero() {
			observed := c.SubscriptionObservedAt.UTC()
			result.SubscriptionObservedAt = &observed
		}
	}
	if !s.poolMetricsAvailableLocked(c) {
		return result
	}
	entry, ok := s.usage[id]
	if !ok || entry.epoch != c.Epoch || entry.identityEpoch != c.IdentityEpoch {
		return result
	}
	snapshot := entry.snapshot
	if snapshot.Status != UsageStatusFresh || !snapshot.Verified || snapshot.FreshUntil == nil || !s.now().Before(*snapshot.FreshUntil) {
		return result
	}
	if snapshot.Plan.Verified && snapshot.Plan.Source == UsageSourceOfficial {
		result.PlanFresh = true
		result.PlanFamily = snapshot.Plan.Family
	}
	quota := poolQuotaFromSnapshot(snapshot, model)
	result.QuotaFresh = quota.Fresh
	result.QuotaAllowed = quota.Allowed
	result.RemainingPercent = quota.RemainingPercent
	return result
}

// poolMetricsAvailableLocked keeps verified provider observations from being
// advertised as executable routing facts while the credential owner is in an
// unresolved state. The sanitized subscription hint remains display/sort
// metadata, but quota reserves and verified-plan ordering must wait for a
// current, saved credential and a new observation for that credential epoch.
func (s *Service) poolMetricsAvailableLocked(c credential) bool {
	if c.Status != "" && c.Status != StatusReady {
		return false
	}
	if c.AccessToken == "" || !c.ExpiresAt.IsZero() && !s.now().Before(c.ExpiresAt) {
		return false
	}
	if s.pending[c.ID].epoch != 0 {
		return false
	}
	for _, login := range s.logins {
		if login.session.Status == "pending" && login.request.ReplaceID == c.ID {
			return false
		}
	}
	return true
}

func (s *Service) poolQuotaLocked(c credential, model string) PoolQuotaStatus {
	entry, ok := s.usage[c.ID]
	if !ok || entry.epoch != c.Epoch || entry.identityEpoch != c.IdentityEpoch {
		return PoolQuotaStatus{}
	}
	snapshot := entry.snapshot
	if snapshot.Status != UsageStatusFresh || !snapshot.Verified || snapshot.FreshUntil == nil || !s.now().Before(*snapshot.FreshUntil) {
		return PoolQuotaStatus{}
	}
	return poolQuotaFromSnapshot(snapshot, model)
}

func poolQuotaFromSnapshot(snapshot UsageSnapshot, model string) PoolQuotaStatus {
	if snapshot.RateLimit == nil {
		return PoolQuotaStatus{}
	}
	// The provider may return more model-specific limits than the public DTO
	// budget. A hidden entry could be the requested model's tighter budget, so a
	// reserve decision cannot treat the base window as complete.
	if snapshot.AdditionalLimitsTruncated {
		return PoolQuotaStatus{}
	}
	remaining, found, allowed := 0.0, false, true
	if !constrainPoolQuota(snapshot.RateLimit, &remaining, &found, &allowed) {
		return PoolQuotaStatus{}
	}
	model = strings.TrimSpace(model)
	if model != "" {
		for i := range snapshot.AdditionalRateLimits {
			limit := &snapshot.AdditionalRateLimits[i]
			if limit.NormalModelSlug == model && !constrainPoolQuota(limit, &remaining, &found, &allowed) {
				return PoolQuotaStatus{}
			}
		}
	}
	if snapshot.SpendControl != nil && snapshot.SpendControl.Reached != nil && *snapshot.SpendControl.Reached {
		allowed = false
	}
	if !found {
		return PoolQuotaStatus{}
	}
	return PoolQuotaStatus{Fresh: true, Allowed: allowed, RemainingPercent: remaining}
}

func constrainPoolQuota(limit *UsageLimit, remaining *float64, found *bool, allowed *bool) bool {
	if limit == nil {
		return false
	}
	if limit.Allowed != nil && !*limit.Allowed || limit.LimitReached != nil && *limit.LimitReached {
		*allowed = false
	}
	windowFound := false
	for _, window := range []*UsageWindow{limit.Primary, limit.Secondary} {
		if window == nil || window.RemainingPercent == nil {
			continue
		}
		windowFound = true
		if !*found || *window.RemainingPercent < *remaining {
			*remaining = *window.RemainingPercent
			*found = true
		}
	}
	return windowFound
}

func usageFreshUntil(snapshot UsageSnapshot, now time.Time, ttl time.Duration) time.Time {
	freshUntil := now.Add(ttl)
	consider := func(reset *time.Time) {
		if reset != nil && reset.Before(freshUntil) {
			freshUntil = reset.UTC()
		}
	}
	considerLimit := func(limit *UsageLimit) {
		if limit == nil {
			return
		}
		for _, window := range []*UsageWindow{limit.Primary, limit.Secondary} {
			if window != nil {
				consider(window.ResetsAt)
			}
		}
	}
	considerLimit(snapshot.RateLimit)
	considerLimit(snapshot.CodeReviewRateLimit)
	for i := range snapshot.AdditionalRateLimits {
		considerLimit(&snapshot.AdditionalRateLimits[i])
	}
	if snapshot.SpendControl != nil && snapshot.SpendControl.IndividualLimit != nil {
		consider(snapshot.SpendControl.IndividualLimit.ResetsAt)
	}
	return freshUntil
}

func cloneUsageSnapshot(in UsageSnapshot) UsageSnapshot {
	out := in
	cloneTime := func(value *time.Time) *time.Time {
		if value == nil {
			return nil
		}
		copy := *value
		return &copy
	}
	cloneFloat := func(value *float64) *float64 {
		if value == nil {
			return nil
		}
		copy := *value
		return &copy
	}
	cloneInt := func(value *int64) *int64 {
		if value == nil {
			return nil
		}
		copy := *value
		return &copy
	}
	cloneBool := func(value *bool) *bool {
		if value == nil {
			return nil
		}
		copy := *value
		return &copy
	}
	cloneWindow := func(value *UsageWindow) *UsageWindow {
		if value == nil {
			return nil
		}
		copy := *value
		copy.UsedPercent = cloneFloat(value.UsedPercent)
		copy.RemainingPercent = cloneFloat(value.RemainingPercent)
		copy.WindowSeconds = cloneInt(value.WindowSeconds)
		copy.WindowMinutes = cloneInt(value.WindowMinutes)
		copy.ResetAfterSeconds = cloneInt(value.ResetAfterSeconds)
		copy.ResetsAt = cloneTime(value.ResetsAt)
		return &copy
	}
	cloneLimit := func(value *UsageLimit) *UsageLimit {
		if value == nil {
			return nil
		}
		copy := *value
		copy.Allowed = cloneBool(value.Allowed)
		copy.LimitReached = cloneBool(value.LimitReached)
		copy.Primary = cloneWindow(value.Primary)
		copy.Secondary = cloneWindow(value.Secondary)
		return &copy
	}
	out.ObservedAt = cloneTime(in.ObservedAt)
	out.FetchStartedAt = cloneTime(in.FetchStartedAt)
	out.FreshUntil = cloneTime(in.FreshUntil)
	out.LastAttemptAt = cloneTime(in.LastAttemptAt)
	out.RateLimit = cloneLimit(in.RateLimit)
	out.CodeReviewRateLimit = cloneLimit(in.CodeReviewRateLimit)
	out.AdditionalRateLimits = make([]UsageLimit, len(in.AdditionalRateLimits))
	for i := range in.AdditionalRateLimits {
		out.AdditionalRateLimits[i] = *cloneLimit(&in.AdditionalRateLimits[i])
	}
	if in.Credits != nil {
		copy := *in.Credits
		copy.HasCredits = cloneBool(in.Credits.HasCredits)
		copy.Unlimited = cloneBool(in.Credits.Unlimited)
		out.Credits = &copy
	}
	if in.SpendControl != nil {
		copy := *in.SpendControl
		copy.Reached = cloneBool(in.SpendControl.Reached)
		if in.SpendControl.IndividualLimit != nil {
			individual := *in.SpendControl.IndividualLimit
			individual.UsedPercent = cloneFloat(individual.UsedPercent)
			individual.RemainingPercent = cloneFloat(individual.RemainingPercent)
			individual.ResetAfterSeconds = cloneInt(individual.ResetAfterSeconds)
			individual.ResetsAt = cloneTime(individual.ResetsAt)
			copy.IndividualLimit = &individual
		}
		out.SpendControl = &copy
	}
	out.ResetCreditsAvailable = cloneInt(in.ResetCreditsAvailable)
	return out
}
