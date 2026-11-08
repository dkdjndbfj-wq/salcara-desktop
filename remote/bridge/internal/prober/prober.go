// Package prober performs lightweight, periodic health probes against each
// configured account to detect failures before a real request hits them.
//
// A Prober sends GET /v1/models to each account at a configurable interval.
// A successful response (2xx) records a positive observation; a 4xx/5xx records
// the appropriate ErrorKind. The Prober never sends inference traffic and never
// reads or stores model lists — it only cares about reachability and auth.
//
// Usage (called once from console/server.go after New()):
//
//	p := prober.New(prober.Options{
//	    Health:   s.apiService.Health,
//	    Accounts: func() []config.LocalAccount { return s.d.Store.Get().LocalAccounts },
//	    Client:   s.apiService.Client,
//	    Interval: 5 * time.Minute,
//	})
//	go p.Run(ctx)
package prober

import (
	"context"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"salcara/bridge/internal/accounthealth"
	"salcara/bridge/internal/apierror"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/gateway"
)

const (
	defaultInterval = 5 * time.Minute
	probeTimeout    = 15 * time.Second
	// Jitter up to ±20% of interval to avoid thundering-herd on large account pools.
	jitterFraction = 0.2
	// Maximum probes to run concurrently; avoids hammering providers under a large pool.
	maxConcurrent = 4
)

// Options configures a Prober.
type Options struct {
	// Health is the shared health tracker updated after each probe.
	Health *accounthealth.Tracker
	// Accounts returns the current account list; called at each probe cycle.
	Accounts func() []config.LocalAccount
	// Client is the HTTP client used for probes. New defaults nil to the
	// standard client; per-request deadlines still bound every probe.
	Client *http.Client
	// Interval between probe cycles. Defaults to 5 minutes.
	Interval time.Duration
	// OnResult is called after each probe (for logging/metrics). Optional.
	OnResult func(accountID, baseURL string, kind accounthealth.ErrorKind, latency time.Duration)
	// Sleep overrides the small startup/per-account jitter delay in tests. A
	// nil value uses the context-aware timer implementation.
	Sleep func(context.Context, time.Duration) error
}

// Prober sends lightweight health probes to every configured account.
type Prober struct {
	opts Options
}

// New returns a Prober ready to run. Call Run(ctx) in a goroutine.
func New(opts Options) *Prober {
	if opts.Interval <= 0 {
		opts.Interval = defaultInterval
	}
	if opts.Client == nil {
		opts.Client = http.DefaultClient
	}
	return &Prober{opts: opts}
}

// Run starts the probe loop. It blocks until ctx is cancelled.
func (p *Prober) Run(ctx context.Context) {
	// Stagger the first probe slightly so it doesn't fire immediately on startup.
	select {
	case <-ctx.Done():
		return
	default:
	}
	if err := p.sleep(ctx, p.jitter(30*time.Second)); err != nil {
		return
	}

	ticker := time.NewTicker(p.opts.Interval)
	defer ticker.Stop()

	for {
		p.runCycle(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ProbeOnce immediately probes all accounts once. Useful for testing.
func (p *Prober) ProbeOnce(ctx context.Context) {
	p.runCycle(ctx)
}

// runCycle probes every unique (baseURL, key) combination once.
func (p *Prober) runCycle(ctx context.Context) {
	if ctx.Err() != nil || p.opts.Accounts == nil || p.opts.Health == nil {
		return
	}
	accounts := p.opts.Accounts()
	if len(accounts) == 0 {
		return
	}

	// Deduplicate by account ID; avoid redundant probes. Only direct API-key
	// endpoints are probed: OAuth and protected official credentials must go
	// through their own executor rather than a generic metadata request.
	seen := make(map[string]bool, len(accounts))
	var targets []config.LocalAccount
	for _, a := range accounts {
		if a.ID == "" || seen[a.ID] {
			continue
		}
		// Only generic API and reverse-proxy cards have a directly probeable
		// bearer endpoint. OAuth/official cards are fulfilled by their protected
		// executor; treating their metadata URL as a generic /models endpoint
		// would create false cooldowns and an unintended upstream request.
		source := a.SourceType
		if source == "" {
			source = "api_key"
		}
		if (source != "api_key" && source != "reverse_proxy") || a.CredentialRef != "" || strings.TrimSpace(a.Key) == "" {
			continue
		}
		if _, _, err := config.APIBase(a.BaseURL); err != nil {
			continue
		}
		seen[a.ID] = true
		targets = append(targets, a)
	}

	// Semaphore for max-concurrent probes.
	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup
	for _, a := range targets {
		// A ready semaphore send can win a select alongside an already-closed
		// context. Check explicitly before scheduling work so cancellation never
		// starts a new probe cycle's request fan-out.
		if ctx.Err() != nil {
			wg.Wait()
			return
		}
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(acct config.LocalAccount) {
			defer wg.Done()
			defer func() { <-sem }()
			p.probeAccount(ctx, acct)
		}(a)
	}
	wg.Wait()
}

func (p *Prober) probeAccount(ctx context.Context, a config.LocalAccount) {
	if ctx.Err() != nil {
		return
	}
	baseURL, v1, err := config.APIBase(a.BaseURL)
	if err != nil || baseURL == "" || v1 == "" {
		return
	}

	probeURL := v1 + "/models"
	// Add random jitter so concurrent probes don't all fire at exactly the same time.
	if err := p.sleep(ctx, p.jitter(2*time.Second)); err != nil || ctx.Err() != nil {
		return
	}

	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if probeCtx.Err() != nil {
		return
	}

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, probeURL, nil)
	if err != nil {
		return
	}
	// Match account-specific routing metadata first; then set credential headers
	// so ExtraHeaders can never replace the actual API key.
	gateway.InjectAccountHeaders(req, a)
	if a.Key != "" {
		if a.AuthMode == "api-key" {
			req.Header.Set("x-api-key", a.Key)
			req.Header.Set("anthropic-version", "2023-06-01")
		} else {
			req.Header.Set("Authorization", "Bearer "+a.Key)
		}
	}
	req.Header.Set("User-Agent", "SalcaraBridgeProbe/1.0")

	start := time.Now()
	resp, err := p.opts.Client.Do(req)
	latency := time.Since(start)
	if ctx.Err() != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return // shutdown cancellation is not an upstream health failure
	}

	var body []byte
	var status int
	var header http.Header
	if err != nil {
		// Network-level error: treat as NetworkRetry, status 0.
		status = 0
		header = http.Header{}
	} else {
		if resp == nil {
			return
		}
		status = resp.StatusCode
		header = resp.Header
		// Read a small slice of the body to aid error classification.
		limited := io.LimitReader(resp.Body, 4096)
		body, _ = io.ReadAll(limited)
		resp.Body.Close()
	}

	result := apierror.Classify(status, header, body, latency)
	p.opts.Health.Record(a.ID, result)

	if p.opts.OnResult != nil {
		p.opts.OnResult(a.ID, baseURL, result.Kind, latency)
	}
}

func (p *Prober) jitter(base time.Duration) time.Duration {
	delta := float64(base) * jitterFraction
	return base + time.Duration((rand.Float64()*2-1)*delta) //nolint:gosec
}

func (p *Prober) sleep(ctx context.Context, d time.Duration) error {
	if p.opts.Sleep != nil {
		return p.opts.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
