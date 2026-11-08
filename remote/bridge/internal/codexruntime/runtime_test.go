package codexruntime

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

type stubExecutor struct {
	execute func(context.Context, *coreauth.Auth, executor.Request, executor.Options) (executor.Response, error)
	stream  func(context.Context, *coreauth.Auth, executor.Request, executor.Options) (*executor.StreamResult, error)
	refresh func(context.Context, *coreauth.Auth) (*coreauth.Auth, error)
}

type syntheticProviderStatusError struct {
	status int
	raw    string
}

func (e syntheticProviderStatusError) Error() string   { return e.raw }
func (e syntheticProviderStatusError) StatusCode() int { return e.status }

func (*stubExecutor) Identifier() string { return "codex" }
func (s *stubExecutor) Execute(ctx context.Context, a *coreauth.Auth, req executor.Request, opts executor.Options) (executor.Response, error) {
	if s.execute == nil {
		return executor.Response{}, ErrUnavailable
	}
	return s.execute(ctx, a, req, opts)
}
func (s *stubExecutor) ExecuteStream(ctx context.Context, a *coreauth.Auth, req executor.Request, opts executor.Options) (*executor.StreamResult, error) {
	if s.stream == nil {
		return nil, ErrUnavailable
	}
	return s.stream(ctx, a, req, opts)
}
func (s *stubExecutor) Refresh(ctx context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
	if s.refresh == nil {
		return nil, ErrUnavailable
	}
	return s.refresh(ctx, a)
}
func (*stubExecutor) CountTokens(context.Context, *coreauth.Auth, executor.Request, executor.Options) (executor.Response, error) {
	return executor.Response{}, ErrUnavailable
}
func (*stubExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, ErrUnavailable
}
func syntheticAuth(id string) *coreauth.Auth {
	return &coreauth.Auth{ID: id, Provider: "codex", Metadata: map[string]any{"access_token": "synthetic-not-real-access-" + id, "refresh_token": "synthetic-not-real-refresh-" + id}}
}
func waitChannel(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stream goroutine did not exit")
	}
}

func TestRuntimeDirectExecutorUsesOnlySuppliedAccountAndPreservesCompact(t *testing.T) {
	a, b := syntheticAuth("account-A"), syntheticAuth("account-B")
	var selected []string
	r := New(t.TempDir())
	r.executor = &stubExecutor{execute: func(_ context.Context, got *coreauth.Auth, req executor.Request, opts executor.Options) (executor.Response, error) {
		selected = append(selected, got.ID)
		if req.Model != "gpt-fixture" || string(req.Payload) != `{"model":"gpt-fixture","input":"fixture"}` || opts.SourceFormat != translator.FormatCodex || opts.ResponseFormat != translator.FormatCodex {
			t.Error("request protocol or payload changed")
		}
		if len(selected) == 2 && opts.Alt != "responses/compact" {
			t.Error("compact flag lost")
		}
		return executor.Response{Payload: []byte(`{"id":"fixture"}`)}, nil
	}}
	for i, auth := range []*coreauth.Auth{a, b} {
		resp, err := r.Execute(context.Background(), auth, []byte(`{"model":"gpt-fixture","input":"fixture"}`), false, i == 1)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || string(body) != `{"id":"fixture"}` {
			t.Fatal("nonstream payload", err)
		}
	}
	if len(selected) != 2 || selected[0] != "account-A" || selected[1] != "account-B" {
		t.Fatalf("account substitution: %v", selected)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Execute(context.Background(), a, []byte(`{"model":"gpt-fixture"}`), false, false); err != ErrUnavailable {
		t.Fatal("closed runtime executed", err)
	}
}

func TestRuntimeStreamBodyCloseCancelsBlockedProducer(t *testing.T) {
	for _, blockedWrite := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "pipe-write"}[blockedWrite], func(t *testing.T) {
			done := make(chan struct{})
			chunks := make(chan executor.StreamChunk)
			r := New(t.TempDir())
			r.executor = &stubExecutor{stream: func(ctx context.Context, a *coreauth.Auth, _ executor.Request, _ executor.Options) (*executor.StreamResult, error) {
				if a.ID != "account-A" {
					t.Error("wrong stream account")
				}
				go func() {
					defer close(done)
					defer close(chunks)
					select {
					case chunks <- executor.StreamChunk{Payload: []byte("data: fixture\n\n")}:
					case <-ctx.Done():
						return
					}
					if blockedWrite {
						select {
						case chunks <- executor.StreamChunk{Payload: []byte("data: blocked\n\n")}:
						case <-ctx.Done():
							return
						}
					}
					<-ctx.Done()
				}()
				return &executor.StreamResult{Chunks: chunks}, nil
			}}
			resp, err := r.Execute(context.Background(), syntheticAuth("account-A"), []byte(`{"model":"gpt-fixture","stream":true}`), true, false)
			if err != nil {
				t.Fatal(err)
			}
			if resp.Header.Get("Content-Type") != "text/event-stream" {
				t.Fatal("wrong SSE header")
			}
			if err = resp.Body.Close(); err != nil {
				t.Fatal(err)
			}
			waitChannel(t, done)
			if _, err = io.ReadAll(resp.Body); err == nil {
				t.Fatal("closed stream stayed readable")
			}
			r.Close()
		})
	}
}

func TestRuntimeStreamErrorsDoNotExposeUpstreamSecret(t *testing.T) {
	r := New(t.TempDir())
	chunks := make(chan executor.StreamChunk, 1)
	chunks <- executor.StreamChunk{Err: errors.New("provider body synthetic-not-real-access-account-A")}
	close(chunks)
	r.executor = &stubExecutor{stream: func(context.Context, *coreauth.Auth, executor.Request, executor.Options) (*executor.StreamResult, error) {
		return &executor.StreamResult{Chunks: chunks}, nil
	}}
	resp, err := r.Execute(context.Background(), syntheticAuth("account-A"), []byte(`{"model":"gpt-fixture","stream":true}`), true, false)
	if resp != nil || err != ErrUnavailable || strings.Contains(err.Error(), "synthetic-not-real") {
		t.Fatalf("unsafe first stream error: response=%v error=%v", resp, err)
	}
}

func TestRuntimeDoesNotReturnSuccessForEmptyStream(t *testing.T) {
	r := New(t.TempDir())
	r.executor = &stubExecutor{stream: func(context.Context, *coreauth.Auth, executor.Request, executor.Options) (*executor.StreamResult, error) {
		chunks := make(chan executor.StreamChunk)
		close(chunks)
		return &executor.StreamResult{Chunks: chunks}, nil
	}}
	resp, err := r.Execute(context.Background(), syntheticAuth("account-A"), []byte(`{"model":"gpt-fixture","stream":true}`), true, false)
	if resp != nil || err != ErrUnavailable {
		t.Fatalf("empty upstream stream became successful response: response=%v error=%v", resp, err)
	}
}

func TestRuntimeNormalizesProviderCapacityErrorsWithoutLeakingBodies(t *testing.T) {
	const secret = "synthetic-private-provider-token"
	tests := []struct {
		name   string
		status int
		raw    string
		call   func(*Runtime) error
	}{
		{
			name:   "execute nested model capacity",
			status: http.StatusTooManyRequests,
			raw:    `{"error":{"message":"Selected model is at capacity. Please try a different model.","code":"model_capacity"},"private":"` + secret + `"}`,
			call: func(r *Runtime) error {
				r.executor = &stubExecutor{execute: func(context.Context, *coreauth.Auth, executor.Request, executor.Options) (executor.Response, error) {
					return executor.Response{}, syntheticProviderStatusError{http.StatusTooManyRequests, `{"error":{"message":"Selected model is at capacity. Please try a different model.","code":"model_capacity"},"private":"` + secret + `"}`}
				}}
				_, err := r.Execute(context.Background(), syntheticAuth("account-A"), []byte(`{"model":"gpt-fixture"}`), false, false)
				return err
			},
		},
		{
			name:   "execute stream direct server overload",
			status: http.StatusServiceUnavailable,
			raw:    `{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later.","private":"` + secret + `"}`,
			call: func(r *Runtime) error {
				r.executor = &stubExecutor{stream: func(context.Context, *coreauth.Auth, executor.Request, executor.Options) (*executor.StreamResult, error) {
					return nil, syntheticProviderStatusError{http.StatusServiceUnavailable, `{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later.","private":"` + secret + `"}`}
				}}
				_, err := r.Execute(context.Background(), syntheticAuth("account-A"), []byte(`{"model":"gpt-fixture","stream":true}`), true, false)
				return err
			},
		},
		{
			name:   "first chunk exact compatibility phrase",
			status: http.StatusTooManyRequests,
			raw:    "Selected model is at capacity. Please try a different model.",
			call: func(r *Runtime) error {
				r.executor = &stubExecutor{stream: func(context.Context, *coreauth.Auth, executor.Request, executor.Options) (*executor.StreamResult, error) {
					chunks := make(chan executor.StreamChunk, 1)
					chunks <- executor.StreamChunk{Err: syntheticProviderStatusError{http.StatusTooManyRequests, "Selected model is at capacity. Please try a different model."}}
					close(chunks)
					return &executor.StreamResult{Chunks: chunks}, nil
				}}
				_, err := r.Execute(context.Background(), syntheticAuth("account-A"), []byte(`{"model":"gpt-fixture","stream":true}`), true, false)
				return err
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := New(t.TempDir())
			err := tc.call(r)
			assertNormalizedProviderError(t, err, tc.status, "model_capacity", secret)
		})
	}
}

func TestRuntimeProviderErrorClassificationIsBoundedAndExact(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		raw        string
		wantStatus int
	}{
		{
			name:       "capacity phrase with suffix is not exact",
			status:     http.StatusServiceUnavailable,
			raw:        "Selected model is at capacity. Please try a different model. synthetic-private-suffix",
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:       "oversized body is not inspected",
			status:     http.StatusServiceUnavailable,
			raw:        `{"code":"server_overloaded","padding":"` + strings.Repeat("x", 70<<10) + `"}`,
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:       "invalid upstream status is not preserved",
			status:     http.StatusFound,
			raw:        `{"code":"server_overloaded"}`,
			wantStatus: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := New(t.TempDir())
			r.executor = &stubExecutor{execute: func(context.Context, *coreauth.Auth, executor.Request, executor.Options) (executor.Response, error) {
				return executor.Response{}, syntheticProviderStatusError{tc.status, tc.raw}
			}}
			_, err := r.Execute(context.Background(), syntheticAuth("account-A"), []byte(`{"model":"gpt-fixture"}`), false, false)
			if err == nil || err.Error() != ErrUnavailable.Error() || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "server_overloaded") {
				t.Fatalf("provider error was not sanitized: %v", err)
			}
			var status interface{ StatusCode() int }
			if tc.wantStatus == 0 {
				if errors.As(err, &status) {
					t.Fatalf("invalid provider status survived normalization: %d", status.StatusCode())
				}
			} else if !errors.As(err, &status) || status.StatusCode() != tc.wantStatus {
				t.Fatalf("valid provider status lost: %v", err)
			}
			var classified interface{ ExecutionCode() string }
			if errors.As(err, &classified) && classified.ExecutionCode() != "" {
				t.Fatalf("untrusted capacity lookalike classified: %q", classified.ExecutionCode())
			}
		})
	}
}

func assertNormalizedProviderError(t *testing.T, err error, wantStatus int, wantCode, secret string) {
	t.Helper()
	if err == nil || err.Error() != ErrUnavailable.Error() || strings.Contains(err.Error(), secret) {
		t.Fatalf("provider error was not sanitized: %v", err)
	}
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != wantStatus {
		t.Fatalf("provider status lost: %v", err)
	}
	var classified interface{ ExecutionCode() string }
	if !errors.As(err, &classified) || classified.ExecutionCode() != wantCode {
		t.Fatalf("provider execution classification lost: %v", err)
	}
}

func TestEmptyBootstrapStoreCannotLoadOrPersistTokens(t *testing.T) {
	store := emptyStore{}
	rows, err := store.List(context.Background())
	if err != nil || len(rows) != 0 {
		t.Fatal("bootstrap loaded auth")
	}
	if _, err = store.Save(context.Background(), syntheticAuth("account-A")); err != ErrUnavailable {
		t.Fatal("bootstrap stored token")
	}
	if err = store.Delete(context.Background(), "account-A"); err != ErrUnavailable {
		t.Fatal("bootstrap delete accepted")
	}
}

func TestRuntimeReadyEmptyLoopbackBootstrapDeniesAllPublicRoutesAndClosesPort(t *testing.T) {
	r := New(t.TempDir())
	t.Cleanup(func() { r.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := r.Ready(ctx); err != nil {
		t.Fatal("empty SDK bootstrap", err)
	}
	r.mu.Lock()
	address := r.bootstrapAddress
	ready := r.executor != nil
	r.mu.Unlock()
	host, _, err := net.SplitHostPort(address)
	if err != nil || host != "127.0.0.1" || !ready {
		t.Fatalf("bootstrap is not ready on loopback: %q", address)
	}
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	for _, path := range []string{"/salcara-ready", "/v1/models", "/v1/responses", "/v0/management/auth-files", "/oauth/callback"} {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+path, nil)
		req.Header.Set("Authorization", "Bearer synthetic-invalid-key")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 403 || strings.Contains(string(body), "synthetic-not-real") {
			t.Fatalf("bootstrap endpoint accessible: %s status=%d body=%s", path, resp.StatusCode, body)
		}
	}
	placeholder, err := os.ReadFile(filepath.Join(r.dir, "sdk-bootstrap", "config.yaml"))
	if err != nil || strings.Contains(string(placeholder), "api-keys") || strings.Contains(string(placeholder), "Bearer") {
		t.Fatal("bootstrap persisted secret", err)
	}
	if err = r.Close(); err != nil {
		t.Fatal("close bootstrap", err)
	}
	if err = r.Ready(context.Background()); err != ErrUnavailable {
		t.Fatal("closed bootstrap ready", err)
	}
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err == nil {
		conn.Close()
		t.Fatal("closed SDK bootstrap listener remains open")
	}
}

func TestConcurrentReadyAndCloseWaitersNeverSucceedAfterClose(t *testing.T) {
	r := New(t.TempDir())
	t.Cleanup(func() { r.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	var succeeded atomic.Int32
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := r.Ready(ctx)
			if err == nil {
				succeeded.Add(1)
			}
			results <- err
		}()
	}
	deadline := time.Now().Add(3 * time.Second)
	var address string
	for time.Now().Before(deadline) {
		r.mu.Lock()
		address = r.bootstrapAddress
		r.mu.Unlock()
		if address != "" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if address == "" {
		t.Fatal("bootstrap did not begin")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	waitChannel(t, done)
	close(results)
	for err := range results {
		if err != nil && err != ErrUnavailable {
			t.Fatalf("unexpected concurrent ready error: %v", err)
		}
	}
	if err := r.Ready(context.Background()); err != ErrUnavailable {
		t.Fatal("Ready returned success after close", err)
	}
	// A Ready call that won before Close may succeed; the listener and all later
	// callers must still be closed after the concurrent operation completes.
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err == nil {
		conn.Close()
		t.Fatalf("concurrent startup/close leaked listener (early ready calls=%d)", succeeded.Load())
	}
}
