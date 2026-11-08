package codexaccounts

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
)

const maxRefreshErrorBytes = 8192

// definitiveRefreshRejection only accepts the OAuth credential-rejection
// statuses and exact machine codes. In particular a proxy/WAF 403, a 429, or
// any 5xx cannot retire a token even if its body repeats an authentication code.
func definitiveRefreshRejection(status int, body map[string]any) bool {
	if status != http.StatusBadRequest && status != http.StatusUnauthorized {
		return false
	}
	var code string
	switch value := body["error"].(type) {
	case string:
		code = value
	case map[string]any:
		code, _ = value["code"].(string)
	default:
		return false
	}
	if other, exists := body["code"]; exists && other != code {
		return false
	}
	switch code {
	case "invalid_grant", "refresh_token_reused", "refresh_token_expired", "refresh_token_invalid":
		return true
	default:
		return false
	}
}

func refreshErrorBody(raw string) map[string]any {
	if len(raw) == 0 || len(raw) > maxRefreshErrorBytes {
		return nil
	}
	value, err := strictJSONLimit([]byte(raw), maxRefreshErrorBytes)
	if err != nil {
		return nil
	}
	body, _ := value.(map[string]any)
	return body
}

// normalizeSDKRefreshError is the sole compatibility boundary for the pinned
// CLIProxyAPI v7.2.157 refresh transport. Typed statuses win; the legacy SDK
// otherwise exposes status plus response body in one exact error envelope.
// Parse that envelope only, never search arbitrary diagnostics for keywords.
// No provider body or private diagnostic is returned through this boundary.
func normalizeSDKRefreshError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		return ErrRefreshFailed
	}
	var statusError interface {
		error
		StatusCode() int
	}
	if errors.As(err, &statusError) {
		if definitiveRefreshRejection(statusError.StatusCode(), refreshErrorBody(statusError.Error())) {
			return ErrNeedsLogin
		}
		return ErrRefreshFailed
	}
	if errors.Is(err, ErrNeedsLogin) {
		return ErrNeedsLogin
	}
	const prefix = "token refresh failed with status "
	for depth := 0; err != nil && depth < 8; depth++ {
		text := err.Error()
		if strings.HasPrefix(text, prefix) {
			rest := text[len(prefix):]
			if len(rest) < 5 || rest[3:5] != ": " {
				return ErrRefreshFailed
			}
			status, parseErr := strconv.Atoi(rest[:3])
			if parseErr == nil && definitiveRefreshRejection(status, refreshErrorBody(rest[5:])) {
				return ErrNeedsLogin
			}
			return ErrRefreshFailed
		}
		err = errors.Unwrap(err)
	}
	return ErrRefreshFailed
}
