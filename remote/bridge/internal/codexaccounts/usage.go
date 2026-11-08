package codexaccounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	UsageSourceOfficial = "openai_codex_usage_api"
	UsageSourceJWTHint  = "jwt_hint"
	UsageStatusFresh    = "fresh"
	UsageStatusStale    = "stale"
	UsageStatusUnknown  = "unverified"
	UsageStatusError    = "unavailable"

	officialUsageURL    = "https://chatgpt.com/backend-api/wham/usage"
	maxUsageBodyBytes   = 256 << 10
	maxAdditionalLimits = 16
)

// UsagePlan separates authenticated provider metadata from an import/JWT hint.
// Raw is emitted only when it is a bounded identifier safe for display.
type UsagePlan struct {
	Raw      string `json:"raw,omitempty"`
	Family   string `json:"family"`
	Source   string `json:"source"`
	Verified bool   `json:"verified"`
}

type UsageWindow struct {
	UsedPercent       *float64   `json:"usedPercent,omitempty"`
	RemainingPercent  *float64   `json:"remainingPercent,omitempty"`
	WindowSeconds     *int64     `json:"windowSeconds,omitempty"`
	WindowMinutes     *int64     `json:"windowMinutes,omitempty"`
	ResetAfterSeconds *int64     `json:"resetAfterSeconds,omitempty"`
	ResetsAt          *time.Time `json:"resetsAt,omitempty"`
}

type UsageLimit struct {
	ID              string       `json:"id"`
	Name            string       `json:"name,omitempty"`
	MeteredFeature  string       `json:"meteredFeature,omitempty"`
	NormalModelSlug string       `json:"normalModelSlug,omitempty"`
	Allowed         *bool        `json:"allowed,omitempty"`
	LimitReached    *bool        `json:"limitReached,omitempty"`
	Primary         *UsageWindow `json:"primary,omitempty"`
	Secondary       *UsageWindow `json:"secondary,omitempty"`
}

type UsageCredits struct {
	HasCredits *bool  `json:"hasCredits,omitempty"`
	Unlimited  *bool  `json:"unlimited,omitempty"`
	Balance    string `json:"balance,omitempty"`
}

type UsageSpendLimit struct {
	Source            string     `json:"source,omitempty"`
	Limit             string     `json:"limit,omitempty"`
	Used              string     `json:"used,omitempty"`
	Remaining         string     `json:"remaining,omitempty"`
	UsedPercent       *float64   `json:"usedPercent,omitempty"`
	RemainingPercent  *float64   `json:"remainingPercent,omitempty"`
	ResetAfterSeconds *int64     `json:"resetAfterSeconds,omitempty"`
	ResetsAt          *time.Time `json:"resetsAt,omitempty"`
}

type UsageSpendControl struct {
	Reached         *bool            `json:"reached,omitempty"`
	IndividualLimit *UsageSpendLimit `json:"individualLimit,omitempty"`
}

// UsageSnapshot is safe to serialize. It never contains provider account IDs,
// bearer tokens, response bodies, or upstream error text.
type UsageSnapshot struct {
	Status     string     `json:"status"`
	Verified   bool       `json:"verified"`
	Source     string     `json:"source"`
	ObservedAt *time.Time `json:"observedAt,omitempty"`
	// FetchStartedAt is the local start of the authenticated observation's
	// complete fetch interval, including a 401 refresh/retry. It is not supplied
	// by the provider and does not claim the exact server-side sampling instant.
	FetchStartedAt            *time.Time         `json:"fetchStartedAt,omitempty"`
	FreshUntil                *time.Time         `json:"freshUntil,omitempty"`
	LastAttemptAt             *time.Time         `json:"lastAttemptAt,omitempty"`
	Plan                      UsagePlan          `json:"plan"`
	RateLimit                 *UsageLimit        `json:"rateLimit,omitempty"`
	CodeReviewRateLimit       *UsageLimit        `json:"codeReviewRateLimit,omitempty"`
	AdditionalRateLimits      []UsageLimit       `json:"additionalRateLimits,omitempty"`
	AdditionalLimitsTruncated bool               `json:"additionalLimitsTruncated,omitempty"`
	Credits                   *UsageCredits      `json:"credits,omitempty"`
	SpendControl              *UsageSpendControl `json:"spendControl,omitempty"`
	ResetCreditsAvailable     *int64             `json:"resetCreditsAvailable,omitempty"`
	RateLimitReachedType      string             `json:"rateLimitReachedType,omitempty"`
	ErrorCode                 string             `json:"errorCode,omitempty"`
}

// UsageFetchResult keeps the provider identity private so Service can fence a
// response against the imported credential before publishing its DTO.
type UsageFetchResult struct {
	Snapshot          UsageSnapshot
	ProviderAccountID string
}

// UsageFetcher is a trusted backend boundary. The Auth argument contains the
// imported credential and must never be logged or returned to an HTTP caller.
type UsageFetcher interface {
	FetchUsage(context.Context, *coreauth.Auth) (UsageFetchResult, error)
}

type UsageFetchFunc func(context.Context, *coreauth.Auth) (UsageFetchResult, error)

func (f UsageFetchFunc) FetchUsage(ctx context.Context, a *coreauth.Auth) (UsageFetchResult, error) {
	return f(ctx, a)
}

type officialUsageFetcher struct {
	client        *http.Client
	endpoint      string
	now           func() time.Time
	allowLoopback bool
}

func newOfficialUsageFetcher(timeout time.Duration, now func() time.Time) UsageFetcher {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if now == nil {
		now = time.Now
	}
	return &officialUsageFetcher{
		client: &http.Client{
			Timeout:       timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		endpoint: officialUsageURL,
		now:      now,
	}
}

type usageFetchError struct {
	code  string
	cause error
}

func (e usageFetchError) Error() string { return "Codex usage is unavailable" }
func (e usageFetchError) Unwrap() error {
	if e.cause != nil {
		return e.cause
	}
	return ErrUsageUnavailable
}

func usageError(code string, cause error) error {
	return usageFetchError{code: code, cause: cause}
}

func usageErrorCode(err error) string {
	var typed usageFetchError
	if errors.As(err, &typed) && safeIdentifier(typed.code, 48) != "" {
		return typed.code
	}
	if errors.Is(err, ErrNeedsLogin) {
		return "needs_login"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	return "unavailable"
}

func (f *officialUsageFetcher) FetchUsage(ctx context.Context, record *coreauth.Auth) (UsageFetchResult, error) {
	if record == nil || record.Provider != "codex" || record.Metadata == nil {
		return UsageFetchResult{}, usageError("invalid_credential", ErrUsageUnavailable)
	}
	accessToken, _ := record.Metadata["access_token"].(string)
	accountID, _ := record.Metadata["account_id"].(string)
	accessToken, accountID = strings.TrimSpace(accessToken), strings.TrimSpace(accountID)
	if accessToken == "" || len(accessToken) > maxTokenBytes || strings.ContainsAny(accessToken, "\r\n\x00") || len(accountID) > 512 || strings.ContainsAny(accountID, "\r\n\x00") {
		return UsageFetchResult{}, usageError("invalid_credential", ErrUsageUnavailable)
	}
	endpoint := strings.TrimSpace(f.endpoint)
	if endpoint == "" {
		endpoint = officialUsageURL
	}
	if !validUsageEndpoint(endpoint, f.allowLoopback) {
		return UsageFetchResult{}, usageError("invalid_endpoint", ErrUsageUnavailable)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return UsageFetchResult{}, usageError("request_failed", ErrUsageUnavailable)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", "salcara-bridge/codex-usage")
	req.Header.Set("originator", "salcara")
	if accountID != "" {
		req.Header.Set("ChatGPT-Account-Id", accountID)
	}
	client := f.client
	if client == nil {
		client = newOfficialUsageFetcher(5*time.Second, f.now).(*officialUsageFetcher).client
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return UsageFetchResult{}, usageError(usageErrorCode(ctx.Err()), ctx.Err())
		}
		return UsageFetchResult{}, usageError("network", ErrUsageUnavailable)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		code, accessErr := explicitUsageAccessFailure(resp)
		if ctx.Err() != nil {
			return UsageFetchResult{}, usageError(usageErrorCode(ctx.Err()), ctx.Err())
		}
		if code != "" {
			return UsageFetchResult{}, usageError(code, ErrUsageUnavailable)
		}
		if resp.StatusCode == http.StatusUnauthorized {
			// A proxy's HTML page, truncated response, or body read timeout
			// is not a reliable provider-authentication observation. Keep
			// bounded credential recovery for ordinary JSON 401 responses.
			if accessErr != nil {
				return UsageFetchResult{}, accessErr
			}
			return UsageFetchResult{}, usageError("needs_login", ErrNeedsLogin)
		}
		return UsageFetchResult{}, usageError("forbidden", ErrUsageUnavailable)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return UsageFetchResult{}, usageError("rate_limited", ErrUsageUnavailable)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return UsageFetchResult{}, usageError("upstream_status", ErrUsageUnavailable)
	}
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if contentType != "" && !strings.HasPrefix(contentType, "application/json") {
		return UsageFetchResult{}, usageError("invalid_content_type", ErrUsageUnavailable)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUsageBodyBytes+1))
	if err != nil || len(body) == 0 || len(body) > maxUsageBodyBytes {
		return UsageFetchResult{}, usageError("invalid_response", ErrUsageUnavailable)
	}
	now := time.Now()
	if f.now != nil {
		now = f.now()
	}
	result, err := parseOfficialUsage(body, now)
	if err != nil {
		return UsageFetchResult{}, usageError("invalid_response", ErrUsageUnavailable)
	}
	if accountID != "" && result.ProviderAccountID != "" && result.ProviderAccountID != accountID {
		return UsageFetchResult{}, usageError("identity_mismatch", ErrConflict)
	}
	return result, nil
}

// Generic 403, free-form messages, and undocumented error shapes are not proof
// of workspace removal. Only exact structured access-state codes are promoted;
// all other errors keep their recoverable HTTP classification. Never retain or
// return the upstream body (which can contain private account/workspace data).
func explicitUsageAccessFailure(resp *http.Response) (string, error) {
	contentTypes := resp.Header.Values("Content-Type")
	if len(contentTypes) != 1 {
		return "", usageError("invalid_content_type", ErrUsageUnavailable)
	}
	contentType, _, err := mime.ParseMediaType(contentTypes[0])
	if err != nil || contentType != "application/json" {
		return "", usageError("invalid_content_type", ErrUsageUnavailable)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8193))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return "", usageError(usageErrorCode(err), err)
		}
		var networkErr net.Error
		if errors.As(err, &networkErr) && networkErr.Timeout() {
			return "", usageError("timeout", ErrUsageUnavailable)
		}
		return "", usageError("invalid_response", ErrUsageUnavailable)
	}
	if len(body) == 0 || len(body) > 8192 {
		return "", usageError("invalid_response", ErrUsageUnavailable)
	}
	value, err := strictJSONLimit(body, 8192)
	if err != nil {
		return "", usageError("invalid_response", ErrUsageUnavailable)
	}
	root, ok := value.(map[string]any)
	if !ok {
		return "", usageError("invalid_response", ErrUsageUnavailable)
	}
	readCode := func(fields map[string]any) (string, bool) {
		value, exists := fields["code"]
		if !exists {
			return "", true
		}
		code, ok := value.(string)
		return code, ok && code != "" && safeIdentifier(code, 64) == code
	}
	code, valid := readCode(root)
	if !valid {
		return "", nil
	}
	if raw, exists := root["error"]; exists {
		nested, ok := raw.(map[string]any)
		if !ok {
			return "", nil
		}
		other, valid := readCode(nested)
		if !valid {
			return "", nil
		}
		if code != "" && other != "" && code != other {
			return "", nil
		}
		if other != "" {
			code = other
		}
	}
	switch code {
	case "workspace_access_revoked", "workspace_membership_revoked", "workspace_revoked":
		return "workspace_revoked", nil
	case "account_deactivated", "account_disabled", "account_deleted", "account_suspended":
		return "account_unavailable", nil
	case "permission_denied", "insufficient_permissions", "workspace_access_denied", "organization_access_denied":
		return "permission_denied", nil
	default:
		return "", nil
	}
}

func validUsageEndpoint(raw string, allowLoopback bool) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	if u.Scheme == "https" && strings.EqualFold(u.Hostname(), "chatgpt.com") && u.Port() == "" && u.EscapedPath() == "/backend-api/wham/usage" {
		return true
	}
	if !allowLoopback || u.Scheme != "http" || u.Port() == "" {
		return false
	}
	host := net.ParseIP(u.Hostname())
	return host != nil && host.IsLoopback()
}

func parseOfficialUsage(body []byte, observedAt time.Time) (UsageFetchResult, error) {
	value, err := strictJSONLimit(body, maxUsageBodyBytes)
	if err != nil {
		return UsageFetchResult{}, err
	}
	root, ok := value.(map[string]any)
	if !ok {
		return UsageFetchResult{}, ErrInvalidImport
	}
	planRaw, _ := optionalString(root, "plan_type", 64)
	planRaw, planFamily := normalizePlan(planRaw)
	result := UsageFetchResult{ProviderAccountID: boundedPrivateString(root["account_id"], 512)}
	result.Snapshot = UsageSnapshot{
		Status:        UsageStatusFresh,
		Verified:      true,
		Source:        UsageSourceOfficial,
		ObservedAt:    timePtr(observedAt.UTC()),
		LastAttemptAt: timePtr(observedAt.UTC()),
		Plan:          UsagePlan{Raw: planRaw, Family: planFamily, Source: UsageSourceOfficial, Verified: true},
	}
	seen := planRaw != "" || result.ProviderAccountID != ""
	if raw, exists := root["rate_limit"]; exists && raw != nil {
		limit, parseErr := parseUsageLimit(raw, "codex", observedAt)
		if parseErr != nil {
			return UsageFetchResult{}, parseErr
		}
		result.Snapshot.RateLimit = limit
		seen = true
	}
	if raw, exists := root["code_review_rate_limit"]; exists && raw != nil {
		limit, parseErr := parseUsageLimit(raw, "code_review", observedAt)
		if parseErr != nil {
			return UsageFetchResult{}, parseErr
		}
		result.Snapshot.CodeReviewRateLimit = limit
		seen = true
	}
	if raw, exists := root["additional_rate_limits"]; exists && raw != nil {
		items, ok := raw.([]any)
		if !ok {
			return UsageFetchResult{}, ErrInvalidImport
		}
		if len(items) > maxAdditionalLimits {
			result.Snapshot.AdditionalLimitsTruncated = true
			items = items[:maxAdditionalLimits]
		}
		for i, item := range items {
			m, ok := item.(map[string]any)
			if !ok {
				return UsageFetchResult{}, ErrInvalidImport
			}
			name := safeDisplayString(m["limit_name"], 128)
			feature := safeIdentifierValue(m["metered_feature"], 128)
			model := safeIdentifierValue(m["normal_model_slug"], 200)
			limit := &UsageLimit{ID: fmt.Sprintf("additional_%d", i+1)}
			if rawLimit, exists := m["rate_limit"]; exists && rawLimit != nil {
				var parseErr error
				limit, parseErr = parseUsageLimit(rawLimit, limit.ID, observedAt)
				if parseErr != nil {
					return UsageFetchResult{}, parseErr
				}
			}
			limit.Name, limit.MeteredFeature, limit.NormalModelSlug = name, feature, model
			result.Snapshot.AdditionalRateLimits = append(result.Snapshot.AdditionalRateLimits, *limit)
		}
		seen = true
	}
	if raw, exists := root["credits"]; exists && raw != nil {
		credits, parseErr := parseUsageCredits(raw)
		if parseErr != nil {
			return UsageFetchResult{}, parseErr
		}
		result.Snapshot.Credits = credits
		seen = true
	}
	if raw, exists := root["spend_control"]; exists && raw != nil {
		spend, parseErr := parseUsageSpendControl(raw, observedAt)
		if parseErr != nil {
			return UsageFetchResult{}, parseErr
		}
		result.Snapshot.SpendControl = spend
		seen = true
	}
	if raw, exists := root["rate_limit_reset_credits"]; exists && raw != nil {
		m, ok := raw.(map[string]any)
		if !ok {
			return UsageFetchResult{}, ErrInvalidImport
		}
		count, parseErr := optionalInt64(m, "available_count")
		if parseErr != nil || count != nil && *count < 0 {
			return UsageFetchResult{}, ErrInvalidImport
		}
		result.Snapshot.ResetCreditsAvailable = count
		seen = true
	}
	result.Snapshot.RateLimitReachedType = parseReachedType(root["rate_limit_reached_type"])
	if !seen {
		return UsageFetchResult{}, ErrInvalidImport
	}
	return result, nil
}

func parseUsageLimit(raw any, id string, observedAt time.Time) (*UsageLimit, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, ErrInvalidImport
	}
	allowed, err := optionalBool(m, "allowed")
	if err != nil {
		return nil, err
	}
	reached, err := optionalBool(m, "limit_reached")
	if err != nil {
		return nil, err
	}
	primary, err := parseUsageWindow(m["primary_window"], observedAt)
	if err != nil {
		return nil, err
	}
	secondary, err := parseUsageWindow(m["secondary_window"], observedAt)
	if err != nil {
		return nil, err
	}
	return &UsageLimit{ID: id, Allowed: allowed, LimitReached: reached, Primary: primary, Secondary: secondary}, nil
}

func parseUsageWindow(raw any, observedAt time.Time) (*UsageWindow, error) {
	if raw == nil {
		return nil, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, ErrInvalidImport
	}
	used, err := optionalPercent(m, "used_percent")
	if err != nil || used == nil {
		return nil, ErrInvalidImport
	}
	windowSeconds, err := optionalInt64(m, "limit_window_seconds")
	if err != nil || windowSeconds != nil && *windowSeconds <= 0 {
		return nil, ErrInvalidImport
	}
	resetAfter, err := optionalInt64(m, "reset_after_seconds")
	if err != nil || resetAfter != nil && *resetAfter < 0 {
		return nil, ErrInvalidImport
	}
	resetAt, err := optionalInt64(m, "reset_at")
	if err != nil || resetAt != nil && *resetAt <= 0 {
		return nil, ErrInvalidImport
	}
	remaining := 100 - *used
	out := &UsageWindow{UsedPercent: used, RemainingPercent: &remaining, WindowSeconds: windowSeconds, ResetAfterSeconds: resetAfter}
	if windowSeconds != nil {
		minutes := (*windowSeconds + 59) / 60
		out.WindowMinutes = &minutes
	}
	if resetAt != nil {
		at := time.Unix(*resetAt, 0).UTC()
		out.ResetsAt = &at
	} else if resetAfter != nil {
		at := observedAt.Add(time.Duration(*resetAfter) * time.Second).UTC()
		out.ResetsAt = &at
	}
	return out, nil
}

func parseUsageCredits(raw any) (*UsageCredits, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, ErrInvalidImport
	}
	has, err := optionalBool(m, "has_credits")
	if err != nil {
		return nil, err
	}
	unlimited, err := optionalBool(m, "unlimited")
	if err != nil {
		return nil, err
	}
	return &UsageCredits{HasCredits: has, Unlimited: unlimited, Balance: safeDecimal(m["balance"])}, nil
}

func parseUsageSpendControl(raw any, observedAt time.Time) (*UsageSpendControl, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, ErrInvalidImport
	}
	reached, err := optionalBool(m, "reached")
	if err != nil {
		return nil, err
	}
	out := &UsageSpendControl{Reached: reached}
	if rawLimit := m["individual_limit"]; rawLimit != nil {
		limit, ok := rawLimit.(map[string]any)
		if !ok {
			return nil, ErrInvalidImport
		}
		usedPercent, parseErr := optionalPercent(limit, "used_percent")
		if parseErr != nil {
			return nil, parseErr
		}
		remainingPercent, parseErr := optionalPercent(limit, "remaining_percent")
		if parseErr != nil {
			return nil, parseErr
		}
		resetAfter, parseErr := optionalInt64(limit, "reset_after_seconds")
		if parseErr != nil || resetAfter != nil && *resetAfter < 0 {
			return nil, ErrInvalidImport
		}
		resetAt, parseErr := optionalInt64(limit, "reset_at")
		if parseErr != nil || resetAt != nil && *resetAt <= 0 {
			return nil, ErrInvalidImport
		}
		individual := &UsageSpendLimit{
			Source: safeIdentifierValue(limit["source"], 64), Limit: safeDecimal(limit["limit"]), Used: safeDecimal(limit["used"]), Remaining: safeDecimal(limit["remaining"]),
			UsedPercent: usedPercent, RemainingPercent: remainingPercent, ResetAfterSeconds: resetAfter,
		}
		if resetAt != nil {
			at := time.Unix(*resetAt, 0).UTC()
			individual.ResetsAt = &at
		} else if resetAfter != nil {
			at := observedAt.Add(time.Duration(*resetAfter) * time.Second).UTC()
			individual.ResetsAt = &at
		}
		out.IndividualLimit = individual
	}
	return out, nil
}

func optionalBool(m map[string]any, key string) (*bool, error) {
	raw, exists := m[key]
	if !exists || raw == nil {
		return nil, nil
	}
	value, ok := raw.(bool)
	if !ok {
		return nil, ErrInvalidImport
	}
	return &value, nil
}

func optionalInt64(m map[string]any, key string) (*int64, error) {
	raw, exists := m[key]
	if !exists || raw == nil {
		return nil, nil
	}
	number, ok := raw.(json.Number)
	if !ok {
		return nil, ErrInvalidImport
	}
	value, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil {
		return nil, ErrInvalidImport
	}
	return &value, nil
}

func optionalPercent(m map[string]any, key string) (*float64, error) {
	raw, exists := m[key]
	if !exists || raw == nil {
		return nil, nil
	}
	number, ok := raw.(json.Number)
	if !ok {
		return nil, ErrInvalidImport
	}
	value, err := strconv.ParseFloat(number.String(), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 100 {
		return nil, ErrInvalidImport
	}
	return &value, nil
}

func optionalString(m map[string]any, key string, max int) (string, error) {
	raw, exists := m[key]
	if !exists || raw == nil {
		return "", nil
	}
	value, ok := raw.(string)
	if !ok || len(value) > max || strings.ContainsAny(value, "\r\n\x00") {
		return "", ErrInvalidImport
	}
	return strings.TrimSpace(value), nil
}

func normalizePlan(raw string) (string, string) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if safeIdentifier(raw, 64) == "" {
		return "", "unknown"
	}
	switch raw {
	case "free":
		return raw, "free"
	case "go":
		return raw, "go"
	case "plus":
		return raw, "plus"
	case "pro", "prolite", "promax":
		return raw, "pro"
	case "team":
		return raw, "team"
	case "self_serve_business_prolite", "self_serve_business_usage_based", "business":
		return raw, "business"
	case "ent26", "enterprise_cbp_automation", "enterprise_cbp_usage_based", "enterprise", "hc":
		return raw, "enterprise"
	case "education", "edu", "edu_plus", "edu_pro":
		return raw, "edu"
	default:
		return raw, "unknown"
	}
}

func safeIdentifier(value string, max int) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > max {
		return ""
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' {
			continue
		}
		return ""
	}
	return value
}

func safeIdentifierValue(raw any, max int) string {
	value, _ := raw.(string)
	return safeIdentifier(value, max)
}

func safeDisplayString(raw any, max int) string {
	value, _ := raw.(string)
	value = strings.TrimSpace(value)
	if value == "" || len(value) > max || strings.ContainsAny(value, "\r\n\x00") {
		return ""
	}
	return value
}

func boundedPrivateString(raw any, max int) string {
	value, _ := raw.(string)
	value = strings.TrimSpace(value)
	if value == "" || len(value) > max || strings.ContainsAny(value, "\r\n\x00") {
		return ""
	}
	return value
}

func safeDecimal(raw any) string {
	var value string
	switch typed := raw.(type) {
	case string:
		value = strings.TrimSpace(typed)
	case json.Number:
		value = typed.String()
	default:
		return ""
	}
	if value == "" || len(value) > 64 {
		return ""
	}
	dots := 0
	for i, r := range value {
		if r >= '0' && r <= '9' {
			continue
		}
		if r == '.' {
			dots++
			if dots > 1 {
				return ""
			}
			continue
		}
		if i == 0 && (r == '-' || r == '+') {
			continue
		}
		return ""
	}
	return value
}

func parseReachedType(raw any) string {
	if m, ok := raw.(map[string]any); ok {
		raw = m["kind"]
	}
	value, _ := raw.(string)
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "rate_limit_reached", "workspace_owner_credits_depleted", "workspace_member_credits_depleted", "workspace_owner_usage_limit_reached", "workspace_member_usage_limit_reached":
		return value
	default:
		return ""
	}
}

func timePtr(value time.Time) *time.Time { return &value }

func unverifiedUsage(planHint string) UsageSnapshot {
	raw, family := normalizePlan(planHint)
	source := "not_queried"
	if raw != "" {
		source = UsageSourceJWTHint
	}
	return UsageSnapshot{Status: UsageStatusUnknown, Source: source, Plan: UsagePlan{Raw: raw, Family: family, Source: source, Verified: false}}
}
