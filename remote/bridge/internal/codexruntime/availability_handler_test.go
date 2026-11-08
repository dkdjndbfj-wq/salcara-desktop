package codexruntime

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"salcara/bridge/internal/codexaccounts"
	"salcara/bridge/internal/config"
)

func TestAccountExecutorRefreshOnlyRetriesExchangeBeforeStreamOrInference(t *testing.T) {
	for _, streamed := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonstream", true: "stream"}[streamed], func(t *testing.T) {
			var refreshes, executed atomic.Int32
			stub := &stubExecutor{}
			stub.refresh = func(_ context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
				if refreshes.Add(1) == 1 {
					return nil, errors.New("synthetic transient exchange failure")
				}
				return rotatedSDK(a), nil
			}
			observe := func(a *coreauth.Auth) {
				executed.Add(1)
				if a.Metadata["access_token"] != "synthetic-access-rotated" {
					t.Error("empty/unrefreshed credential reached inference")
				}
			}
			stub.execute = func(_ context.Context, a *coreauth.Auth, _ executor.Request, _ executor.Options) (executor.Response, error) {
				observe(a)
				return executor.Response{}, syntheticStatusError{503}
			}
			stub.stream = func(_ context.Context, a *coreauth.Auth, _ executor.Request, _ executor.Options) (*executor.StreamResult, error) {
				observe(a)
				return nil, syntheticStatusError{503}
			}
			runtime := New(t.TempDir())
			runtime.executor = stub
			accounts, err := codexaccounts.New(codexaccounts.Options{Path: filepath.Join(t.TempDir(), "vault.json"), Refresher: runtime})
			if err != nil {
				t.Fatal(err)
			}
			defer accounts.Close()
			defer runtime.Close()
			p, _ := codexaccounts.ParseTokenImport([]byte("synthetic-refresh-only"), "refresh_token")
			rows, err := accounts.Import(context.Background(), p, "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = accounts.Refresh(context.Background(), rows[0].ID); err == nil {
				t.Fatal("expected first transient import exchange failure")
			}
			row, _ := accounts.Get(rows[0].ID)
			if row.ExpiresAt != nil || row.Status != codexaccounts.StatusRefreshFailed {
				t.Fatal("not a refresh-only failure fixture")
			}
			a := config.LocalAccount{ID: "fixture-A", ProviderID: "openai", SourceType: "official_account", CredentialRef: row.ID, CredentialEpoch: row.IdentityEpoch, Model: "gpt-fixture", BaseURL: "https://chatgpt.com/backend-api/codex"}
			_, _ = (AccountExecutor{Accounts: accounts, Runtime: runtime}).Do(context.Background(), a, []byte(`{"model":"gpt-fixture","input":"synthetic"}`), streamed, false, func() bool { return true })
			if refreshes.Load() != 2 || executed.Load() != 1 {
				t.Fatalf("missing owned refresh: refresh=%d execution=%d", refreshes.Load(), executed.Load())
			}
		})
	}
}

func TestAccountExecutorRefreshOnlyFailureOrRevocationNeverDispatchesEmptyAccess(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		t.Run(map[bool]string{false: "exchange-failure", true: "revoked-during-exchange"}[revoked], func(t *testing.T) {
			allowed := atomic.Bool{}
			allowed.Store(true)
			var executed atomic.Int32
			stub := &stubExecutor{refresh: func(_ context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
				if revoked {
					allowed.Store(false)
					return rotatedSDK(a), nil
				}
				return nil, errors.New("synthetic transient failure")
			}, execute: func(context.Context, *coreauth.Auth, executor.Request, executor.Options) (executor.Response, error) {
				executed.Add(1)
				return executor.Response{}, nil
			}}
			runtime := New(t.TempDir())
			runtime.executor = stub
			accounts, err := codexaccounts.New(codexaccounts.Options{Path: filepath.Join(t.TempDir(), "vault.json"), Refresher: runtime})
			if err != nil {
				t.Fatal(err)
			}
			defer accounts.Close()
			defer runtime.Close()
			p, _ := codexaccounts.ParseTokenImport([]byte("synthetic-refresh-only"), "refresh_token")
			rows, err := accounts.Import(context.Background(), p, "")
			if err != nil {
				t.Fatal(err)
			}
			a := config.LocalAccount{ProviderID: "openai", SourceType: "official_account", CredentialRef: rows[0].ID, CredentialEpoch: rows[0].IdentityEpoch}
			_, err = (AccountExecutor{Accounts: accounts, Runtime: runtime}).Do(context.Background(), a, []byte(`{"model":"gpt-fixture"}`), false, false, allowed.Load)
			want := codexaccounts.ErrRefreshFailed
			if revoked {
				want = codexaccounts.ErrConflict
			}
			if err != want || executed.Load() != 0 {
				t.Fatalf("empty/revoked access executed: %d error=%v", executed.Load(), err)
			}
		})
	}
}

type availabilityProtector struct{ fail atomic.Bool }

func (p *availabilityProtector) Protect(raw []byte) ([]byte, error) {
	if p.fail.Load() {
		return nil, errors.New("synthetic save failure")
	}
	return append([]byte(nil), raw...), nil
}
func (*availabilityProtector) Unprotect(raw []byte) ([]byte, error) {
	return append([]byte(nil), raw...), nil
}

func TestAccountExecutorRefreshOnlyPendingSaveAndNeedsLoginNeverExecute(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "needs-login", true: "pending-save"}[pending], func(t *testing.T) {
			var executes, refreshes atomic.Int32
			stub := &stubExecutor{refresh: func(_ context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
				refreshes.Add(1)
				if !pending {
					return nil, codexaccounts.ErrNeedsLogin
				}
				return rotatedSDK(a), nil
			}, execute: func(context.Context, *coreauth.Auth, executor.Request, executor.Options) (executor.Response, error) {
				executes.Add(1)
				return executor.Response{}, nil
			}}
			runtime := New(t.TempDir())
			runtime.executor = stub
			protector := &availabilityProtector{}
			accounts, err := codexaccounts.New(codexaccounts.Options{Path: filepath.Join(t.TempDir(), "vault.json"), Protector: protector, Refresher: runtime})
			if err != nil {
				t.Fatal(err)
			}
			defer accounts.Close()
			defer runtime.Close()
			p, _ := codexaccounts.ParseTokenImport([]byte("synthetic-refresh-only"), "refresh_token")
			rows, err := accounts.Import(context.Background(), p, "")
			if err != nil {
				t.Fatal(err)
			}
			protector.fail.Store(pending)
			_, _ = accounts.Refresh(context.Background(), rows[0].ID)
			a := config.LocalAccount{ProviderID: "openai", SourceType: "official_account", CredentialRef: rows[0].ID, CredentialEpoch: rows[0].IdentityEpoch}
			_, err = (AccountExecutor{Accounts: accounts, Runtime: runtime}).Do(context.Background(), a, []byte(`{"model":"gpt-fixture"}`), false, false, func() bool { return true })
			want := codexaccounts.ErrNeedsLogin
			if pending {
				want = codexaccounts.ErrSaveFailed
			}
			if err != want || executes.Load() != 0 || refreshes.Load() != 1 {
				t.Fatalf("blocked credential touched upstream: execution=%d refresh=%d error=%v", executes.Load(), refreshes.Load(), err)
			}
		})
	}
}

func TestAccountExecutorAccessExpiredDuringRefreshWaitNeverExecutes(t *testing.T) {
	for _, success := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed-refresh-old-access-expired", true: "refresh-result-already-expired"}[success], func(t *testing.T) {
			base := time.Now().UTC().Truncate(time.Second)
			var clock, executions atomic.Int64
			clock.Store(base.Unix())
			stub := &stubExecutor{refresh: func(_ context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
				clock.Store(base.Add(2 * time.Minute).Unix())
				if !success {
					return nil, errors.New("synthetic transient refresh failure")
				}
				r := rotatedSDK(a)
				r.Metadata["expired"] = base.Add(time.Minute).Format(time.RFC3339)
				return r, nil
			}, execute: func(context.Context, *coreauth.Auth, executor.Request, executor.Options) (executor.Response, error) {
				executions.Add(1)
				return executor.Response{}, nil
			}}
			runtime := New(t.TempDir())
			runtime.executor = stub
			accounts, err := codexaccounts.New(codexaccounts.Options{Path: filepath.Join(t.TempDir(), "vault.json"), Refresher: runtime, Now: func() time.Time { return time.Unix(clock.Load(), 0).UTC() }})
			if err != nil {
				t.Fatal(err)
			}
			defer accounts.Close()
			defer runtime.Close()
			p, err := codexaccounts.ParseImport([]byte(`{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expired":"` + base.Add(30*time.Second).Format(time.RFC3339) + `"}`))
			if err != nil {
				t.Fatal(err)
			}
			rows, err := accounts.Import(context.Background(), p, "")
			if err != nil {
				t.Fatal(err)
			}
			a := config.LocalAccount{ProviderID: "openai", SourceType: "official_account", CredentialRef: rows[0].ID, CredentialEpoch: rows[0].IdentityEpoch}
			_, err = (AccountExecutor{Accounts: accounts, Runtime: runtime}).Do(context.Background(), a, []byte(`{"model":"gpt-fixture"}`), false, false, func() bool { return true })
			if err != codexaccounts.ErrRefreshFailed || executions.Load() != 0 {
				t.Fatalf("expired access dispatched after refresh wait: execution=%d error=%v", executions.Load(), err)
			}
		})
	}
}
