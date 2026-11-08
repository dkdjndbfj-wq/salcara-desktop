package codexruntime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"salcara/bridge/internal/codexaccounts"
	"salcara/bridge/internal/config"
)

func reserveFixture(t *testing.T, stub *stubExecutor, snapshot codexaccounts.UsageSnapshot, now func() time.Time) (AccountExecutor, config.LocalAccount) {
	t.Helper()
	r := New(t.TempDir())
	r.executor = stub
	accounts, err := codexaccounts.New(codexaccounts.Options{
		Path: filepath.Join(t.TempDir(), "vault.json"), Refresher: r, Now: now,
		UsageFetcher: codexaccounts.UsageFetchFunc(func(context.Context, *coreauth.Auth) (codexaccounts.UsageFetchResult, error) {
			return codexaccounts.UsageFetchResult{Snapshot: snapshot}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(accounts.Close)
	t.Cleanup(func() { _ = r.Close() })
	parsed, err := codexaccounts.ParseImport([]byte(`{"type":"codex","access_token":"synthetic-reserve-access","refresh_token":"synthetic-reserve-refresh","expired":"2099-01-01T00:00:00Z"}`))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := accounts.Import(context.Background(), parsed, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.RefreshUsage(context.Background(), rows[0].ID); err != nil {
		t.Fatal(err)
	}
	return AccountExecutor{Accounts: accounts, Runtime: r}, config.LocalAccount{ID: "reserve-fixture", SourceType: "official_account", ProviderID: "openai", CredentialRef: rows[0].ID, CredentialEpoch: rows[0].Epoch, BaseURL: "https://chatgpt.com/backend-api/codex", Model: "gpt-fixture", QuotaReservePercent: 25}
}

func reserveLimit(remaining float64) *codexaccounts.UsageLimit {
	return &codexaccounts.UsageLimit{Primary: &codexaccounts.UsageWindow{RemainingPercent: &remaining}}
}

func TestAccountExecutorQuotaReserveUsesFreshModelWindows(t *testing.T) {
	for _, tt := range []struct {
		name    string
		body    string
		base    float64
		image   *float64
		stale   bool
		allowed bool
	}{
		{"above_reserve", `{"model":"gpt-fixture","input":"fixture"}`, 26, nil, false, true},
		{"at_reserve", `{"model":"gpt-fixture"}`, 25, nil, false, false},
		{"below_reserve", `{"model":"gpt-fixture"}`, 24, nil, false, false},
		{"stale", `{"model":"gpt-fixture"}`, 90, nil, true, false},
		{"image_tool_budget", `{"model":"gpt-fixture","tools":[{"type":"image_generation"}]}`, 90, reserveLimit(20).Primary.RemainingPercent, false, false},
		{"image_tool_above_reserve", `{"model":"gpt-fixture","tools":[{"type":"image_generation","model":"gpt-image-2"}]}`, 90, reserveLimit(80).Primary.RemainingPercent, false, true},
		{"ambiguous_model", `{"model":"gpt-fixture","Model":"gpt-image-2"}`, 90, nil, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			stub := &stubExecutor{execute: func(context.Context, *coreauth.Auth, executor.Request, executor.Options) (executor.Response, error) {
				calls++
				return executor.Response{Payload: []byte(`{"id":"fixture"}`)}, nil
			}}
			snapshot := codexaccounts.UsageSnapshot{RateLimit: reserveLimit(tt.base)}
			if tt.image != nil {
				limit := reserveLimit(*tt.image)
				limit.NormalModelSlug = "gpt-image-2"
				snapshot.AdditionalRateLimits = []codexaccounts.UsageLimit{*limit}
			}
			now := time.Now().UTC()
			e, a := reserveFixture(t, stub, snapshot, func() time.Time { return now })
			if tt.stale {
				now = now.Add(6 * time.Minute)
			}
			response, err := e.Do(context.Background(), a, []byte(tt.body), false, false, func() bool { return true })
			if response != nil {
				response.Body.Close()
			}
			if tt.allowed {
				if err != nil || calls != 1 {
					t.Fatalf("fresh sufficient quota rejected: calls=%d err=%v", calls, err)
				}
			} else if !errors.Is(err, ErrQuotaReserve) || calls != 0 {
				t.Fatalf("insufficient/unknown quota executed: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestAccountExecutorQuotaReserveRecheckedAfter401Rotation(t *testing.T) {
	calls, refreshes := 0, 0
	stub := &stubExecutor{
		execute: func(context.Context, *coreauth.Auth, executor.Request, executor.Options) (executor.Response, error) {
			calls++
			return executor.Response{}, syntheticStatusError{401}
		},
		refresh: func(_ context.Context, record *coreauth.Auth) (*coreauth.Auth, error) {
			refreshes++
			return rotatedSDK(record), nil
		},
	}
	e, a := reserveFixture(t, stub, codexaccounts.UsageSnapshot{RateLimit: reserveLimit(90)}, time.Now)
	_, err := e.Do(context.Background(), a, []byte(`{"model":"gpt-fixture"}`), false, false, func() bool { return true })
	if !errors.Is(err, ErrQuotaReserve) || calls != 1 || refreshes != 1 {
		t.Fatalf("401 retry used pre-rotation quota: calls=%d refreshes=%d err=%v", calls, refreshes, err)
	}
}
