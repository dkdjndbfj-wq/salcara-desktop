package codexruntime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"salcara/bridge/internal/codexaccounts"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	sdkRoundTripperContextKey = "cliproxy.roundtripper"
	officialCodexTokenURL     = "https://auth.openai.com/oauth/token"
	// This is the public legacy Codex CLI OAuth client used by the pinned MIT
	// CLIProxyAPI v7.2.157 flow. It is not a Codex Desktop identity.
	legacyCodexClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
)

var errRuntimeRedirect = errors.New("Codex provider redirect rejected")

// runtimeRedirectGuard prevents net/http from ever seeing a redirect response,
// so it cannot replay an account Authorization header or request body to a
// Location target. The error is deliberately constant and contains neither the
// source URL, redirect target, response body, nor credential material.
type runtimeRedirectGuard struct {
	base http.RoundTripper
}

func (t runtimeRedirectGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil || req.URL.Host == "" || req.URL.User != nil || !validRuntimeScheme(req.URL.Scheme, req.URL.Hostname()) {
		return nil, ErrUnavailable
	}
	base := t.base
	if base == nil {
		return nil, ErrUnavailable
	}
	resp, err := base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < http.StatusBadRequest {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, errRuntimeRedirect
	}
	return resp, nil
}

func validRuntimeScheme(scheme, host string) bool {
	if strings.EqualFold(scheme, "https") {
		return true
	}
	if !strings.EqualFold(scheme, "http") {
		return false
	}
	ip := net.ParseIP(host)
	return strings.EqualFold(host, "localhost") || ip != nil && ip.IsLoopback()
}

// newRuntimeHTTPTransport is intentionally independent from
// http.DefaultTransport. In particular it does not inherit environment proxy
// settings that could receive bearer credentials outside Salcara's configured
// account path.
func newRuntimeHTTPTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

func (r *Runtime) withSDKTransport(ctx context.Context, auth *coreauth.Auth) (context.Context, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// The pinned SDK gives Auth.ProxyURL precedence over its documented context
	// RoundTripper hook. Salcara account records never set it; rejecting it here
	// prevents an injected SDK record from bypassing the redirect guard.
	if auth != nil && strings.TrimSpace(auth.ProxyURL) != "" {
		return nil, ErrUnavailable
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrUnavailable
	}
	if r.transport == nil {
		r.transport = newRuntimeHTTPTransport()
	}
	return context.WithValue(ctx, sdkRoundTripperContextKey, http.RoundTripper(runtimeRedirectGuard{base: r.transport})), nil
}

// refresh uses Salcara's bounded token response parser and a no-redirect client.
// The pinned SDK's CodexExecutor.Refresh constructs its own http.Client and does
// not consume cliproxy.roundtripper, so invoking it would leave refresh as the
// sole credential-bearing path able to follow a provider redirect.
func (r *Runtime) refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	if auth == nil || auth.Provider != "codex" {
		return nil, ErrUnavailable
	}
	safeCtx, err := r.withSDKTransport(ctx, auth)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	// Tests inject the package-local executor before SDK bootstrap to exercise
	// refresh ownership and identity fences without network traffic. A real SDK
	// executor is always installed together with r.service, so production never
	// enters this seam and therefore never reaches the SDK's unsafe refresh
	// client.
	injected, service := r.executor, r.service
	tokenURL := r.refreshTokenURL
	if tokenURL == "" {
		tokenURL = officialCodexTokenURL
	}
	transport := http.RoundTripper(runtimeRedirectGuard{base: r.transport})
	r.mu.Unlock()
	if injected != nil && service == nil {
		return injected.Refresh(safeCtx, auth)
	}
	connector, err := codexaccounts.NewDeviceConnector(codexaccounts.DeviceOptions{
		ClientID: legacyCodexClientID,
		TokenURL: tokenURL,
		Client: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
	})
	if err != nil {
		return nil, ErrUnavailable
	}
	refreshed, err := connector.Refresh(safeCtx, auth)
	if err == nil {
		applyTrustedRefreshIdentity(refreshed)
		return refreshed, nil
	}
	if safeCtx.Err() != nil {
		return nil, safeCtx.Err()
	}
	if errors.Is(err, codexaccounts.ErrNeedsLogin) {
		return nil, codexaccounts.ErrNeedsLogin
	}
	return nil, ErrUnavailable
}

// applyTrustedRefreshIdentity restores the pinned SDK's behavior of deriving
// the account id from an ID token returned by the authenticated token endpoint.
// Imported/JWT-only claims remain non-authoritative elsewhere; this helper runs
// only after the fixed, no-redirect refresh exchange succeeds.
func applyTrustedRefreshIdentity(record *coreauth.Auth) {
	if record == nil || record.Metadata == nil {
		return
	}
	token, _ := record.Metadata["id_token"].(string)
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(parts[1]) == 0 || len(parts[1]) > 64<<10 {
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(raw) == 0 || len(raw) > 64<<10 {
		return
	}
	var claims struct {
		Email string `json:"email"`
		Auth  struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return
	}
	accountID := strings.TrimSpace(claims.Auth.AccountID)
	if accountID != "" && len(accountID) <= 512 && !strings.ContainsAny(accountID, "\r\n\x00") {
		record.Metadata["account_id"] = accountID
	}
	email := strings.TrimSpace(claims.Email)
	if email != "" && len(email) <= 320 && !strings.ContainsAny(email, "\r\n\x00") {
		record.Metadata["email"] = email
		record.Label = email
	}
}
