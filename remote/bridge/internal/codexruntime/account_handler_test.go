package codexruntime

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"salcara/bridge/internal/codexaccounts"
	"salcara/bridge/internal/config"
)

type syntheticStatusError struct{ code int }

func (e syntheticStatusError) Error() string   { return "synthetic provider failure" }
func (e syntheticStatusError) StatusCode() int { return e.code }
func handlerFixture(t *testing.T, expiry string, stub *stubExecutor) (AccountExecutor, config.LocalAccount) {
	t.Helper()
	r := New(t.TempDir())
	r.executor = stub
	accounts, err := codexaccounts.New(codexaccounts.Options{Path: filepath.Join(t.TempDir(), "accounts", "vault.json"), Refresher: r})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(accounts.Close)
	t.Cleanup(func() { r.Close() })
	p, err := codexaccounts.ParseImport([]byte(`{"type":"codex","email":"fixture@example.test","access_token":"synthetic-access-old","refresh_token":"synthetic-refresh-old","expired":"` + expiry + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := accounts.Import(context.Background(), p, "")
	if err != nil {
		t.Fatal(err)
	}
	return AccountExecutor{Accounts: accounts, Runtime: r}, config.LocalAccount{ID: "account-A", SourceType: "official_account", ProviderID: "openai", CredentialRef: rows[0].ID, CredentialEpoch: rows[0].Epoch, BaseURL: "https://chatgpt.com/backend-api/codex", Model: "gpt-fixture"}
}
func rotatedSDK(a *coreauth.Auth) *coreauth.Auth {
	record := a.Clone()
	record.Metadata["access_token"] = "synthetic-access-rotated"
	record.Metadata["refresh_token"] = "synthetic-refresh-rotated"
	record.Metadata["expired"] = "2099-01-01T00:00:00Z"
	return record
}

func TestAccountExecutorExpiryRefreshesPersistedCredentialBeforeExecutingSameAccount(t *testing.T) {
	var refreshes, executes atomic.Int32
	var refreshID, executeID string
	stub := &stubExecutor{}
	stub.refresh = func(_ context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
		refreshes.Add(1)
		refreshID = a.ID
		if a.Metadata["refresh_token"] != "synthetic-refresh-old" {
			t.Error("wrong original refresh")
		}
		return rotatedSDK(a), nil
	}
	stub.execute = func(_ context.Context, a *coreauth.Auth, _ executor.Request, _ executor.Options) (executor.Response, error) {
		executes.Add(1)
		executeID = a.ID
		if a.Metadata["access_token"] != "synthetic-access-rotated" {
			t.Error("unrefreshed access used")
		}
		return executor.Response{Payload: []byte(`{"id":"fixture"}`)}, nil
	}
	e, a := handlerFixture(t, "2000-01-01T00:00:00Z", stub)
	resp, err := e.Do(context.Background(), a, []byte(`{"model":"gpt-fixture","input":"fixture"}`), false, false, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if refreshes.Load() != 1 || executes.Load() != 1 || refreshID != a.CredentialRef || executeID != a.CredentialRef {
		t.Fatalf("refresh/execution substituted account: refresh=%s execute=%s", refreshID, executeID)
	}
	persisted, err := e.Accounts.SDKRecord(context.Background(), a.CredentialRef)
	if err != nil || persisted.Metadata["refresh_token"] != "synthetic-refresh-rotated" {
		t.Fatal("refresh not persisted before use", err)
	}
}

func TestAccountExecutor401RetriesOnlyOnceOnSameAccountBeforeOutput(t *testing.T) {
	var calls, refreshes atomic.Int32
	var ids []string
	stub := &stubExecutor{refresh: func(_ context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
		refreshes.Add(1)
		return rotatedSDK(a), nil
	}}
	stub.execute = func(_ context.Context, a *coreauth.Auth, _ executor.Request, _ executor.Options) (executor.Response, error) {
		ids = append(ids, a.ID)
		if calls.Add(1) == 1 {
			return executor.Response{}, syntheticStatusError{401}
		}
		if a.Metadata["access_token"] != "synthetic-access-rotated" {
			t.Error("401 replay used stale access")
		}
		return executor.Response{Payload: []byte(`{"id":"fixture"}`)}, nil
	}
	e, a := handlerFixture(t, "2099-01-01T00:00:00Z", stub)
	resp, err := e.Do(context.Background(), a, []byte(`{"model":"gpt-fixture"}`), false, false, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if calls.Load() != 2 || refreshes.Load() != 1 || len(ids) != 2 || ids[0] != a.CredentialRef || ids[1] != a.CredentialRef {
		t.Fatalf("wrong auth retry: calls=%d refreshes=%d ids=%v", calls.Load(), refreshes.Load(), ids)
	}
}

func TestAccountExecutorRetriesFirstStream401BeforeOutput(t *testing.T) {
	var calls, refreshes atomic.Int32
	var ids []string
	stub := &stubExecutor{refresh: func(_ context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
		refreshes.Add(1)
		return rotatedSDK(a), nil
	}}
	stub.stream = func(_ context.Context, a *coreauth.Auth, _ executor.Request, _ executor.Options) (*executor.StreamResult, error) {
		ids = append(ids, a.ID)
		chunks := make(chan executor.StreamChunk, 1)
		if calls.Add(1) == 1 {
			chunks <- executor.StreamChunk{Err: syntheticStatusError{401}}
		} else {
			if a.Metadata["access_token"] != "synthetic-access-rotated" {
				t.Error("stream retry used stale access")
			}
			chunks <- executor.StreamChunk{Payload: []byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")}
		}
		close(chunks)
		return &executor.StreamResult{Chunks: chunks}, nil
	}
	e, a := handlerFixture(t, "2099-01-01T00:00:00Z", stub)
	resp, err := e.Do(context.Background(), a, []byte(`{"model":"gpt-fixture","stream":true}`), true, false, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil || !strings.Contains(string(body), "response.completed") || calls.Load() != 2 || refreshes.Load() != 1 || len(ids) != 2 || ids[0] != a.CredentialRef || ids[1] != a.CredentialRef {
		t.Fatalf("wrong stream auth retry: calls=%d refreshes=%d ids=%v body=%s error=%v", calls.Load(), refreshes.Load(), ids, body, readErr)
	}
}

func TestAccountExecutorDoesNotRetry429Or503OrRepeated401(t *testing.T) {
	for _, code := range []int{429, 503, 401} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			var calls, refreshes atomic.Int32
			stub := &stubExecutor{execute: func(context.Context, *coreauth.Auth, executor.Request, executor.Options) (executor.Response, error) {
				calls.Add(1)
				return executor.Response{}, syntheticStatusError{code}
			}, refresh: func(_ context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
				refreshes.Add(1)
				return rotatedSDK(a), nil
			}}
			e, a := handlerFixture(t, "2099-01-01T00:00:00Z", stub)
			if _, err := e.Do(context.Background(), a, []byte(`{"model":"gpt-fixture"}`), false, false, func() bool { return true }); err == nil {
				t.Fatal("provider refusal ignored")
			}
			wantCalls, wantRefreshes := int32(1), int32(0)
			if code == 401 {
				wantCalls, wantRefreshes = 2, 1
			}
			if calls.Load() != wantCalls || refreshes.Load() != wantRefreshes {
				t.Fatalf("unexpected retry: calls=%d refresh=%d", calls.Load(), refreshes.Load())
			}
		})
	}
}

func TestAccountExecutorDoesNotReplayAfterPartialStream401(t *testing.T) {
	var streams, refreshes atomic.Int32
	stub := &stubExecutor{refresh: func(_ context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
		refreshes.Add(1)
		return rotatedSDK(a), nil
	}, stream: func(context.Context, *coreauth.Auth, executor.Request, executor.Options) (*executor.StreamResult, error) {
		streams.Add(1)
		chunks := make(chan executor.StreamChunk, 2)
		chunks <- executor.StreamChunk{Payload: []byte("event: response.output_text.delta\ndata: {\"delta\":\"fixture partial\"}\n\n")}
		chunks <- executor.StreamChunk{Err: syntheticStatusError{401}}
		close(chunks)
		return &executor.StreamResult{Chunks: chunks}, nil
	}}
	e, a := handlerFixture(t, "2099-01-01T00:00:00Z", stub)
	resp, err := e.Do(context.Background(), a, []byte(`{"model":"gpt-fixture","stream":true}`), true, false, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != ErrUnavailable || !strings.Contains(string(body), "fixture partial") || streams.Load() != 1 || refreshes.Load() != 0 {
		t.Fatalf("partial response was replayed: streams=%d refresh=%d body=%s error=%v", streams.Load(), refreshes.Load(), body, err)
	}
}

func TestAccountExecutorRechecksAuthorizationAfterSlowRefresh(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var allowed atomic.Bool
	allowed.Store(true)
	var executes atomic.Int32
	stub := &stubExecutor{refresh: func(_ context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
		close(entered)
		<-release
		return rotatedSDK(a), nil
	}, execute: func(context.Context, *coreauth.Auth, executor.Request, executor.Options) (executor.Response, error) {
		executes.Add(1)
		return executor.Response{}, errors.New("must not execute revoked request")
	}}
	e, a := handlerFixture(t, "2000-01-01T00:00:00Z", stub)
	done := make(chan error, 1)
	go func() {
		_, err := e.Do(context.Background(), a, []byte(`{"model":"gpt-fixture"}`), false, false, allowed.Load)
		done <- err
	}()
	<-entered
	allowed.Store(false)
	close(release)
	select {
	case err := <-done:
		if err != codexaccounts.ErrConflict || executes.Load() != 0 {
			t.Fatal("revoked request executed after refresh", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("revoked request stuck")
	}
}

func TestAccountExecutorRechecksAuthorizationAfterFirstStreamPayloadWait(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var allowed atomic.Bool
	allowed.Store(true)
	stub := &stubExecutor{stream: func(ctx context.Context, _ *coreauth.Auth, _ executor.Request, _ executor.Options) (*executor.StreamResult, error) {
		chunks := make(chan executor.StreamChunk)
		go func() {
			defer close(chunks)
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return
			}
			select {
			case chunks <- executor.StreamChunk{Payload: []byte("event: response.completed\ndata: {}\n\n")}:
			case <-ctx.Done():
			}
		}()
		return &executor.StreamResult{Chunks: chunks}, nil
	}}
	e, a := handlerFixture(t, "2099-01-01T00:00:00Z", stub)
	done := make(chan struct {
		resp *http.Response
		err  error
	}, 1)
	go func() {
		resp, err := e.Do(context.Background(), a, []byte(`{"model":"gpt-fixture","stream":true}`), true, false, allowed.Load)
		done <- struct {
			resp *http.Response
			err  error
		}{resp: resp, err: err}
	}()
	<-entered
	allowed.Store(false)
	close(release)
	select {
	case result := <-done:
		if result.resp != nil || result.err != codexaccounts.ErrConflict {
			t.Fatalf("revoked request committed first stream payload: response=%v error=%v", result.resp, result.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("revoked first-payload wait stuck")
	}
}

func TestAccountExecutorDoesNotRefreshAfterRevokedFirstStream401(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var allowed atomic.Bool
	allowed.Store(true)
	var streams, refreshes atomic.Int32
	stub := &stubExecutor{
		refresh: func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
			refreshes.Add(1)
			return nil, errors.New("must not refresh revoked request")
		},
		stream: func(ctx context.Context, _ *coreauth.Auth, _ executor.Request, _ executor.Options) (*executor.StreamResult, error) {
			streams.Add(1)
			chunks := make(chan executor.StreamChunk)
			go func() {
				defer close(chunks)
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return
				}
				select {
				case chunks <- executor.StreamChunk{Err: syntheticStatusError{401}}:
				case <-ctx.Done():
				}
			}()
			return &executor.StreamResult{Chunks: chunks}, nil
		},
	}
	e, a := handlerFixture(t, "2099-01-01T00:00:00Z", stub)
	done := make(chan error, 1)
	go func() {
		_, err := e.Do(context.Background(), a, []byte(`{"model":"gpt-fixture","stream":true}`), true, false, allowed.Load)
		done <- err
	}()
	<-entered
	allowed.Store(false)
	close(release)
	select {
	case err := <-done:
		if err != codexaccounts.ErrConflict || streams.Load() != 1 || refreshes.Load() != 0 {
			t.Fatalf("revoked first-stream 401 was refreshed: streams=%d refresh=%d error=%v", streams.Load(), refreshes.Load(), err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("revoked first-stream 401 stuck")
	}
}

func TestAccountExecutorRejectsInitiallyRevokedBeforeRefreshOrExecution(t *testing.T) {
	var refreshes, executes atomic.Int32
	stub := &stubExecutor{
		refresh: func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
			refreshes.Add(1)
			return nil, errors.New("must not refresh revoked request")
		},
		execute: func(context.Context, *coreauth.Auth, executor.Request, executor.Options) (executor.Response, error) {
			executes.Add(1)
			return executor.Response{}, errors.New("must not execute revoked request")
		},
	}
	e, a := handlerFixture(t, "2000-01-01T00:00:00Z", stub)
	resp, err := e.Do(context.Background(), a, []byte(`{"model":"gpt-fixture"}`), false, false, func() bool { return false })
	if resp != nil || err != codexaccounts.ErrConflict || refreshes.Load() != 0 || executes.Load() != 0 {
		t.Fatalf("initially revoked request touched runtime: response=%v refresh=%d execute=%d error=%v", resp, refreshes.Load(), executes.Load(), err)
	}
}

func TestAccountExecutorRejectsProtectedCredentialReplacementBeforeConfigCommit(t *testing.T) {
	var executes atomic.Int32
	stub := &stubExecutor{execute: func(context.Context, *coreauth.Auth, executor.Request, executor.Options) (executor.Response, error) {
		executes.Add(1)
		return executor.Response{}, errors.New("must not execute replacement")
	}}
	e, a := handlerFixture(t, "2099-01-01T00:00:00Z", stub)
	p, err := codexaccounts.ParseImport([]byte(`{"type":"codex","access_token":"synthetic-replaced-access","refresh_token":"synthetic-replaced-refresh","expired":"2099-01-01T00:00:00Z"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Accounts.Import(context.Background(), p, a.CredentialRef); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Do(context.Background(), a, []byte(`{"model":"gpt-fixture"}`), false, false, func() bool { return true }); err != codexaccounts.ErrConflict || executes.Load() != 0 {
		t.Fatalf("stale identity executed replacement: calls=%d error=%v", executes.Load(), err)
	}
}

func TestAccountExecutorRetainsValidOldAccessWhenRefreshNeedsLogin(t *testing.T) {
	var refreshes, executes atomic.Int32
	stub := &stubExecutor{refresh: func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
		refreshes.Add(1)
		return nil, codexaccounts.ErrNeedsLogin
	}, execute: func(_ context.Context, a *coreauth.Auth, _ executor.Request, _ executor.Options) (executor.Response, error) {
		executes.Add(1)
		if a.Metadata["access_token"] != "synthetic-access-old" {
			t.Error("valid old access lost")
		}
		return executor.Response{Payload: []byte(`{"id":"fixture"}`)}, nil
	}}
	e, a := handlerFixture(t, time.Now().Add(40*time.Second).UTC().Format(time.RFC3339), stub)
	resp, err := e.Do(context.Background(), a, []byte(`{"model":"gpt-fixture"}`), false, false, func() bool { return true })
	if err != nil {
		t.Fatal("valid old access refused", err)
	}
	resp.Body.Close()
	row, _ := e.Accounts.Get(a.CredentialRef)
	if row.Status != codexaccounts.StatusNeedsLogin || refreshes.Load() != 1 || executes.Load() != 1 {
		t.Fatalf("fallback state: %+v refresh=%d executes=%d", row, refreshes.Load(), executes.Load())
	}
	resp, err = e.Do(context.Background(), a, []byte(`{"model":"gpt-fixture"}`), false, false, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if refreshes.Load() != 1 || executes.Load() != 2 {
		t.Fatal("invalid refresh token unnecessarily reused")
	}
}
