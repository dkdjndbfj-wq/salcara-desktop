// Package accounthealth tracks per-account request health using a sliding
// time window. It is deliberately free of I/O and external dependencies so it
// can be embedded directly in the selection hot-path.
//
// The Tracker is safe for concurrent use. All exported methods are
// non-blocking; there are no goroutines or background flushers.
//
// Usage pattern:
//
//	h := accounthealth.NewTracker()
//	// ... after a request completes:
//	h.Record(accountID, accounthealth.Result{Err: err, Status: httpStatus, Latency: d})
//	// ... before selecting an account:
//	score := h.Score(accountID)          // 0.0 (dead) – 1.0 (perfect)
//	cooldown := h.CooldownUntil(accountID) // zero time = available now
package accounthealth

import (
	"math"
	"sync"
	"time"
)

// ── error classification ────────────────────────────────────────────────────

// ErrorKind classifies an upstream failure into a stable category that the
// health tracker and the selection policy can reason about independently.
type ErrorKind int

const (
	// OK is not an error.
	OK ErrorKind = iota

	// RateLimit means the provider returned 429 or a Retry-After header.
	// Typical response: cool down this account and pick another.
	RateLimit

	// WindControlled means the account was flagged or suspended by the provider
	// (e.g. OpenAI code 20 / Anthropic overload). Longer cooldown, alert operator.
	WindControlled

	// ModelUnavailable means the requested model is not available on this account
	// right now (e.g. capacity / geographic restriction). Retry with a different
	// account or model, do not penalise the account's general health.
	ModelUnavailable

	// NetworkRetry means a transient network-layer failure (timeout, reset, TLS).
	// Short cooldown; the account itself may be fine.
	NetworkRetry

	// Fatal means any other 4xx/5xx that we cannot safely retry (bad credentials,
	// permission denied, account deleted). Disable selection until operator reviews.
	Fatal
)

func (k ErrorKind) String() string {
	switch k {
	case OK:
		return "ok"
	case RateLimit:
		return "rate_limit"
	case WindControlled:
		return "wind_controlled"
	case ModelUnavailable:
		return "model_unavailable"
	case NetworkRetry:
		return "network_retry"
	case Fatal:
		return "fatal"
	default:
		return "unknown"
	}
}

// cooldownDuration returns the minimum time to hold an account off the selection
// pool after an error of this kind. Callers may extend this based on retry counts.
func (k ErrorKind) cooldownDuration(retries int) time.Duration {
	base := map[ErrorKind]time.Duration{
		RateLimit:        30 * time.Second,
		WindControlled:   5 * time.Minute,
		NetworkRetry:     10 * time.Second,
		Fatal:            24 * time.Hour,
		ModelUnavailable: 0, // not a cooldown event
	}[k]
	if base == 0 {
		return 0
	}
	// Exponential back-off capped at 10× the base.
	mult := math.Pow(2, float64(retries))
	if mult > 10 {
		mult = 10
	}
	return time.Duration(float64(base) * mult)
}

// ── result type ─────────────────────────────────────────────────────────────

// Result is one completed request observation.
type Result struct {
	Kind    ErrorKind     // classified error kind (OK on success)
	Status  int           // HTTP response status (0 if unknown)
	Latency time.Duration // end-to-end request latency
	// RetryAfter is the value of a Retry-After header, if present.
	// Zero means the provider did not send one.
	RetryAfter time.Duration
	// Tokens is the total token count reported by the provider (prompt+completion).
	// Zero if unknown.
	Tokens int
}

// ── sliding window ───────────────────────────────────────────────────────────

const (
	windowSize      = 100             // keep last N observations
	windowDur       = 5 * time.Minute // observations older than this are ignored
	minObservations = 5               // need at least this many before penalising score
)

type observation struct {
	at   time.Time
	kind ErrorKind
	lat  time.Duration
}

// accountState is the per-account mutable state.
type accountState struct {
	mu          sync.Mutex
	ring        [windowSize]observation
	head        int // next write position
	count       int // total ever written (for "at least N" check)
	coolUntil   time.Time
	retries     int // consecutive errors of the same kind
	lastKind    ErrorKind
	totalTokens int64
}

func (s *accountState) record(r Result, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	obs := observation{at: now, kind: r.Kind, lat: r.Latency}
	s.ring[s.head%windowSize] = obs
	s.head++
	s.count++
	s.totalTokens += int64(r.Tokens)

	if r.Kind == OK {
		s.retries = 0
		s.lastKind = OK
		return
	}

	// Consecutive error counting for back-off.
	if r.Kind == s.lastKind {
		s.retries++
	} else {
		s.retries = 0
		s.lastKind = r.Kind
	}

	// Compute cooldown.
	d := r.Kind.cooldownDuration(s.retries)
	if r.RetryAfter > d {
		d = r.RetryAfter
	}
	if d > 0 {
		until := now.Add(d)
		if until.After(s.coolUntil) {
			s.coolUntil = until
		}
	}
}

// score returns a value in [0, 1] where 1 is perfect and 0 means "do not use".
// The score is derived from the error rate and mean latency of recent observations.
func (s *accountState) score(now time.Time) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.coolUntil.IsZero() && now.Before(s.coolUntil) {
		return 0
	}

	// Gather active observations.
	cutoff := now.Add(-windowDur)
	var total, errors int
	var totalLat time.Duration
	for i := 0; i < windowSize; i++ {
		o := s.ring[i]
		if o.at.IsZero() || o.at.Before(cutoff) {
			continue
		}
		total++
		if o.kind != OK && o.kind != ModelUnavailable {
			errors++
		}
		totalLat += o.lat
	}
	if total < minObservations {
		// Not enough data: assume healthy but slightly uncertain.
		return 0.85
	}
	errRate := float64(errors) / float64(total)
	// Latency penalty: p50 target 2s, penalise linearly above that, cap at 30s.
	meanLat := float64(totalLat) / float64(total)
	const targetLat = float64(2 * time.Second)
	const maxLat = float64(30 * time.Second)
	latPenalty := 0.0
	if meanLat > targetLat {
		latPenalty = 0.15 * math.Min(1.0, (meanLat-targetLat)/(maxLat-targetLat))
	}
	score := (1 - errRate) * (1 - latPenalty)
	if score < 0 {
		score = 0
	}
	return score
}

func (s *accountState) cooldownUntil() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.coolUntil
}

func (s *accountState) clearCooldown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.coolUntil = time.Time{}
	s.retries = 0
}

// ── Tracker ──────────────────────────────────────────────────────────────────

// Tracker maintains health state for a collection of accounts identified by
// string IDs. The zero value is not usable; call NewTracker.
type Tracker struct {
	mu       sync.RWMutex
	accounts map[string]*accountState
	scopes   map[string]string
}

// NewTracker returns a ready-to-use Tracker.
func NewTracker() *Tracker {
	return &Tracker{accounts: make(map[string]*accountState), scopes: make(map[string]string)}
}

// Bind retires old health after a credential is replaced. Scope is a digest,
// never a secret; callers reject late observations before binding a snapshot.
func (t *Tracker) Bind(id, scope string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.scopes == nil {
		t.scopes = make(map[string]string)
	}
	if previous, ok := t.scopes[id]; ok && previous != scope {
		delete(t.accounts, id)
	}
	t.scopes[id] = scope
}

func (t *Tracker) get(id string) *accountState {
	t.mu.RLock()
	s := t.accounts[id]
	t.mu.RUnlock()
	if s != nil {
		return s
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if s = t.accounts[id]; s != nil {
		return s
	}
	s = &accountState{}
	t.accounts[id] = s
	return s
}

// Record adds one completed request to the account's history.
func (t *Tracker) Record(accountID string, r Result) {
	t.get(accountID).record(r, time.Now())
}

// RecordFor rejects a late completion after selection has bound a replacement
// credential. It never rewinds the currently bound scope.
func (t *Tracker) RecordFor(accountID, scope string, r Result) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if current, exists := t.scopes[accountID]; exists && current != scope {
		return
	}
	t.scopes[accountID] = scope
	state := t.accounts[accountID]
	if state == nil {
		state = &accountState{}
		t.accounts[accountID] = state
	}
	state.record(r, time.Now())
}

// Score returns the current health score for an account in [0, 1].
// 0 means "do not select"; 1 means "perfectly healthy".
func (t *Tracker) Score(accountID string) float64 {
	return t.get(accountID).score(time.Now())
}

// CooldownUntil returns the time before which the account should not be
// selected. A zero time means the account is available now.
func (t *Tracker) CooldownUntil(accountID string) time.Time {
	return t.get(accountID).cooldownUntil()
}

// Available reports whether an account is out of cooldown right now.
func (t *Tracker) Available(accountID string) bool {
	u := t.CooldownUntil(accountID)
	return u.IsZero() || !time.Now().Before(u)
}

// ClearCooldown manually removes an account's cooldown and resets its
// consecutive-error counter. Used when an operator re-enables an account.
func (t *Tracker) ClearCooldown(accountID string) {
	t.get(accountID).clearCooldown()
}

// Snapshot returns a point-in-time summary of all tracked accounts, sorted by
// descending score. Intended for the console dashboard; not on the hot path.
func (t *Tracker) Snapshot() []AccountSnapshot {
	now := time.Now()
	t.mu.RLock()
	ids := make([]string, 0, len(t.accounts))
	for id := range t.accounts {
		ids = append(ids, id)
	}
	t.mu.RUnlock()

	out := make([]AccountSnapshot, 0, len(ids))
	for _, id := range ids {
		s := t.get(id)
		out = append(out, AccountSnapshot{
			AccountID:   id,
			Score:       s.score(now),
			CoolUntil:   s.cooldownUntil(),
			TotalTokens: func() int64 { s.mu.Lock(); defer s.mu.Unlock(); return s.totalTokens }(),
		})
	}
	// Sort descending by score.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Score > out[j-1].Score; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// AccountSnapshot is a read-only view of one account's health.
type AccountSnapshot struct {
	AccountID   string
	Score       float64
	CoolUntil   time.Time
	TotalTokens int64
}
