package codexaccounts

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const officialResetCreditConsumeURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"

// ResetCreditConsumer is a trusted backend boundary. Implementations must use
// the same redeemRequestID for retries, and must not log credentials or bodies.
type ResetCreditConsumer interface {
	ConsumeResetCredit(context.Context, *coreauth.Auth, string) error
}

type ResetCreditConsumeFunc func(context.Context, *coreauth.Auth, string) error

func (f ResetCreditConsumeFunc) ConsumeResetCredit(ctx context.Context, a *coreauth.Auth, id string) error {
	return f(ctx, a, id)
}

type officialResetCreditConsumer struct {
	client   *http.Client
	endpoint string
}

func newOfficialResetCreditConsumer(timeout time.Duration) ResetCreditConsumer {
	var transport http.RoundTripper = http.DefaultTransport
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		clone := base.Clone()
		// The official account path must not inherit an ambient HTTP proxy.
		clone.Proxy = nil
		transport = clone
	}
	return &officialResetCreditConsumer{endpoint: officialResetCreditConsumeURL, client: &http.Client{
		Transport: transport, Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// NewResetCreditHTTPConsumer allows an explicitly supplied transport to be used
// by synthetic HTTP fixtures. The endpoint remains pinned to OpenAI HTTPS.
func NewResetCreditHTTPConsumer(transport http.RoundTripper, timeout time.Duration) ResetCreditConsumer {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	consumer := newOfficialResetCreditConsumer(timeout).(*officialResetCreditConsumer)
	if transport != nil {
		consumer.client.Transport = transport
	}
	return consumer
}

type resetCreditError struct {
	code  string
	cause error
}

func (e resetCreditError) Error() string { return "Codex quota reset unavailable" }
func (e resetCreditError) Unwrap() error { return e.cause }

func resetError(code string, cause error) error { return resetCreditError{code, cause} }

// ResetCreditErrorCode is a bounded identifier, never an upstream response body.
func ResetCreditErrorCode(err error) string {
	var typed resetCreditError
	if errors.As(err, &typed) && safeIdentifier(typed.code, 48) != "" {
		return typed.code
	}
	switch {
	case errors.Is(err, ErrNeedsLogin):
		return "needs_login"
	case errors.Is(err, ErrResetUnknown):
		return "result_unknown"
	case errors.Is(err, ErrConflict):
		return "account_changed"
	case errors.Is(err, ErrSaveFailed):
		return "save_failed"
	case errors.Is(err, ErrNotFound):
		return "account_removed"
	case errors.Is(err, context.DeadlineExceeded):
		return "result_unknown"
	case errors.Is(err, context.Canceled):
		return "result_unknown"
	default:
		return "unavailable"
	}
}

func (f *officialResetCreditConsumer) ConsumeResetCredit(ctx context.Context, auth *coreauth.Auth, redeemID string) error {
	if auth == nil || auth.Provider != "codex" || auth.Metadata == nil || !validRedeemRequestID(redeemID) {
		return resetError("invalid_credential", ErrResetUnavailable)
	}
	if auth.Attributes["api_key"] != "" || auth.Metadata["api_key"] != nil || auth.Metadata["OPENAI_API_KEY"] != nil || auth.Metadata["auth_mode"] == "apikey" || auth.Metadata["auth_mode"] == "api_key" {
		return resetError("api_key_unsupported", ErrResetUnavailable)
	}
	token, _ := auth.Metadata["access_token"].(string)
	accountID, _ := auth.Metadata["account_id"].(string)
	if token == "" || len(token) > maxTokenBytes || strings.HasPrefix(token, "sk-") || strings.ContainsAny(token, "\r\n\x00") || len(accountID) > 512 || strings.ContainsAny(accountID, "\r\n\x00") {
		return resetError("invalid_credential", ErrResetUnavailable)
	}
	endpoint := f.endpoint
	if endpoint == "" {
		endpoint = officialResetCreditConsumeURL
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.String() != officialResetCreditConsumeURL {
		return resetError("invalid_endpoint", ErrResetUnavailable)
	}
	body, _ := json.Marshal(map[string]string{"redeem_request_id": redeemID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return resetError("request_failed", ErrResetUnavailable)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Referer", "https://chatgpt.com/")
	req.Header.Set("User-Agent", "salcara-bridge/codex-reset")
	req.Header.Set("OpenAI-Beta", "codex-1")
	req.Header.Set("originator", "salcara")
	if accountID != "" {
		req.Header.Set("ChatGPT-Account-Id", accountID)
	}
	client := f.client
	if client == nil {
		client = newOfficialResetCreditConsumer(30 * time.Second).(*officialResetCreditConsumer).client
	}
	resp, err := client.Do(req)
	if err != nil {
		return resetError("result_unknown", ErrResetUnavailable)
	}
	defer resp.Body.Close()
	// Drain only a bounded amount. Provider diagnostics and credentials never
	// become public errors. A successful status is sufficient for this action.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxUsageBodyBytes))
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return resetError("needs_login", ErrNeedsLogin)
	case http.StatusForbidden:
		return resetError("forbidden", ErrResetUnavailable)
	case http.StatusConflict:
		return resetError("provider_conflict", ErrResetUnavailable)
	case http.StatusTooManyRequests:
		return resetError("rate_limited", ErrResetUnavailable)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if resp.StatusCode >= 500 {
		return resetError("result_unknown", ErrResetUnavailable)
	}
	return resetError("upstream_rejected", ErrResetUnavailable)
}

func validRedeemRequestID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func newRedeemRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", ErrSaveFailed
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}
