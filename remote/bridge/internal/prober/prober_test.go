package prober

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"salcara/bridge/internal/accounthealth"
	"salcara/bridge/internal/config"
)

func immediateSleep(context.Context, time.Duration) error { return nil }

func TestProbeOnceNormalizesPathAndSkipsProtectedSources(t *testing.T) {
	var mu sync.Mutex
	paths := make([]string, 0, 2)
	var auth []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	accounts := []config.LocalAccount{
		{ID: "api", SourceType: "api_key", BaseURL: server.URL + "/tenant/v1/", Key: "api-key", ExtraHeaders: map[string]string{"Authorization": "spoofed", "X-Probe": "yes"}},
		{ID: "proxy", SourceType: "reverse_proxy", BaseURL: server.URL + "/tenant", Key: "proxy-key"},
		{ID: "oauth", SourceType: "oauth", BaseURL: server.URL + "/oauth", Key: "oauth-marker"},
		{ID: "official", SourceType: "official_account", CredentialRef: "vault-ref", BaseURL: server.URL + "/official", Key: "official-marker"},
		{ID: "empty", SourceType: "api_key", BaseURL: server.URL, Key: ""},
		{ID: "bad", SourceType: "api_key", BaseURL: "not a url", Key: "bad"},
	}
	health := accounthealth.NewTracker()
	p := New(Options{Health: health, Accounts: func() []config.LocalAccount { return accounts }, Client: server.Client(), Sleep: immediateSleep})
	p.ProbeOnce(context.Background())

	mu.Lock()
	gotPaths, gotAuth := append([]string(nil), paths...), append([]string(nil), auth...)
	mu.Unlock()
	if len(gotPaths) != 2 || gotPaths[0] != "/tenant/v1/models" || gotPaths[1] != "/tenant/v1/models" {
		t.Fatalf("probe paths = %v, want two normalized /tenant/v1/models requests", gotPaths)
	}
	seenAuth := map[string]bool{}
	for _, value := range gotAuth {
		seenAuth[value] = true
	}
	if !seenAuth["Bearer api-key"] || !seenAuth["Bearer proxy-key"] || len(seenAuth) != 2 {
		t.Fatalf("probe authorization = %v, structural auth was overridden", gotAuth)
	}
	healthIDs := map[string]bool{}
	for _, item := range health.Snapshot() {
		healthIDs[item.AccountID] = true
	}
	if !healthIDs["api"] || !healthIDs["proxy"] || healthIDs["oauth"] || healthIDs["official"] {
		t.Fatalf("health accounts = %v; only direct API and proxy accounts should be recorded", healthIDs)
	}
}

func TestProbeOnceHonorsAnthropicAPIKeyAndExtraHeaders(t *testing.T) {
	seen := make(chan *http.Request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Clone(r.Context())
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	health := accounthealth.NewTracker()
	p := New(Options{Health: health, Accounts: func() []config.LocalAccount {
		return []config.LocalAccount{{ID: "anthropic", SourceType: "api_key", BaseURL: server.URL, Key: "sk-ant", AuthMode: "api-key", ExtraHeaders: map[string]string{"anthropic-version": "2023-06-01"}}}
	}, Client: server.Client(), Sleep: immediateSleep})
	p.ProbeOnce(context.Background())
	select {
	case req := <-seen:
		if req.Header.Get("X-Api-Key") != "sk-ant" || req.Header.Get("Anthropic-Version") != "2023-06-01" {
			t.Fatalf("anthropic probe headers = %v", req.Header)
		}
	case <-time.After(time.Second):
		t.Fatal("probe did not reach fixture")
	}
	if got := health.Snapshot(); len(got) != 1 || got[0].AccountID != "anthropic" {
		t.Fatalf("health snapshot = %+v", got)
	}
}

func TestProbeOnceCancellationDoesNotRecordHealth(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	health := accounthealth.NewTracker()
	called := false
	p := New(Options{Health: health, Accounts: func() []config.LocalAccount {
		return []config.LocalAccount{{ID: "cancelled", SourceType: "api_key", BaseURL: "https://example.test", Key: "key"}}
	}, Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("must not send after cancellation")
	})}, Sleep: immediateSleep})
	p.ProbeOnce(ctx)
	if called {
		t.Fatal("cancelled probe sent a request")
	}
	if got := health.Snapshot(); len(got) != 0 {
		t.Fatalf("cancelled probe recorded health: %+v", got)
	}
}

func TestProbeOnceCancellationDuringInjectedSleepDoesNotSend(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	health := accounthealth.NewTracker()
	called := false
	p := New(Options{Health: health, Accounts: func() []config.LocalAccount {
		return []config.LocalAccount{{ID: "cancelled", SourceType: "api_key", BaseURL: "https://example.test", Key: "key"}}
	}, Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("must not send after cancellation")
	})}, Sleep: func(ctx context.Context, _ time.Duration) error {
		cancel()
		return nil // Simulates a test or caller-provided sleeper that ignores ctx.
	}})
	p.ProbeOnce(ctx)
	if called {
		t.Fatal("cancelled probe sent a request after injected sleep")
	}
	if got := health.Snapshot(); len(got) != 0 {
		t.Fatalf("cancelled probe recorded health: %+v", got)
	}
}

func TestNewDefaultsNilClient(t *testing.T) {
	p := New(Options{})
	if p.opts.Client != http.DefaultClient {
		t.Fatal("nil client did not default to http.DefaultClient")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
