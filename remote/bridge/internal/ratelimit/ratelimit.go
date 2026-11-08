// Package ratelimit provides per-account and per-client token-bucket rate
// limiters for the local API layer. It is the Go equivalent of Cockpit's
// per-channel concurrency + RPM/TPM controls.
//
// Two limit types:
//   - RPM (requests per minute)  — hard cap on how many HTTP calls per minute
//   - TPM (tokens per minute)    — soft cap; rejects a request whose estimated
//                                  token cost would push the bucket negative.
//
// Both use a token-bucket algorithm replenished at a fixed rate; burst is set
// to 20% above the steady-state rate so short spikes don't needlessly reject.
//
// Usage:
//
//	lim := ratelimit.New(ratelimit.Config{RPM: 60, TPM: 100_000})
//	ok, wait := lim.AllowRequest(estimatedTokens)
//	if !ok { // return 429, optionally include Retry-After: wait.Seconds() }
//
// A zero Config means "unlimited" for both dimensions.
package ratelimit

import (
	"math"
	"sync"
	"time"
)

// Config declares the rate limits for one account or one API client.
// Zero values mean "no limit".
type Config struct {
	// RPM is the maximum requests per minute. 0 = unlimited.
	RPM int
	// TPM is the maximum tokens per minute. 0 = unlimited.
	TPM int
}

// Limiter enforces a Config.  Safe for concurrent use.
type Limiter struct {
	mu sync.Mutex
	rp *bucket // requests
	tp *bucket // tokens
}

// New creates a Limiter from a Config. Call Allow* methods to check a request.
func New(c Config) *Limiter {
	l := &Limiter{}
	if c.RPM > 0 {
		l.rp = newBucket(float64(c.RPM)/60.0, int(math.Ceil(float64(c.RPM)*1.2/60.0)+1))
	}
	if c.TPM > 0 {
		l.tp = newBucket(float64(c.TPM)/60.0, int(math.Ceil(float64(c.TPM)*1.2/60.0)+1))
	}
	return l
}

// AllowRequest checks whether the request is allowed under the current limits.
// estimatedTokens is the caller's best-effort estimate of prompt + completion
// size; pass 0 if unknown (RPM limit is still enforced).
//
// Returns (true, 0) if allowed, (false, retryAfter) if denied.
// retryAfter is the duration to include in a Retry-After header.
func (l *Limiter) AllowRequest(estimatedTokens int) (allowed bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()

	if l.rp != nil {
		ok, wait := l.rp.take(now, 1)
		if !ok {
			return false, wait
		}
	}
	if l.tp != nil && estimatedTokens > 0 {
		ok, wait := l.tp.take(now, estimatedTokens)
		if !ok {
			// RPM was admitted — refill it back (we haven't forwarded anything).
			if l.rp != nil {
				l.rp.refill(1)
			}
			return false, wait
		}
	}
	return true, 0
}

// RecordTokens records the actual token usage after a completed request so the
// TPM bucket stays accurate. Call with the real usage after you get the response.
// If estimatedTokens was already charged, pass the delta (actual - estimated).
func (l *Limiter) RecordTokens(delta int) {
	if l.tp == nil || delta <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if delta > 0 {
		// Charge additional tokens beyond the estimate.
		l.tp.charge(delta)
	}
}

// ── token bucket ─────────────────────────────────────────────────────────────

type bucket struct {
	tokens    float64
	maxTokens float64
	rate      float64 // tokens per second
	lastFill  time.Time
}

func newBucket(ratePerSec float64, burst int) *bucket {
	b := &bucket{
		rate:      ratePerSec,
		maxTokens: float64(burst),
		lastFill:  time.Now(),
	}
	b.tokens = b.maxTokens // start full
	return b
}

func (b *bucket) fill(now time.Time) {
	elapsed := now.Sub(b.lastFill).Seconds()
	if elapsed > 0 {
		b.tokens = math.Min(b.maxTokens, b.tokens+b.rate*elapsed)
		b.lastFill = now
	}
}

func (b *bucket) take(now time.Time, n int) (ok bool, retryAfter time.Duration) {
	b.fill(now)
	need := float64(n)
	if b.tokens >= need {
		b.tokens -= need
		return true, 0
	}
	// How long until enough tokens accumulate.
	wait := time.Duration((need-b.tokens)/b.rate*1000) * time.Millisecond
	return false, wait
}

func (b *bucket) refill(n int) {
	b.tokens = math.Min(b.maxTokens, b.tokens+float64(n))
}

func (b *bucket) charge(n int) {
	b.tokens = math.Max(0, b.tokens-float64(n))
}

// ── per-account and per-client registry ─────────────────────────────────────

// Registry manages a set of named Limiters that are created on demand and
// recycled when their Config changes.
type Registry struct {
	mu      sync.Mutex
	entries map[string]*entry
}

type entry struct {
	cfg Config
	lim *Limiter
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{entries: make(map[string]*entry)}
}

// Get returns the Limiter for key, (re)creating it if the config changed.
// A zero Config returns nil (unlimited).
func (r *Registry) Get(key string, cfg Config) *Limiter {
	if cfg.RPM == 0 && cfg.TPM == 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entries[key]
	if e == nil || e.cfg != cfg {
		e = &entry{cfg: cfg, lim: New(cfg)}
		r.entries[key] = e
	}
	return e.lim
}

// Delete removes the Limiter for key (e.g. when an account is deleted).
func (r *Registry) Delete(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, key)
}
