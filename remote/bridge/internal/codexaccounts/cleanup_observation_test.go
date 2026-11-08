package codexaccounts

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func cleanupUsageFixture(t *testing.T, fetcher UsageFetcher) (*Service, Account) {
	t.Helper()
	s, _, _ := newTestService(t, nil, nil)
	s.usageFetcher = fetcher
	s.now = func() time.Time { return time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC) }
	return s, importFixture(t, s)
}

func TestCleanupObservedUsageFailuresRequireExplicitRevision(t *testing.T) {
	for _, tc := range []struct {
		code, reason string
		recoverable  bool
	}{
		{"workspace_revoked", "workspace_revoked", false},
		{"account_unavailable", "account_unavailable", false},
		{"invalid_credential", "account_unavailable", false},
		{"needs_login", "usage_unauthorized", false},
		{"permission_denied", "usage_forbidden", true},
	} {
		t.Run(tc.code, func(t *testing.T) {
			s, row := cleanupUsageFixture(t, UsageFetchFunc(func(context.Context, *coreauth.Auth) (UsageFetchResult, error) {
				return UsageFetchResult{}, usageError(tc.code, ErrUsageUnavailable)
			}))
			if tc.code == "needs_login" {
				s.mu.Lock()
				c := s.vault.Accounts[row.ID]
				c.RefreshToken = ""
				s.vault.Accounts[row.ID] = c
				s.mu.Unlock()
			}
			if _, err := s.RefreshUsage(context.Background(), row.ID); err == nil {
				t.Fatal("expected observed failure")
			}
			preview, err := s.CleanupPreview(context.Background())
			if err != nil || len(preview) != 1 || preview[0].Reason != tc.reason || preview[0].Recoverable != tc.recoverable || len(preview[0].Revision) != 64 {
				t.Fatal(preview, err)
			}
			current, _ := s.Get(row.ID)
			if current.Cleanup == nil || !current.Cleanup.Eligible || current.Cleanup.Reason != tc.reason || current.Cleanup.Recoverable != tc.recoverable {
				t.Fatal("list/preview disagree", current.Cleanup)
			}
			encoded, _ := json.Marshal(current)
			for _, secret := range []string{"synthetic-access", "synthetic-refresh", "explicit-fixture-account"} {
				if strings.Contains(string(encoded), secret) {
					t.Fatal("cleanup DTO leaked credential")
				}
			}
			result, err := s.Cleanup(context.Background(), []CleanupIdentity{cleanupIdentity(row)})
			if err != nil || len(result.Deleted) != 0 || len(result.Skipped) != 1 || result.Skipped[0].Reason != "not_eligible" {
				t.Fatal("legacy client broadened destructive scope", result, err)
			}
			result, err = s.Cleanup(context.Background(), []CleanupIdentity{preview[0].CleanupIdentity})
			if err != nil || len(result.Deleted) != 1 || len(result.Skipped) != 0 {
				t.Fatal("explicit revision failed", result, err)
			}
		})
	}
}

func TestCleanupUnknownCancelledAndStaleAreNotFailures(t *testing.T) {
	for _, code := range []string{"", "cancelled", "unknown_future_error", "forbidden", "network", "timeout", "rate_limited", "upstream_status", "invalid_content_type", "invalid_response", "identity_mismatch", "invalid_endpoint", "request_failed", "unavailable"} {
		t.Run(code, func(t *testing.T) {
			s, row := cleanupUsageFixture(t, UsageFetchFunc(func(context.Context, *coreauth.Auth) (UsageFetchResult, error) {
				if code != "" {
					return UsageFetchResult{}, usageError(code, ErrUsageUnavailable)
				}
				return UsageFetchResult{Snapshot: UsageSnapshot{RateLimit: quotaLimit(20, 30)}}, nil
			}))
			if rows, _ := s.CleanupPreview(context.Background()); len(rows) != 0 {
				t.Fatal("unqueried account eligible")
			}
			_, _ = s.RefreshUsage(context.Background(), row.ID)
			if code == "" {
				now := s.now().Add(time.Hour)
				s.now = func() time.Time { return now }
			}
			if rows, _ := s.CleanupPreview(context.Background()); len(rows) != 0 {
				t.Fatal("unknown/cancelled/stale is not failure", rows)
			}
			current, _ := s.Get(row.ID)
			if current.Cleanup != nil {
				t.Fatal("account DTO marked unknown as failed", current.Cleanup)
			}
		})
	}
}

func TestCleanupDoesNotPromoteCredentialRefreshNetworkFailure(t *testing.T) {
	s, row := cleanupUsageFixture(t, UsageFetchFunc(func(context.Context, *coreauth.Auth) (UsageFetchResult, error) {
		return UsageFetchResult{}, usageError("needs_login", ErrNeedsLogin)
	}))
	s.refresher = RefreshFunc(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
		return nil, errors.New("synthetic temporary connection failure")
	})
	if _, err := s.RefreshUsage(context.Background(), row.ID); !errors.Is(err, ErrRefreshFailed) {
		t.Fatal(err)
	}
	current, _ := s.Get(row.ID)
	if current.Status != StatusRefreshFailed || current.Cleanup != nil {
		t.Fatal("inconclusive recovery became cleanup candidate", current.Status, current.Cleanup)
	}
	preview, err := s.CleanupPreview(context.Background())
	if err != nil || len(preview) != 0 {
		t.Fatal(preview, err)
	}
}

func TestCleanupUnauthorizedRecoveryFailureKeepsOnlyProvenAccessRejection(t *testing.T) {
	for _, firstUnauthorized := range []bool{true, false} {
		t.Run(map[bool]string{true: "usage-401-first", false: "untested-access-only"}[firstUnauthorized], func(t *testing.T) {
			s, row := cleanupUsageFixture(t, UsageFetchFunc(func(context.Context, *coreauth.Auth) (UsageFetchResult, error) {
				return UsageFetchResult{}, usageError("needs_login", ErrNeedsLogin)
			}))
			s.refresher = RefreshFunc(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) { return nil, ErrNeedsLogin })
			var err error
			if firstUnauthorized {
				_, err = s.RefreshUsage(context.Background(), row.ID)
			} else {
				_, err = s.Refresh(context.Background(), row.ID)
			}
			if !errors.Is(err, ErrNeedsLogin) {
				t.Fatal(err)
			}
			current, _ := s.Get(row.ID)
			if current.Epoch <= row.Epoch || current.Status != StatusNeedsLogin {
				t.Fatal("fixture did not retire refresh token")
			}
			preview, err := s.CleanupPreview(context.Background())
			if err != nil || (len(preview) == 1) != firstUnauthorized {
				t.Fatal("proven and untested access confused", preview, err)
			}
			if firstUnauthorized {
				if preview[0].Reason != "usage_unauthorized" || preview[0].Recoverable {
					t.Fatal(preview)
				}
				result, err := s.Cleanup(context.Background(), []CleanupIdentity{preview[0].CleanupIdentity})
				if err != nil || len(result.Deleted) != 1 {
					t.Fatal(result, err)
				}
			}
		})
	}
}

func TestCleanupLaterNetworkFailureRetiresObservedPermissionCandidate(t *testing.T) {
	var network atomic.Bool
	s, row := cleanupUsageFixture(t, UsageFetchFunc(func(context.Context, *coreauth.Auth) (UsageFetchResult, error) {
		if network.Load() {
			return UsageFetchResult{}, usageError("network", ErrUsageUnavailable)
		}
		return UsageFetchResult{}, usageError("permission_denied", ErrUsageUnavailable)
	}))
	_, _ = s.RefreshUsage(context.Background(), row.ID)
	preview, _ := s.CleanupPreview(context.Background())
	if len(preview) != 1 {
		t.Fatal(preview)
	}
	network.Store(true)
	_, _ = s.RefreshUsage(context.Background(), row.ID)
	if current, _ := s.Get(row.ID); current.Cleanup != nil {
		t.Fatal("network failure retained cleanup eligibility", current.Cleanup)
	}
	result, err := s.Cleanup(context.Background(), []CleanupIdentity{preview[0].CleanupIdentity})
	if err != nil || len(result.Deleted) != 0 || len(result.Skipped) != 1 || result.Skipped[0].Reason != "not_eligible" {
		t.Fatal(result, err)
	}
}

func TestCleanupCancelledUsageWaiterCannotPromoteLateFirstUnauthorized(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	s, row := cleanupUsageFixture(t, UsageFetchFunc(func(context.Context, *coreauth.Auth) (UsageFetchResult, error) {
		close(entered)
		<-release
		return UsageFetchResult{}, usageError("needs_login", ErrNeedsLogin)
	}))
	var refreshes atomic.Int32
	s.refresher = RefreshFunc(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
		refreshes.Add(1)
		return sdkFixture("synthetic-new-access", "synthetic-new-refresh"), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := s.RefreshUsage(ctx, row.ID); done <- err }()
	<-entered
	s.mu.Lock()
	flightDone := s.usageFlights[row.ID].done
	s.mu.Unlock()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(release)
	select {
	case <-flightDone:
	case <-time.After(5 * time.Second):
		t.Fatal("synthetic usage worker did not settle")
	}
	preview, err := s.CleanupPreview(context.Background())
	if err != nil || len(preview) != 0 || refreshes.Load() != 0 {
		t.Fatal("late intermediate 401 treated as completed recovery", preview, err, refreshes.Load())
	}
	current, _ := s.Get(row.ID)
	if current.Cleanup != nil || current.Usage.ErrorCode != "needs_login" {
		t.Fatal("error display and cleanup proof must differ", current.Cleanup, current.Usage.ErrorCode)
	}
}

func TestCleanupUsageWorkerTimeoutCannotPromoteLateAccessRejection(t *testing.T) {
	for _, code := range []string{"needs_login", "workspace_revoked", "account_unavailable", "permission_denied", "invalid_credential"} {
		t.Run(code, func(t *testing.T) {
			s, row := cleanupUsageFixture(t, UsageFetchFunc(func(ctx context.Context, _ *coreauth.Auth) (UsageFetchResult, error) {
				<-ctx.Done()
				// Model a connector returning a response that raced with its
				// shared timeout instead of propagating the context error.
				return UsageFetchResult{}, usageError(code, ErrNeedsLogin)
			}))
			s.usageTimeout = 5 * time.Millisecond
			s.mu.Lock()
			credential := s.vault.Accounts[row.ID]
			credential.RefreshToken = ""
			s.vault.Accounts[row.ID] = credential
			s.mu.Unlock()
			var refreshes atomic.Int32
			s.refresher = RefreshFunc(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
				refreshes.Add(1)
				return nil, ErrNeedsLogin
			})
			usage, err := s.RefreshUsage(context.Background(), row.ID)
			if !errors.Is(err, ErrUsageUnavailable) || usage.ErrorCode != "timeout" || refreshes.Load() != 0 {
				t.Fatal("late auth response escaped operation timeout", usage, err, refreshes.Load())
			}
			preview, err := s.CleanupPreview(context.Background())
			if err != nil || len(preview) != 0 {
				t.Fatal("timeout became destructive evidence", preview, err)
			}
			current, err := s.Get(row.ID)
			if err != nil || current.Cleanup != nil || current.Status != StatusReady {
				t.Fatal("timeout retired usable account", current, err)
			}
		})
	}
}

func TestCleanupCancellationBetweenFirstUnauthorizedAndRecoveryDoesNotFinalize(t *testing.T) {
	s, row := cleanupUsageFixture(t, UsageFetchFunc(func(context.Context, *coreauth.Auth) (UsageFetchResult, error) {
		return UsageFetchResult{}, usageError("needs_login", ErrNeedsLogin)
	}))
	var refreshes atomic.Int32
	s.refresher = RefreshFunc(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
		refreshes.Add(1)
		return nil, ErrNeedsLogin
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, attempt, err := s.refreshUsageOnce(ctx, row.ID, nil)
	if !errors.Is(err, ErrNeedsLogin) {
		t.Fatal(err)
	}
	// Exact deterministic boundary used by RefreshUsage: first observation
	// completed, but cancellation happens before credential recovery starts.
	cancel()
	if err := s.rotateUsageCredentialAfterUnauthorized(ctx, row.ID, attempt); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	preview, err := s.CleanupPreview(context.Background())
	if err != nil || len(preview) != 0 || refreshes.Load() != 0 {
		t.Fatal("cancelled recovery published final proof", preview, err, refreshes.Load())
	}
}

func TestCleanupSecondUnauthorizedAfterSuccessfulRecoveryIsFinal(t *testing.T) {
	s, row := cleanupUsageFixture(t, UsageFetchFunc(func(context.Context, *coreauth.Auth) (UsageFetchResult, error) {
		return UsageFetchResult{}, usageError("needs_login", ErrNeedsLogin)
	}))
	var refreshes atomic.Int32
	s.refresher = RefreshFunc(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
		refreshes.Add(1)
		return sdkFixture("synthetic-rotated-access", "synthetic-rotated-refresh"), nil
	})
	if _, err := s.RefreshUsage(context.Background(), row.ID); !errors.Is(err, ErrNeedsLogin) {
		t.Fatal(err)
	}
	preview, err := s.CleanupPreview(context.Background())
	if err != nil || len(preview) != 1 || preview[0].Reason != "usage_unauthorized" || preview[0].Epoch <= row.Epoch || refreshes.Load() != 1 {
		t.Fatal("final 401 after bounded recovery not represented", preview, err, refreshes.Load())
	}
	prepared, _ := ParseImport([]byte(fixture))
	if _, err := s.Import(context.Background(), prepared, row.ID); err != nil {
		t.Fatal(err)
	}
	result, err := s.Cleanup(context.Background(), []CleanupIdentity{preview[0].CleanupIdentity})
	if err != nil || len(result.Deleted) != 0 || len(result.Skipped) != 1 || result.Skipped[0].Reason != "changed" {
		t.Fatal("final proof escaped credential identity", result, err)
	}
}

func TestCleanupQuotaExhaustionIsRecoverableAndExpiresAtReset(t *testing.T) {
	for _, kind := range []string{"overall", "spend", "model_only"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
			reset := now.Add(time.Minute)
			limited := true
			s, row := cleanupUsageFixture(t, UsageFetchFunc(func(context.Context, *coreauth.Auth) (UsageFetchResult, error) {
				limit := &UsageLimit{LimitReached: &limited, Primary: &UsageWindow{ResetsAt: &reset}}
				u := UsageSnapshot{}
				switch kind {
				case "overall":
					u.RateLimit = limit
				case "spend":
					u.SpendControl = &UsageSpendControl{Reached: &limited, IndividualLimit: &UsageSpendLimit{ResetsAt: &reset}}
				case "model_only":
					u.AdditionalRateLimits = []UsageLimit{*limit}
				}
				return UsageFetchResult{Snapshot: u}, nil
			}))
			s.now = func() time.Time { return now }
			_, _ = s.RefreshUsage(context.Background(), row.ID)
			preview, err := s.CleanupPreview(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if kind == "model_only" {
				if len(preview) != 0 {
					t.Fatal("single-model limit invalidated whole account")
				}
				return
			}
			if len(preview) != 1 || preview[0].Reason != "quota_exhausted" || !preview[0].Recoverable {
				t.Fatal(preview)
			}
			now = reset
			result, err := s.Cleanup(context.Background(), []CleanupIdentity{preview[0].CleanupIdentity})
			if err != nil || len(result.Deleted) != 0 || len(result.Skipped) != 1 || result.Skipped[0].Reason != "not_eligible" {
				t.Fatal("quota reset did not retire cleanup eligibility", result, err)
			}
		})
	}
}

func TestCleanupRevisionRejectsRepeatedSameClockFailureAndRecovery(t *testing.T) {
	for _, sequence := range []string{"same-failure", "recover-fail"} {
		t.Run(sequence, func(t *testing.T) {
			var healthy atomic.Bool
			s, row := cleanupUsageFixture(t, UsageFetchFunc(func(context.Context, *coreauth.Auth) (UsageFetchResult, error) {
				if healthy.Load() {
					return UsageFetchResult{Snapshot: UsageSnapshot{RateLimit: quotaLimit(20, 30)}}, nil
				}
				return UsageFetchResult{}, usageError("permission_denied", ErrUsageUnavailable)
			}))
			_, _ = s.RefreshUsage(context.Background(), row.ID)
			before, _ := s.CleanupPreview(context.Background())
			if len(before) != 1 {
				t.Fatal(before)
			}
			if sequence == "recover-fail" {
				healthy.Store(true)
				_, _ = s.RefreshUsage(context.Background(), row.ID)
				result, err := s.Cleanup(context.Background(), []CleanupIdentity{before[0].CleanupIdentity})
				if err != nil || len(result.Deleted) != 0 || len(result.Skipped) != 1 {
					t.Fatal("recovered account deleted", result, err)
				}
				healthy.Store(false)
			}
			_, _ = s.RefreshUsage(context.Background(), row.ID)
			after, _ := s.CleanupPreview(context.Background())
			if len(after) != 1 || after[0].Revision == before[0].Revision {
				t.Fatal("same timestamp reused cleanup revision")
			}
			result, err := s.Cleanup(context.Background(), []CleanupIdentity{before[0].CleanupIdentity})
			if err != nil || len(result.Deleted) != 0 || len(result.Skipped) != 1 || result.Skipped[0].Reason != "changed" {
				t.Fatal("old confirmation survived newer observation", result, err)
			}
		})
	}
}

func TestCleanupRevisionDoesNotInvalidateOtherAccountsAndReceiptSurvivesRestart(t *testing.T) {
	s, p, path := newTestService(t, nil, nil)
	s.usageFetcher = UsageFetchFunc(func(context.Context, *coreauth.Auth) (UsageFetchResult, error) {
		return UsageFetchResult{}, usageError("permission_denied", ErrUsageUnavailable)
	})
	row := importFixture(t, s)
	prepared, err := ParseImport([]byte(strings.NewReplacer("explicit-fixture-account", "other-synthetic-account", "synthetic-access-old", "synthetic-access-other", "synthetic-refresh-old", "synthetic-refresh-other").Replace(fixture)))
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.Import(context.Background(), prepared, "")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.RefreshUsage(context.Background(), row.ID)
	before, _ := s.CleanupPreview(context.Background())
	if len(before) != 1 {
		t.Fatal(before)
	}
	_, _ = s.RefreshUsage(context.Background(), other[0].ID)
	result, err := s.Cleanup(context.Background(), []CleanupIdentity{before[0].CleanupIdentity})
	if err != nil || len(result.Deleted) != 1 {
		t.Fatal("unrelated observation invalidated reviewed account", result, err)
	}
	s.Close()
	reopened, err := New(Options{Path: path, Protector: p})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	result, err = reopened.Cleanup(context.Background(), []CleanupIdentity{before[0].CleanupIdentity})
	if err != nil || len(result.Deleted) != 1 {
		t.Fatal("revision receipt not durable", result, err)
	}
	wrong := before[0].CleanupIdentity
	wrong.Revision = strings.Repeat("0", 64)
	result, err = reopened.Cleanup(context.Background(), []CleanupIdentity{wrong})
	if err != nil || len(result.Deleted) != 0 {
		t.Fatal("different revision replayed receipt", result, err)
	}
}

func TestCleanupRevisionFromPriorProcessCannotDeleteLiveAccount(t *testing.T) {
	s, p, path := newTestService(t, nil, nil)
	row := cleanupNeedsLogin(t, s)
	s.mu.Lock()
	err := writeVault(path, p, s.vault)
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	preview, _ := s.CleanupPreview(context.Background())
	s.Close()
	reopened, err := New(Options{Path: path, Protector: p})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	result, err := reopened.Cleanup(context.Background(), []CleanupIdentity{preview[0].CleanupIdentity})
	if err != nil || len(result.Deleted) != 0 || len(result.Skipped) != 1 || result.Skipped[0].Reason != "changed" {
		t.Fatal("previous process confirmation survived", result, err)
	}
	if _, err := reopened.Get(row.ID); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupExcludesWholeUnauthorizedRecoveryScope(t *testing.T) {
	s, row := cleanupUsageFixture(t, UsageFetchFunc(func(context.Context, *coreauth.Auth) (UsageFetchResult, error) {
		return UsageFetchResult{}, usageError("permission_denied", ErrUsageUnavailable)
	}))
	_, _ = s.RefreshUsage(context.Background(), row.ID)
	preview, _ := s.CleanupPreview(context.Background())
	// Deterministically model the first 401's completed usage flight before
	// the outer RefreshUsage call starts credential recovery. Neither worker
	// map is occupied, but the whole recovery operation remains active.
	s.mu.Lock()
	s.cleanupUsageWork[row.ID] = 1
	s.mu.Unlock()
	if rows, _ := s.CleanupPreview(context.Background()); len(rows) != 0 {
		t.Fatal("401 recovery gap eligible")
	}
	result, err := s.Cleanup(context.Background(), []CleanupIdentity{preview[0].CleanupIdentity})
	if err != nil || len(result.Deleted) != 0 || len(result.Skipped) != 1 || result.Skipped[0].Reason != "not_eligible" {
		t.Fatal(result, err)
	}
}

func TestOfficialUsageCleanupClassifiesOnlyExplicitStructuredAccessErrors(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		status           int
	}{
		{"workspace", `{"error":{"code":"workspace_access_revoked","message":"private-provider-detail"}}`, "workspace_revoked", 403},
		{"account", `{"code":"account_deactivated"}`, "account_unavailable", 401},
		{"generic403", `{"error":{"message":"workspace_access_revoked"}}`, "forbidden", 403},
		{"permission403", `{"error":{"code":"insufficient_permissions"}}`, "permission_denied", 403},
		{"unknown403", `{"error":{"code":"new_unknown_reason"}}`, "forbidden", 403},
		{"duplicate", `{"error":{"code":"workspace_access_revoked","code":"other"}}`, "forbidden", 403},
		{"conflicting", `{"code":"other","error":{"code":"workspace_access_revoked"}}`, "forbidden", 403},
		{"wrongRootCodeType", `{"code":123,"error":{"code":"workspace_access_revoked"}}`, "forbidden", 403},
		{"wrongNestedCodeType", `{"code":"workspace_access_revoked","error":{"code":null}}`, "forbidden", 403},
		{"wrongErrorShape", `{"code":"workspace_access_revoked","error":"upstream_timeout"}`, "forbidden", 403},
		{"paddedCode", `{"error":{"code":" workspace_access_revoked "}}`, "forbidden", 403},
		{"oversized", `{"code":"workspace_access_revoked","message":"` + strings.Repeat("x", 8192) + `"}`, "forbidden", 403},
		{"transient429", `{"code":"workspace_access_revoked"}`, "rate_limited", 429},
		{"generic401", `{"error":{"message":"private-provider-detail"}}`, "needs_login", 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			fetcher := &officialUsageFetcher{client: server.Client(), endpoint: server.URL + "/usage", allowLoopback: true}
			_, err := fetcher.FetchUsage(context.Background(), &coreauth.Auth{Provider: "codex", Metadata: map[string]any{"access_token": "synthetic"}})
			if err == nil || usageErrorCode(err) != tc.want || strings.Contains(err.Error(), "private-provider-detail") {
				t.Fatal("unsafe error classification", err, usageErrorCode(err))
			}
			if tc.want == "needs_login" && !errors.Is(err, ErrNeedsLogin) {
				t.Fatal("generic401 lost existing recovery")
			}
		})
	}
}

func TestOfficialUsageCleanupRequiresExactJSONMediaType(t *testing.T) {
	for _, contentType := range []string{"application/jsonp", "application/json+html", "application/json; broken", "text/plain", ""} {
		t.Run(contentType, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", contentType)
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":{"code":"workspace_access_revoked"}}`))
			}))
			defer server.Close()
			fetcher := &officialUsageFetcher{client: server.Client(), endpoint: server.URL + "/usage", allowLoopback: true}
			_, err := fetcher.FetchUsage(context.Background(), &coreauth.Auth{Provider: "codex", Metadata: map[string]any{"access_token": "synthetic"}})
			if err == nil || usageErrorCode(err) != "forbidden" {
				t.Fatal("ambiguous media type became destructive evidence", err, usageErrorCode(err))
			}
		})
	}
}

func TestOfficialUsageCleanupRejectsDuplicateContentType(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		for _, contentTypes := range [][]string{{"application/json", "text/html"}, {"text/html", "application/json"}, {"application/json", "application/json"}} {
			t.Run(http.StatusText(status)+"/"+strings.Join(contentTypes, ","), func(t *testing.T) {
				transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": contentTypes}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"workspace_access_revoked"}}`))}, nil
				})
				fetcher := &officialUsageFetcher{client: &http.Client{Transport: transport}}
				s, row := cleanupUsageFixture(t, fetcher)
				s.mu.Lock()
				credential := s.vault.Accounts[row.ID]
				credential.RefreshToken = ""
				s.vault.Accounts[row.ID] = credential
				s.mu.Unlock()
				usage, err := s.RefreshUsage(context.Background(), row.ID)
				want := "forbidden"
				if status == http.StatusUnauthorized {
					want = "invalid_content_type"
				}
				if err == nil || usage.ErrorCode != want {
					t.Fatal("ambiguous headers promoted auth rejection", usage.ErrorCode, err)
				}
				preview, err := s.CleanupPreview(context.Background())
				if err != nil || len(preview) != 0 {
					t.Fatal("ambiguous headers became cleanup evidence", preview, err)
				}
			})
		}
	}
}

type cleanupUsageBodyError struct{ err error }

func (r cleanupUsageBodyError) Read([]byte) (int, error) { return 0, r.err }

func TestOfficialUsageCleanupRejectsInconclusiveUnauthorizedBodies(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, body, want string
		bodyErr                       error
	}{
		{"body-timeout", "application/json", `{"error":{"code":"workspace_access_revoked"}}`, "timeout", context.DeadlineExceeded},
		{"body-cancelled", "application/json", `{"error":"unauthorized"}`, "cancelled", context.Canceled},
		{"truncated-body", "application/json", `{"error":`, "invalid_response", io.ErrUnexpectedEOF},
		{"invalid-json", "application/json", `{"error":`, "invalid_response", nil},
		{"empty-body", "application/json", "", "invalid_response", nil},
		{"duplicate-fields", "application/json", `{"error":"one","error":"two"}`, "invalid_response", nil},
		{"oversized-body", "application/json", `{"error":"` + strings.Repeat("x", 8192) + `"}`, "invalid_response", nil},
		{"html-page", "text/html", `<h1>gateway authorization failed</h1>`, "invalid_content_type", nil},
		{"jsonp-type", "application/jsonp", `{"error":"unauthorized"}`, "invalid_content_type", nil},
		{"ordinary-json", "application/json; charset=utf-8", `{"error":{"message":"unauthorized"}}`, "needs_login", nil},
		{"ordinary-string-error", "application/json", `{"error":"unauthorized"}`, "needs_login", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
				var reader io.Reader = strings.NewReader(tc.body)
				if tc.bodyErr != nil {
					reader = io.MultiReader(reader, cleanupUsageBodyError{tc.bodyErr})
				}
				return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{"Content-Type": []string{tc.contentType}}, Body: io.NopCloser(reader)}, nil
			})
			fetcher := &officialUsageFetcher{client: &http.Client{Transport: transport}}
			s, row := cleanupUsageFixture(t, fetcher)
			s.mu.Lock()
			credential := s.vault.Accounts[row.ID]
			credential.RefreshToken = ""
			s.vault.Accounts[row.ID] = credential
			s.mu.Unlock()
			usage, err := s.RefreshUsage(context.Background(), row.ID)
			if err == nil || usage.ErrorCode != tc.want {
				t.Fatal("unexpected sanitized classification", usage.ErrorCode, err)
			}
			preview, err := s.CleanupPreview(context.Background())
			if err != nil || (len(preview) == 1) != (tc.want == "needs_login") {
				t.Fatal("inconclusive response became cleanup evidence or ordinary 401 lost recovery", preview, err)
			}
		})
	}
}
