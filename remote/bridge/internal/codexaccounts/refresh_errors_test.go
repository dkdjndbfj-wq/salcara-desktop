package codexaccounts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// All errors and credentials in this file are synthetic. Neither the SDK
// adapter nor the HTTP fixtures contact a provider or load local auth files.
type refreshStatusError struct {
	status int
	body   string
}

func (e refreshStatusError) Error() string   { return e.body }
func (e refreshStatusError) StatusCode() int { return e.status }

func sdkRefreshEnvelope(status int, body string) error {
	return fmt.Errorf("token refresh failed with status %d: %s", status, body)
}

func TestSDKRefreshErrorNormalizationRequiresPermanentEvidence(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"nil", nil, nil},
		{"sentinel", ErrNeedsLogin, ErrNeedsLogin},
		{"wrapped-sentinel", fmt.Errorf("private fixture: %w", ErrNeedsLogin), ErrNeedsLogin},
		{"oauth-bad-request", sdkRefreshEnvelope(400, `{"error":"invalid_grant"}`), ErrNeedsLogin},
		{"oauth-unauthorized", sdkRefreshEnvelope(401, `{"error":"invalid_grant","error_description":"private fixture"}`), ErrNeedsLogin},
		{"reused-token", sdkRefreshEnvelope(401, `{"error":{"code":"refresh_token_reused","type":"invalid_request_error"}}`), ErrNeedsLogin},
		{"expired-token", sdkRefreshEnvelope(400, `{"error":{"code":"refresh_token_expired"}}`), ErrNeedsLogin},
		{"invalid-token", sdkRefreshEnvelope(400, `{"error":{"code":"refresh_token_invalid"}}`), ErrNeedsLogin},
		{"matching-code", sdkRefreshEnvelope(400, `{"error":"invalid_grant","code":"invalid_grant"}`), ErrNeedsLogin},
		{"sdk-retry-wrapper", fmt.Errorf("token refresh failed after 3 attempts: %w", sdkRefreshEnvelope(400, `{"error":"invalid_grant"}`)), ErrNeedsLogin},
		{"typed-status", refreshStatusError{401, `{"error":{"code":"refresh_token_reused"}}`}, ErrNeedsLogin},
		{"wrapped-typed-status", fmt.Errorf("private fixture: %w", refreshStatusError{400, `{"error":"invalid_grant"}`}), ErrNeedsLogin},
		{"bare-keyword", errors.New("invalid_grant"), ErrRefreshFailed},
		{"bare-json", errors.New(`{"error":"invalid_grant"}`), ErrRefreshFailed},
		{"arbitrary-prefix", errors.New(`network error: token refresh failed with status 400: {"error":"invalid_grant"}`), ErrRefreshFailed},
		{"not-json", sdkRefreshEnvelope(400, "invalid_grant"), ErrRefreshFailed},
		{"html", sdkRefreshEnvelope(401, `<html>invalid_grant: private fixture</html>`), ErrRefreshFailed},
		{"text-in-message", sdkRefreshEnvelope(400, `{"error":{"message":"invalid_grant"}}`), ErrRefreshFailed},
		{"wrong-oauth-code", sdkRefreshEnvelope(400, `{"error":"invalid_client"}`), ErrRefreshFailed},
		{"conflicting-code", sdkRefreshEnvelope(400, `{"error":"invalid_grant","code":"temporarily_unavailable"}`), ErrRefreshFailed},
		{"non-string-code", sdkRefreshEnvelope(400, `{"error":"invalid_grant","code":[]}`), ErrRefreshFailed},
		{"duplicate-error", sdkRefreshEnvelope(400, `{"error":"temporarily_unavailable","error":"invalid_grant"}`), ErrRefreshFailed},
		{"duplicate-nested-code", sdkRefreshEnvelope(400, `{"error":{"code":"temporarily_unavailable","code":"invalid_grant"}}`), ErrRefreshFailed},
		{"trailing-json", sdkRefreshEnvelope(400, `{"error":"invalid_grant"} {}`), ErrRefreshFailed},
		{"oversized-error", sdkRefreshEnvelope(400, `{"error":"invalid_grant","padding":"`+strings.Repeat("x", maxRefreshErrorBytes)+`"}`), ErrRefreshFailed},
		{"no-error-envelope", sdkRefreshEnvelope(400, `{"code":"invalid_grant"}`), ErrRefreshFailed},
		{"array-body", sdkRefreshEnvelope(400, `[{"error":"invalid_grant"}]`), ErrRefreshFailed},
		{"empty-body", sdkRefreshEnvelope(401, ""), ErrRefreshFailed},
		{"waf-forbidden", sdkRefreshEnvelope(403, `{"error":"invalid_grant"}`), ErrRefreshFailed},
		{"rate-limited", sdkRefreshEnvelope(429, `{"error":"invalid_grant"}`), ErrRefreshFailed},
		{"server-error", sdkRefreshEnvelope(500, `{"error":"invalid_grant"}`), ErrRefreshFailed},
		{"unavailable", sdkRefreshEnvelope(503, `{"error":"invalid_grant"}`), ErrRefreshFailed},
		{"success-status", sdkRefreshEnvelope(200, `{"error":"invalid_grant"}`), ErrRefreshFailed},
		{"malformed-status", errors.New(`token refresh failed with status 4000: {"error":"invalid_grant"}`), ErrRefreshFailed},
		{"typed-status-wins", refreshStatusError{503, `token refresh failed with status 400: {"error":"invalid_grant"}`}, ErrRefreshFailed},
		{"network-wins", &net.OpError{Op: "read", Net: "tcp", Err: sdkRefreshEnvelope(400, `{"error":"invalid_grant"}`)}, ErrRefreshFailed},
		{"cancelled", fmt.Errorf("private fixture: %w", context.Canceled), context.Canceled},
		{"timeout", fmt.Errorf("private fixture: %w", context.DeadlineExceeded), context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeSDKRefreshError(tc.err); got != tc.want {
				t.Fatalf("normalization returned %v; want %v", got, tc.want)
			}
		})
	}
}

func TestNeedsLoginDoesNotInferInvalidityFromDiagnosticText(t *testing.T) {
	for _, err := range []error{
		errors.New("invalid_grant private fixture"),
		errors.New("refresh_token_reused"),
		sdkRefreshEnvelope(400, `{"error":"invalid_grant"}`),
		refreshStatusError{401, `{"error":"invalid_grant"}`},
	} {
		if needsLogin(err) {
			t.Fatal("service interpreted unnormalized transport diagnostics as a permanent verdict")
		}
	}
	if !needsLogin(fmt.Errorf("private fixture: %w", ErrNeedsLogin)) {
		t.Fatal("typed permanent verdict lost")
	}
}

type refreshOnlyExecutor struct {
	coreauth.ProviderExecutor
	refresh RefreshFunc
}

func (refreshOnlyExecutor) Identifier() string { return "codex" }
func (e refreshOnlyExecutor) Refresh(ctx context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
	return e.refresh(ctx, a)
}

func syntheticSDKRefresher(refresh RefreshFunc) Refresher {
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(refreshOnlyExecutor{refresh: refresh})
	return NewSDKRefresher(manager)
}

func TestSDKRefreshTransientFailuresPreserveCredentialsAndCleanupExclusion(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"raw-keyword", errors.New("invalid_grant private synthetic-refresh-secret")},
		{"waf-json", sdkRefreshEnvelope(403, `{"error":"refresh_token_reused"}`)},
		{"waf-html", sdkRefreshEnvelope(403, `<html>invalid_grant private synthetic-refresh-secret</html>`)},
		{"server-json", sdkRefreshEnvelope(503, `{"error":"invalid_grant"}`)},
		{"rate-limit", sdkRefreshEnvelope(429, `{"error":"invalid_grant"}`)},
		{"timeout", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, protector, path := newTestService(t, syntheticSDKRefresher(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
				return nil, tc.err
			}), nil)
			before := importFixture(t, s)
			after, err := s.Refresh(context.Background(), before.ID)
			if !errors.Is(err, ErrRefreshFailed) || after.Status != StatusRefreshFailed || after.Epoch != before.Epoch {
				t.Fatalf("transient failure changed credential state: status=%s epoch=%d err=%v", after.Status, after.Epoch, err)
			}
			record, err := s.SDKRecord(context.Background(), before.ID)
			if err != nil || record.Metadata["refresh_token"] != "synthetic-refresh-old" || record.Metadata["access_token"] != "synthetic-access-old" {
				t.Fatal("transient error discarded existing credentials")
			}
			rows, err := s.CleanupPreview(context.Background())
			if err != nil || len(rows) != 0 {
				t.Fatal("transient refresh failure became cleanup eligible")
			}
			s.Close()
			reopened, err := New(Options{Path: path, Protector: protector})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			record, err = reopened.SDKRecord(context.Background(), before.ID)
			if err != nil || record.Metadata["refresh_token"] != "synthetic-refresh-old" {
				t.Fatal("transient failure retired the persisted credential")
			}
		})
	}
}

func TestSDKRefreshVerifiedRejectionStillRetiresCredential(t *testing.T) {
	s, _, _ := newTestService(t, syntheticSDKRefresher(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
		return nil, fmt.Errorf("token refresh failed after 3 attempts: %w", sdkRefreshEnvelope(400, `{"error":"invalid_grant"}`))
	}), nil)
	before := importFixture(t, s)
	after, err := s.Refresh(context.Background(), before.ID)
	if !errors.Is(err, ErrNeedsLogin) || after.Status != StatusNeedsLogin || after.Epoch <= before.Epoch {
		t.Fatal("verified OAuth credential rejection was not retired")
	}
	record, err := s.SDKRecord(context.Background(), before.ID)
	if err != nil || record.Metadata["refresh_token"] != "" || record.Metadata["access_token"] != "synthetic-access-old" {
		t.Fatal("credential rejection discarded valid access or retained invalid refresh token")
	}
}

func TestSDKRefreshCancellationOverridesLateRejection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	refresher := syntheticSDKRefresher(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
		cancel()
		return nil, sdkRefreshEnvelope(400, `{"error":"invalid_grant"}`)
	})
	if _, err := refresher.Refresh(ctx, sdkFixture("synthetic-access", "synthetic-refresh")); !errors.Is(err, context.Canceled) {
		t.Fatal("late SDK credential rejection overrode cancellation", err)
	}
}

func TestDeviceRefreshClassifiesOnlyStructuredPermanentRejections(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"oauth", 400, `{"error":"invalid_grant"}`, ErrNeedsLogin},
		{"reused", 401, `{"error":{"code":"refresh_token_reused"}}`, ErrNeedsLogin},
		{"waf", 403, `{"error":"invalid_grant"}`, ErrLoginFailed},
		{"rate-limited", 429, `{"error":"invalid_grant"}`, ErrLoginFailed},
		{"unavailable", 503, `{"error":"invalid_grant"}`, ErrLoginFailed},
		{"html", 400, `<html>invalid_grant</html>`, ErrLoginFailed},
		{"message", 401, `{"error":{"message":"invalid_grant"}}`, ErrLoginFailed},
		{"duplicate", 400, `{"error":"invalid_grant","error":"temporarily_unavailable"}`, ErrLoginFailed},
		{"trailing", 400, `{"error":"invalid_grant"} {}`, ErrLoginFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "refresh_token" {
					t.Error("fixture expected a refresh exchange")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			connector, err := NewDeviceConnector(DeviceOptions{ClientID: "synthetic-client", Client: server.Client(), TokenURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = connector.Refresh(context.Background(), sdkFixture("synthetic-access", "synthetic-refresh")); err != tc.want {
				t.Fatalf("device response returned %v; want %v", err, tc.want)
			}
		})
	}
}

type refreshRoundTripper func(*http.Request) (*http.Response, error)

func (f refreshRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestDeviceRefreshCancellationOverridesLateRejection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	connector, err := NewDeviceConnector(DeviceOptions{ClientID: "synthetic-client", Client: &http.Client{Transport: refreshRoundTripper(func(req *http.Request) (*http.Response, error) {
		cancel()
		return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"invalid_grant"}`)), Request: req}, nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = connector.Refresh(ctx, sdkFixture("synthetic-access", "synthetic-refresh")); !errors.Is(err, context.Canceled) {
		t.Fatal("late device credential rejection overrode cancellation", err)
	}
}

func TestDeviceRefreshRequiresExplicitJSONErrorMediaType(t *testing.T) {
	for _, tc := range []struct {
		name         string
		contentTypes []string
		body         string
		want         error
	}{
		{"json", []string{"application/json"}, `{"error":"invalid_grant"}`, ErrNeedsLogin},
		{"json-charset", []string{"application/json; charset=utf-8"}, `{"error":"invalid_grant"}`, ErrNeedsLogin},
		{"problem-json", []string{"application/problem+json"}, `{"error":"invalid_grant"}`, ErrNeedsLogin},
		{"missing", nil, `{"error":"invalid_grant"}`, ErrLoginFailed},
		{"html", []string{"text/html"}, `{"error":"invalid_grant"}`, ErrLoginFailed},
		{"invalid-suffix", []string{"application/json-invalid"}, `{"error":"invalid_grant"}`, ErrLoginFailed},
		{"invalid-parameter", []string{"application/json; charset="}, `{"error":"invalid_grant"}`, ErrLoginFailed},
		{"duplicate", []string{"application/json", "text/html"}, `{"error":"invalid_grant"}`, ErrLoginFailed},
		{"oversized", []string{"application/json"}, `{"error":"invalid_grant","padding":"` + strings.Repeat("x", maxRefreshErrorBytes) + `"}`, ErrLoginFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			connector, err := NewDeviceConnector(DeviceOptions{ClientID: "synthetic-client", Client: &http.Client{Transport: refreshRoundTripper(func(req *http.Request) (*http.Response, error) {
				header := make(http.Header)
				for _, contentType := range tc.contentTypes {
					header.Add("Content-Type", contentType)
				}
				return &http.Response{StatusCode: http.StatusBadRequest, Header: header, Body: io.NopCloser(strings.NewReader(tc.body)), Request: req}, nil
			})}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = connector.Refresh(context.Background(), sdkFixture("synthetic-access", "synthetic-refresh")); err != tc.want {
				t.Fatalf("media type returned %v; want %v", err, tc.want)
			}
		})
	}
}
