package codexaccounts

import (
	"context"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	officialSubscriptionAccountsURL = "https://chatgpt.com/backend-api/accounts/check/v4-2023-04-27"
	officialSubscriptionsURL        = "https://chatgpt.com/backend-api/subscriptions"
	maxSubscriptionBodyBytes        = 256 << 10
)

// SubscriptionFetchResult keeps provider identity within the trusted backend.
// A nil ActiveUntil means the response did not establish a subscription term.
type SubscriptionFetchResult struct {
	ProviderAccountID string
	ActiveUntil       *time.Time
}

type SubscriptionFetcher interface {
	FetchSubscription(context.Context, *coreauth.Auth) (SubscriptionFetchResult, error)
}

type SubscriptionFetchFunc func(context.Context, *coreauth.Auth) (SubscriptionFetchResult, error)

func (f SubscriptionFetchFunc) FetchSubscription(ctx context.Context, a *coreauth.Auth) (SubscriptionFetchResult, error) {
	return f(ctx, a)
}

type officialSubscriptionFetcher struct {
	client                *http.Client
	accountsEndpoint      string
	subscriptionsEndpoint string
	allowLoopback         bool // Test-only, never accepted from an HTTP/configuration input.
}

func newOfficialSubscriptionFetcher(timeout time.Duration) SubscriptionFetcher {
	return &officialSubscriptionFetcher{
		client:                &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		accountsEndpoint:      officialSubscriptionAccountsURL,
		subscriptionsEndpoint: officialSubscriptionsURL,
	}
}

type subscriptionFetchError struct {
	code  string
	cause error
}

func (e subscriptionFetchError) Error() string { return "Codex subscription is unavailable" }
func (e subscriptionFetchError) Unwrap() error {
	if e.cause != nil {
		return e.cause
	}
	return ErrSubscriptionUnavailable
}
func subscriptionError(code string, cause error) error {
	return subscriptionFetchError{code: code, cause: cause}
}
func subscriptionErrorCode(err error) string {
	var typed subscriptionFetchError
	if errors.As(err, &typed) && safeIdentifier(typed.code, 48) != "" {
		return typed.code
	}
	switch {
	case errors.Is(err, ErrNeedsLogin):
		return "needs_login"
	case errors.Is(err, ErrConflict):
		return "account_changed"
	case errors.Is(err, ErrSaveFailed):
		return "save_failed"
	case errors.Is(err, ErrRefreshFailed):
		return "refresh_failed"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	default:
		return "unavailable"
	}
}

func (f *officialSubscriptionFetcher) FetchSubscription(ctx context.Context, record *coreauth.Auth) (SubscriptionFetchResult, error) {
	if record == nil || record.Provider != "codex" || record.Metadata == nil {
		return SubscriptionFetchResult{}, subscriptionError("invalid_credential", nil)
	}
	token, _ := record.Metadata["access_token"].(string)
	id, _ := record.Metadata["account_id"].(string)
	token, id = strings.TrimSpace(token), strings.TrimSpace(id)
	if token == "" || len(token) > maxTokenBytes || strings.ContainsAny(token, "\r\n\x00") {
		return SubscriptionFetchResult{}, subscriptionError("invalid_credential", nil)
	}
	if id == "" || len(id) > 512 || strings.ContainsAny(id, "\r\n\x00") {
		return SubscriptionFetchResult{}, subscriptionError("identity_unavailable", nil)
	}
	root, err := f.request(ctx, f.accountsEndpoint, officialSubscriptionAccountsURL, token, id, false)
	if err != nil {
		return SubscriptionFetchResult{}, err
	}
	result, err := parseSubscriptionAccounts(root, id)
	if err != nil || result.ActiveUntil != nil {
		return result, err
	}
	root, err = f.request(ctx, f.subscriptionsEndpoint, officialSubscriptionsURL, token, id, true)
	if err != nil {
		return SubscriptionFetchResult{}, err
	}
	// The preceding check proved this exact account exists. The fixed request
	// selects it with both account_id and ChatGPT-Account-Id. If the provider
	// echoes an identity, it must agree rather than override the selection.
	if err = subscriptionResponseIdentity(root, id); err != nil {
		return SubscriptionFetchResult{}, err
	}
	if subscriptionExplicitlyFree(root) {
		return SubscriptionFetchResult{}, subscriptionError("no_expiry", nil)
	}
	expiry, err := subscriptionNamedExpiry(root, "active_until")
	if err != nil {
		return SubscriptionFetchResult{}, err
	}
	if expiry == nil {
		return SubscriptionFetchResult{}, subscriptionError("no_expiry", nil)
	}
	return SubscriptionFetchResult{ProviderAccountID: id, ActiveUntil: expiry}, nil
}

func validSubscriptionEndpoint(raw, official string, allowLoopback bool) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return false
	}
	if raw == official {
		return true
	}
	if !allowLoopback || u.Scheme != "http" || u.Port() == "" {
		return false
	}
	host := net.ParseIP(u.Hostname())
	want, _ := url.Parse(official)
	return host != nil && host.IsLoopback() && u.EscapedPath() == want.EscapedPath()
}

func (f *officialSubscriptionFetcher) request(ctx context.Context, endpoint, official, token, id string, withAccountQuery bool) (map[string]any, error) {
	if endpoint == "" {
		endpoint = official
	}
	if !validSubscriptionEndpoint(endpoint, official, f.allowLoopback) {
		return nil, subscriptionError("invalid_endpoint", nil)
	}
	u, _ := url.Parse(endpoint)
	if withAccountQuery {
		u.RawQuery = url.Values{"account_id": []string{id}}.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, subscriptionError("request_failed", nil)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("ChatGPT-Account-Id", id)
	req.Header.Set("User-Agent", "salcara-bridge/codex-subscription")
	req.Header.Set("originator", "salcara")
	client := &http.Client{Timeout: 8 * time.Second}
	if f.client != nil {
		copy := *f.client
		client = &copy
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, subscriptionError(subscriptionErrorCode(ctx.Err()), ctx.Err())
		}
		return nil, subscriptionError("network", nil)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		code, accessErr := explicitUsageAccessFailure(resp)
		if ctx.Err() != nil {
			return nil, subscriptionError(subscriptionErrorCode(ctx.Err()), ctx.Err())
		}
		if code != "" {
			return nil, subscriptionError(code, nil)
		}
		if accessErr != nil {
			return nil, subscriptionError(usageErrorCode(accessErr), nil)
		}
		if resp.StatusCode == http.StatusUnauthorized {
			return nil, subscriptionError("needs_login", ErrNeedsLogin)
		}
		return nil, subscriptionError("forbidden", nil)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, subscriptionError("rate_limited", nil)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, subscriptionError("upstream_status", nil)
	}
	media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return nil, subscriptionError("invalid_content_type", nil)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSubscriptionBodyBytes+1))
	if ctx.Err() != nil {
		return nil, subscriptionError(subscriptionErrorCode(ctx.Err()), ctx.Err())
	}
	if err != nil || len(body) == 0 || len(body) > maxSubscriptionBodyBytes {
		return nil, subscriptionError("invalid_response", nil)
	}
	value, err := strictJSONLimit(body, maxSubscriptionBodyBytes)
	root, ok := value.(map[string]any)
	if err != nil || !ok {
		return nil, subscriptionError("invalid_response", nil)
	}
	return root, nil
}

func parseSubscriptionAccounts(root map[string]any, id string) (SubscriptionFetchResult, error) {
	accounts, ok := root["accounts"].(map[string]any)
	if !ok || len(accounts) == 0 || len(accounts) > 256 {
		return SubscriptionFetchResult{}, subscriptionError("invalid_response", nil)
	}
	var matched map[string]any
	for key, raw := range accounts {
		record, ok := raw.(map[string]any)
		if !ok {
			return SubscriptionFetchResult{}, subscriptionError("invalid_response", nil)
		}
		account := record
		if raw, exists := record["account"]; exists {
			account, ok = raw.(map[string]any)
			if !ok {
				return SubscriptionFetchResult{}, subscriptionError("invalid_response", nil)
			}
		}
		matches := key == id
		for _, field := range []string{"account_id", "id", "chatgpt_account_id"} {
			if value, ok := account[field].(string); ok && value == id {
				matches = true
			}
		}
		if !matches {
			continue
		}
		if matched != nil {
			return SubscriptionFetchResult{}, subscriptionError("identity_mismatch", ErrConflict)
		}
		if err := subscriptionResponseIdentity(account, id); err != nil {
			return SubscriptionFetchResult{}, err
		}
		matched = record
	}
	if matched == nil {
		return SubscriptionFetchResult{}, subscriptionError("identity_mismatch", ErrConflict)
	}
	result := SubscriptionFetchResult{ProviderAccountID: id}
	if raw, exists := matched["entitlement"]; exists && raw != nil {
		entitlement, ok := raw.(map[string]any)
		if !ok {
			return SubscriptionFetchResult{}, subscriptionError("invalid_response", nil)
		}
		if subscriptionExplicitlyFree(entitlement) {
			return result, nil
		}
		expiry, err := subscriptionNamedExpiry(entitlement, "expires_at")
		if err != nil {
			return SubscriptionFetchResult{}, err
		}
		result.ActiveUntil = expiry
	}
	return result, nil
}

func subscriptionExplicitlyFree(root map[string]any) bool {
	for _, field := range []string{"subscription_plan", "plan_type"} {
		value, _ := root[field].(string)
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "free", "chatgptfreeplan":
			return true
		}
	}
	return false
}

func subscriptionResponseIdentity(root map[string]any, id string) error {
	for _, field := range []string{"account_id", "id", "chatgpt_account_id"} {
		if raw, exists := root[field]; exists {
			value, ok := raw.(string)
			if !ok || value != id {
				return subscriptionError("identity_mismatch", ErrConflict)
			}
		}
	}
	return nil
}

func subscriptionNamedExpiry(root map[string]any, field string) (*time.Time, error) {
	raw, exists := root[field]
	if !exists || raw == nil || raw == "" {
		return nil, nil
	}
	expiry, ok := parseSubscriptionExpiry(subscriptionScalar(raw))
	if !ok {
		return nil, subscriptionError("invalid_response", nil)
	}
	return &expiry, nil
}
