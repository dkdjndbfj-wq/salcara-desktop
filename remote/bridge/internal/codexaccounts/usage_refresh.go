package codexaccounts

import (
	"context"
	"sort"
	"time"
)

// UsageRefreshTarget opts an actively authorized route into quota maintenance.
// IdentityEpoch is the public account identity, not the rotating token epoch.
// Merely importing an account never enables background provider requests.
type UsageRefreshTarget struct {
	ID            string
	IdentityEpoch uint64
}

const usageRefreshConcurrency = 2

type usageRefreshState struct {
	target     UsageRefreshTarget
	epoch      uint64
	next       time.Time
	failures   int
	job        *usageRefreshJob
	observed   time.Time
	freshUntil time.Time
}

// All scheduler state, including job.work and flight ownership, uses Service.mu.
type usageRefreshScheduler struct {
	wake    chan struct{}
	done    chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
	targets map[string]*usageRefreshState
	jobs    map[*usageRefreshJob]struct{}
}

type usageRefreshJob struct {
	target UsageRefreshTarget
	epoch  uint64 // latest credential observed by this recovery chain
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	work   []<-chan struct{}
}

type usageRefreshContextKey struct{}

func backgroundUsageJob(ctx context.Context) *usageRefreshJob {
	job, _ := ctx.Value(usageRefreshContextKey{}).(*usageRefreshJob)
	return job
}

// SetUsageRefreshTargets replaces the explicitly enabled set. Removed accounts
// cancel background-only quota reads; a manual waiter that joined the same
// flight keeps its existing singleflight semantics. Already-issued token
// rotations finish saving under their own timeout. This method does no I/O.
func (s *Service) SetUsageRefreshTargets(targets []UsageRefreshTarget) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	desired := make(map[string]UsageRefreshTarget, len(targets))
	for _, target := range targets {
		c, ok := s.vault.Accounts[target.ID]
		if ok && target.IdentityEpoch != 0 && c.IdentityEpoch == target.IdentityEpoch && c.TokenSourceMode != "chatgpt_web_session" {
			desired[target.ID] = target
		}
	}
	r := s.usageRefresh
	if r == nil {
		if len(desired) == 0 {
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		r = &usageRefreshScheduler{wake: make(chan struct{}, 1), done: make(chan struct{}), ctx: ctx, cancel: cancel,
			targets: make(map[string]*usageRefreshState), jobs: make(map[*usageRefreshJob]struct{})}
		s.usageRefresh = r
		go s.runUsageScheduler(r)
	}
	for id, state := range r.targets {
		if target, ok := desired[id]; !ok || target != state.target {
			s.cancelUsageJobLocked(state.job)
			delete(r.targets, id)
		}
	}
	for id, target := range desired {
		if r.targets[id] == nil {
			r.targets[id] = &usageRefreshState{target: target, epoch: s.vault.Accounts[id].Epoch}
		}
	}
	s.wakeUsageRefreshLocked()
}

func (s *Service) wakeUsageRefreshLocked() {
	if r := s.usageRefresh; r != nil {
		select {
		case r.wake <- struct{}{}:
		default:
		}
	}
}

func (s *Service) cancelUsageJobLocked(job *usageRefreshJob) {
	if job == nil {
		return
	}
	job.cancel()
	if f := s.usageFlights[job.target.ID]; f != nil && f.background == job {
		f.cancelled = true
		f.cancel()
	}
	// Do not abandon a token rotation already sent to the provider: it may
	// have consumed the old refresh token. Keep its independent timeout and
	// credential/identity fence so a timely replacement is durably saved.
	// Delete, replacement and Service.Close retain their existing cancellation.
}

// Keep a request's complete refresh/401/retry chain within its selected identity.
func (s *Service) backgroundUsageCurrentLocked(ctx context.Context, c credential) bool {
	job := backgroundUsageJob(ctx)
	if job == nil {
		return true
	}
	r := s.usageRefresh
	if r == nil || ctx.Err() != nil || job.target.ID != c.ID || job.target.IdentityEpoch != c.IdentityEpoch {
		return false
	}
	state := r.targets[c.ID]
	if state == nil || state.target != job.target || state.job != job {
		return false
	}
	job.epoch = c.Epoch
	return true
}

func (s *Service) runUsageScheduler(r *usageRefreshScheduler) {
	defer close(r.done)
	for {
		s.mu.Lock()
		if s.closed || r.ctx.Err() != nil {
			s.mu.Unlock()
			return
		}
		now := s.now()
		var pending []*usageRefreshState
		for id, state := range r.targets {
			c, ok := s.vault.Accounts[id]
			if !ok || c.IdentityEpoch != state.target.IdentityEpoch {
				s.cancelUsageJobLocked(state.job)
				delete(r.targets, id)
				continue
			}
			if state.job != nil {
				continue
			}
			if state.epoch != c.Epoch {
				state.epoch, state.failures, state.next = c.Epoch, 0, time.Time{}
			}
			// A manual refresh may already have supplied a newer observation.
			if entry, ok := s.usage[id]; ok && entry.epoch == c.Epoch && entry.identityEpoch == c.IdentityEpoch {
				if due, ok := s.nextUsageRefresh(entry.snapshot); ok && (!state.observed.Equal(*entry.snapshot.ObservedAt) || !state.freshUntil.Equal(*entry.snapshot.FreshUntil)) {
					state.next = due
					state.observed, state.freshUntil = *entry.snapshot.ObservedAt, *entry.snapshot.FreshUntil
				}
			}
			pending = append(pending, state)
		}
		sort.Slice(pending, func(i, j int) bool {
			if pending[i].next.Equal(pending[j].next) {
				return pending[i].target.ID < pending[j].target.ID
			}
			return pending[i].next.Before(pending[j].next)
		})
		var next time.Time
		for _, state := range pending {
			if len(r.jobs) >= usageRefreshConcurrency {
				break // Completion will wake the scheduler; avoid a busy timer.
			}
			if state.next.After(now) {
				next = state.next
				break
			}
			// The underlying credential and usage flights each retain their own
			// shorter timeout. This bounds the full recovery chain as well.
			ctx, cancel := context.WithTimeout(r.ctx, s.timeout+2*s.usageTimeout)
			job := &usageRefreshJob{target: state.target, epoch: state.epoch, ctx: ctx, cancel: cancel, done: make(chan struct{})}
			job.ctx = context.WithValue(ctx, usageRefreshContextKey{}, job)
			state.job = job
			r.jobs[job] = struct{}{}
			go s.runBackgroundUsage(r, state, job)
		}
		var timer *time.Timer
		var tick <-chan time.Time
		if !next.IsZero() {
			timer = time.NewTimer(next.Sub(now))
			tick = timer.C
		}
		s.mu.Unlock()
		select {
		case <-r.ctx.Done():
		case <-r.wake:
		case <-tick:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

func (s *Service) runBackgroundUsage(r *usageRefreshScheduler, state *usageRefreshState, job *usageRefreshJob) {
	defer close(job.done)
	snapshot, err := s.RefreshUsage(job.ctx, job.target.ID)
	s.mu.Lock()
	// A stopped waiter must not leave an unowned quota request running.
	// Manual joiners promote a flight out of this background cancellation scope.
	s.cancelUsageJobLocked(job)
	work := append([]<-chan struct{}(nil), job.work...)
	s.mu.Unlock()
	// Count cancelled-but-unwinding provider requests toward the concurrency
	// limit too. A connector that ignores cancellation cannot create a storm
	// when an account is rapidly disabled and re-enabled.
	for _, done := range work {
		<-done
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(r.jobs, job)
	if current := r.targets[job.target.ID]; current == state && state.job == job {
		state.job = nil
		c, exists := s.vault.Accounts[job.target.ID]
		if exists && c.IdentityEpoch == job.target.IdentityEpoch {
			// Rotation inside RefreshUsage is already covered by its new result.
			// A later external rotation must instead schedule an immediate read.
			entry, observed := s.usage[job.target.ID]
			observed = observed && entry.epoch == c.Epoch && entry.identityEpoch == c.IdentityEpoch
			state.epoch = job.epoch
			if due, fresh := s.nextUsageRefresh(snapshot); err == nil && fresh && observed {
				state.failures, state.next = 0, due
			} else {
				state.failures++
				state.next = s.now().Add(s.usageRefreshBackoff(state.failures))
			}
		}
	}
	s.wakeUsageRefreshLocked()
}

func (s *Service) nextUsageRefresh(snapshot UsageSnapshot) (time.Time, bool) {
	if snapshot.Status != UsageStatusFresh || !snapshot.Verified || snapshot.ObservedAt == nil || snapshot.FreshUntil == nil || !snapshot.ObservedAt.Before(*snapshot.FreshUntil) {
		return time.Time{}, false
	}
	// Refresh ahead of the actual TTL/reset boundary, but avoid accelerating
	// into a tight loop as a provider window approaches its reset time.
	span := snapshot.FreshUntil.Sub(*snapshot.ObservedAt)
	delay := span * 3 / 4
	if minimum := s.usageRefreshMinimum(); delay < minimum {
		delay = minimum
	}
	if delay > span {
		// A close reset boundary must not acquire an artificial 15-second
		// stale gap. Read at that boundary once; an unchanged/past reset is
		// stale and follows failure backoff instead of a tight polling loop.
		delay = span
	}
	return snapshot.ObservedAt.Add(delay), true
}

func (s *Service) usageRefreshMinimum() time.Duration {
	minimum := s.usageFreshFor / 4
	if minimum > 15*time.Second {
		minimum = 15 * time.Second
	}
	if minimum < time.Millisecond {
		minimum = time.Millisecond
	}
	return minimum
}

func (s *Service) usageRefreshBackoff(failures int) time.Duration {
	delay := s.usageRefreshMinimum()
	for i := 1; i < failures && delay < s.usageFreshFor; i++ {
		delay *= 2
	}
	if delay > s.usageFreshFor {
		delay = s.usageFreshFor
	}
	return delay
}
