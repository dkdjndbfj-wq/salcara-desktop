package codexaccounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func importCheckService(t *testing.T, fetcher UsageFetcher) *Service {
	t.Helper()
	s, err := New(Options{Path: filepath.Join(t.TempDir(), "synthetic.vault"), Protector: &testProtector{}, UsageFetcher: fetcher, Refresher: RefreshFunc(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
		t.Error("check must never rotate credentials")
		return nil, ErrRefreshFailed
	})})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}
func checkPrepared(t *testing.T, count int) *PreparedImport {
	t.Helper()
	rows := make([]map[string]any, count)
	for i := range rows {
		rows[i] = map[string]any{"access_token": fmt.Sprintf("synthetic-access-%d", i), "email": fmt.Sprintf("fixture-%d@example.test", i), "account_id": fmt.Sprintf("synthetic-account-%d", i)}
	}
	data, _ := json.Marshal(rows)
	p, err := ParseImport(data)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func checkedPlanResult(raw string) UsageFetchResult {
	return UsageFetchResult{Snapshot: UsageSnapshot{Verified: true, Source: UsageSourceOfficial, Plan: UsagePlan{Raw: raw}}}
}

func TestImportCheckClassificationAndNoPersistence(t *testing.T) {
	for _, tc := range []struct {
		name, want, code                   string
		refresh, expired, emptyAccess, web bool
		result                             UsageFetchResult
		err                                error
	}{
		{name: "valid", want: "verified", code: "authenticated", result: checkedPlanResult("plus")},
		{name: "rate-window-zero", want: "limited", code: "quota_limited", result: UsageFetchResult{Snapshot: UsageSnapshot{RateLimit: &UsageLimit{Primary: &UsageWindow{RemainingPercent: ptrFloat(0)}}}}},
		{name: "spend-limit", want: "limited", code: "quota_limited", result: UsageFetchResult{Snapshot: UsageSnapshot{SpendControl: &UsageSpendControl{Reached: ptrBool(true)}}}},
		{name: "invalid", want: "invalid", code: "unauthorized", err: usageError("needs_login", ErrNeedsLogin)},
		{name: "refreshable-401", want: "needs_refresh", code: "refresh_required", refresh: true, err: usageError("needs_login", ErrNeedsLogin)},
		{name: "refresh-only", want: "needs_refresh", code: "refresh_required", refresh: true, emptyAccess: true},
		{name: "expired-refreshable", want: "needs_refresh", code: "refresh_required", refresh: true, expired: true},
		{name: "expired-access-still-authenticates", want: "verified", code: "authenticated", expired: true, result: checkedPlanResult("pro")},
		{name: "web-quota", want: "quota_only", code: "web_session", web: true, result: checkedPlanResult("free")},
		{name: "web-expired-hint-not-cli-refreshable", want: "quota_only", code: "web_session", web: true, refresh: true, expired: true, result: checkedPlanResult("free")},
		{name: "web-unauthorized-not-cli-refreshable", want: "invalid", code: "unauthorized", web: true, refresh: true, err: usageError("needs_login", ErrNeedsLogin)},
		{name: "403", want: "unknown", code: "forbidden", err: usageError("forbidden", ErrUsageUnavailable)},
		{name: "429", want: "unknown", code: "rate_limited", err: usageError("rate_limited", ErrUsageUnavailable)},
		{name: "network", want: "unknown", code: "network", err: usageError("network", errors.New("private upstream body"))},
		{name: "unknown-error", want: "unknown", code: "unavailable", err: errors.New("synthetic-access-0 synthetic-account-0")},
		{name: "typed-secret-error", want: "unknown", code: "unavailable", err: usageError("synthetic-access-0", ErrUsageUnavailable)},
		{name: "identity", want: "unknown", code: "identity_mismatch", result: UsageFetchResult{ProviderAccountID: "other-private-account"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			s := importCheckService(t, UsageFetchFunc(func(context.Context, *coreauth.Auth) (UsageFetchResult, error) {
				calls.Add(1)
				return tc.result, tc.err
			}))
			p := checkPrepared(t, 1)
			if tc.refresh {
				p.records[0].RefreshToken = "synthetic-refresh-private"
			}
			if tc.expired {
				p.records[0].ExpiresAt = time.Unix(1, 0)
			}
			if tc.emptyAccess {
				p.records[0].AccessToken = ""
			}
			if tc.web {
				p.records[0].TokenSourceMode = "chatgpt_web_session"
			}
			before := cloneVault(s.vault)
			got, err := s.CheckImport(context.Background(), p)
			if err != nil || len(got.Accounts) != 1 || got.Accounts[0].Status != tc.want || got.Accounts[0].Code != tc.code {
				t.Fatal(got, err)
			}
			if (calls.Load() == 0) != (tc.emptyAccess || tc.refresh && tc.expired && !tc.web) {
				t.Fatal("wrong probe/skip", calls.Load())
			}
			if !reflect.DeepEqual(before, s.vault) || len(s.usage) != 0 || len(s.pending) != 0 {
				t.Fatal("check mutated vault/cache")
			}
			if _, err := os.Stat(s.path); !os.IsNotExist(err) {
				t.Fatal("check wrote a credential file", err)
			}
			data, _ := json.Marshal(got)
			for _, secret := range []string{"synthetic-access-0", "synthetic-refresh-private", "synthetic-account-0", "other-private-account", "private upstream body"} {
				if strings.Contains(string(data), secret) {
					t.Fatal("private check output")
				}
			}
		})
	}
}

func TestImportCheckRedactsCrossRecordNamesAndUnknownPlan(t *testing.T) {
	p := checkPrepared(t, 2)
	p.records[1].AccessToken = strings.Repeat("synthetic-long-token-", 12)
	p.records[0].Name = "label " + p.records[1].AccessToken
	p.records[1].Name = "label " + p.records[0].AccountID
	s := importCheckService(t, UsageFetchFunc(func(context.Context, *coreauth.Auth) (UsageFetchResult, error) {
		return checkedPlanResult("synthetic-access-1"), nil
	}))
	got, err := s.CheckImport(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	for i, row := range got.Accounts {
		if row.Index != i || row.Name != "Codex account" || row.Plan.Raw != "" || row.Plan.Family != "unknown" {
			t.Fatal(row)
		}
	}
}

func TestImportCheckHTTPOnlyUsesTrustedUsageEndpoint(t *testing.T) {
	for _, status := range []int{200, 401, 403, 429, 502} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "GET" || r.URL.Path != "/usage" || r.Header.Get("Authorization") != "Bearer synthetic-access-0" || r.Header.Get("ChatGPT-Account-Id") != "synthetic-account-0" {
					t.Error("unexpected provider request")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				if status == 200 {
					_, _ = w.Write([]byte(`{"plan_type":"business","account_id":"synthetic-account-0"}`))
				} else {
					_, _ = w.Write([]byte(`{"error":"private upstream body"}`))
				}
			}))
			defer server.Close()
			s := importCheckService(t, &officialUsageFetcher{client: server.Client(), endpoint: server.URL + "/usage", allowLoopback: true})
			got, err := s.CheckImport(context.Background(), checkPrepared(t, 1))
			if err != nil || calls.Load() != 1 {
				t.Fatal(got, err)
			}
			want := "unknown"
			if status == 200 {
				want = "verified"
			}
			if status == 401 {
				want = "invalid"
			}
			if got.Accounts[0].Status != want {
				t.Fatal(got)
			}
			data, _ := json.Marshal(got)
			if strings.Contains(string(data), "private upstream body") {
				t.Fatal("provider body exposed")
			}
		})
	}
}

func TestImportCheckGlobalConcurrencyCancelAndNoLockHeldAcrossNetwork(t *testing.T) {
	var inflight, maxInflight atomic.Int32
	entered := make(chan struct{}, 3)
	s := importCheckService(t, UsageFetchFunc(func(ctx context.Context, _ *coreauth.Auth) (UsageFetchResult, error) {
		n := inflight.Add(1)
		defer inflight.Add(-1)
		for old := maxInflight.Load(); n > old; old = maxInflight.Load() {
			if maxInflight.CompareAndSwap(old, n) {
				break
			}
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return UsageFetchResult{}, ctx.Err()
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := checkPrepared(t, 10)
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { _, err := s.CheckImport(ctx, p); done <- err }()
	}
	for i := 0; i < 3; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("workers did not start")
		}
	}
	listed := make(chan struct{})
	go func() { _ = s.List(); close(listed) }()
	select {
	case <-listed:
	case <-time.After(time.Second):
		t.Fatal("network held service lock")
	}
	cancel()
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("cancel hung")
		}
	}
	if maxInflight.Load() > 3 || inflight.Load() != 0 {
		t.Fatal("global provider admission leaked", maxInflight.Load(), inflight.Load())
	}
}

func TestImportCheckBatchDeadlineAndCloseCancelWorkers(t *testing.T) {
	for _, closeService := range []bool{false, true} {
		t.Run(fmt.Sprint(closeService), func(t *testing.T) {
			entered := make(chan struct{}, 3)
			s := importCheckService(t, UsageFetchFunc(func(ctx context.Context, _ *coreauth.Auth) (UsageFetchResult, error) {
				entered <- struct{}{}
				<-ctx.Done()
				return UsageFetchResult{}, ctx.Err()
			}))
			p := checkPrepared(t, 10)
			if !closeService {
				got, err := s.checkImport(context.Background(), p, 20*time.Millisecond)
				if err != nil || len(got.Accounts) != 10 {
					t.Fatal(got, err)
				}
				for _, row := range got.Accounts {
					if row.Status != "unknown" || row.Code != "timeout" {
						t.Fatal(row)
					}
				}
				return
			}
			done := make(chan error, 1)
			go func() { _, err := s.CheckImport(context.Background(), p); done <- err }()
			<-entered
			s.Close()
			select {
			case err := <-done:
				if !errors.Is(err, ErrClosed) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("close leaked workers")
			}
			if _, err := s.CheckImport(context.Background(), p); !errors.Is(err, ErrClosed) {
				t.Fatal(err)
			}
		})
	}
}

func TestPreparedImportSelectionPreservesOriginalOrderAndRefreshOnlyAlignment(t *testing.T) {
	p := checkPrepared(t, 3)
	p.records[2].AccessToken = ""
	p.records[2].RefreshToken = "synthetic-refresh-only"
	p.records[2].ExpiresAt = time.Unix(1, 0)
	selected, err := p.Select([]int{2, 0})
	if err != nil || len(selected.records) != 2 || selected.records[0].Name != p.records[0].Name || !selected.RefreshOnly(1) || selected.RefreshOnly(0) {
		t.Fatal("selection changed input ordering", err)
	}
	if len(p.records) != 3 || p.RefreshOnly(1) {
		t.Fatal("selection mutated original import")
	}
	all, err := p.Select(nil)
	if err != nil || len(all.records) != 3 {
		t.Fatal(err)
	}
	for _, indices := range [][]int{{}, {0, 0}, {-1}, {3}, {0, 1, 2, 3}} {
		if _, err := p.Select(indices); !errors.Is(err, ErrInvalidImport) {
			t.Fatal("invalid selection accepted", indices, err)
		}
	}
}

func TestImportCheckOutOfOrderCompletionKeepsIndices(t *testing.T) {
	secondDone := make(chan struct{})
	s := importCheckService(t, UsageFetchFunc(func(ctx context.Context, record *coreauth.Auth) (UsageFetchResult, error) {
		if record.Metadata["access_token"] == "synthetic-access-0" {
			select {
			case <-secondDone:
			case <-ctx.Done():
				return UsageFetchResult{}, ctx.Err()
			}
			return checkedPlanResult("plus"), nil
		}
		close(secondDone)
		return checkedPlanResult("pro"), nil
	}))
	result, err := s.CheckImport(context.Background(), checkPrepared(t, 2))
	if err != nil || result.Accounts[0].Index != 0 || result.Accounts[0].Plan.Family != "plus" || result.Accounts[1].Index != 1 || result.Accounts[1].Plan.Family != "pro" {
		t.Fatal(result, err)
	}
}

func TestImportCheckBatchAdmissionIsBoundedAndReleased(t *testing.T) {
	release := make(chan struct{})
	s := importCheckService(t, UsageFetchFunc(func(ctx context.Context, _ *coreauth.Auth) (UsageFetchResult, error) {
		select {
		case <-release:
			return checkedPlanResult("plus"), nil
		case <-ctx.Done():
			return UsageFetchResult{}, ctx.Err()
		}
	}))
	p := checkPrepared(t, 1)
	waitForActive := func(want int) {
		t.Helper()
		deadline, ticker := time.NewTimer(time.Second), time.NewTicker(time.Millisecond)
		defer deadline.Stop()
		defer ticker.Stop()
		for {
			s.mu.Lock()
			count := len(s.importChecks)
			s.mu.Unlock()
			if count == want {
				return
			}
			select {
			case <-deadline.C:
				t.Fatalf("active batches = %d, want %d", count, want)
			case <-ticker.C:
			}
		}
	}
	cancels := make([]context.CancelFunc, maxActiveImportChecks)
	done := make([]chan error, maxActiveImportChecks)
	for i := range cancels {
		ctx, cancel := context.WithCancel(context.Background())
		cancels[i] = cancel
		defer cancel()
		done[i] = make(chan error, 1)
		go func(ctx context.Context, out chan<- error) { _, err := s.CheckImport(ctx, p); out <- err }(ctx, done[i])
	}
	waitForActive(maxActiveImportChecks)
	if _, err := s.CheckImport(context.Background(), p); !errors.Is(err, ErrImportCheckBusy) {
		t.Fatal("fifth batch was admitted", err)
	}
	cancels[0]()
	if err := <-done[0]; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	waitForActive(maxActiveImportChecks - 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	retryDone := make(chan error, 1)
	go func() { _, err := s.CheckImport(ctx, p); retryDone <- err }()
	waitForActive(maxActiveImportChecks)
	for _, stop := range cancels {
		stop()
	}
	cancel()
	for _, out := range done[1:] {
		if err := <-out; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	if err := <-retryDone; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	waitForActive(0)
	close(release)
	if got, err := s.CheckImport(context.Background(), p); err != nil || got.Accounts[0].Status != "verified" {
		t.Fatal("admission did not recover", got, err)
	}
}

func ptrFloat(value float64) *float64 { return &value }
func ptrBool(value bool) *bool        { return &value }
