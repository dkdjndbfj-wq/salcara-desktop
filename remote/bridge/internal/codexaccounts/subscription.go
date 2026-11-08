package codexaccounts

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

const (
	SubscriptionStatusActive  = "active"
	SubscriptionStatusExpired = "expired"
	SubscriptionStatusUnknown = "unknown"

	SubscriptionSourceImportHint = "import_hint"
	SubscriptionSourceJWTHint    = "jwt_hint"
	SubscriptionSourceUnknown    = "not_available"
	SubscriptionSourceOfficial   = "openai_subscription_api"

	SubscriptionFreshnessFresh       = "fresh"
	SubscriptionFreshnessStale       = "stale"
	SubscriptionFreshnessUnverified  = "unverified"
	SubscriptionFreshnessUnavailable = "unavailable"
)

// SubscriptionMetadata contains no provider identity or credentials. Verified
// is true only for a fresh authenticated subscription observation. Imported/JWT
// values and stale observations never establish entitlement or pool eligibility.
type SubscriptionMetadata struct {
	Status           string     `json:"status"`
	Source           string     `json:"source"`
	Verified         bool       `json:"verified"`
	ActiveUntil      *time.Time `json:"activeUntil,omitempty"`
	ObservedAt       *time.Time `json:"observedAt,omitempty"`
	RemainingSeconds *int64     `json:"remainingSeconds,omitempty"`
	Freshness        string     `json:"freshness"`
	FreshUntil       *time.Time `json:"freshUntil,omitempty"`
	LastAttemptAt    *time.Time `json:"lastAttemptAt,omitempty"`
	ErrorCode        string     `json:"errorCode,omitempty"`
	CredentialEpoch  uint64     `json:"credentialEpoch,omitempty"`
	IdentityEpoch    uint64     `json:"identityEpoch,omitempty"`
}

// applySubscriptionHint is the single import boundary for an explicit
// subscription field. The caller supplies import_hint or jwt_hint; malformed,
// unbounded, or unsupported values leave the credential unchanged.
func applySubscriptionHint(c *credential, raw, source string) bool {
	if c == nil || source != SubscriptionSourceImportHint && source != SubscriptionSourceJWTHint {
		return false
	}
	expiry, ok := parseSubscriptionExpiry(raw)
	if !ok {
		return false
	}
	c.SubscriptionActiveUntil = expiry
	c.SubscriptionObservedAt = time.Time{}
	c.SubscriptionSource = source
	return true
}

// applySubscriptionHintFromTokens reads only the named subscription claim.
// ID token wins, then access token is a compatibility fallback. Token exp is
// deliberately ignored because credential expiry is not subscription expiry.
func applySubscriptionHintFromTokens(c *credential) bool {
	if c == nil || !c.SubscriptionActiveUntil.IsZero() {
		return false
	}
	for _, token := range []string{c.IDToken, c.AccessToken} {
		claims := displayClaims(token)
		auth, _ := claims["https://api.openai.com/auth"].(map[string]any)
		raw := subscriptionScalar(auth["chatgpt_subscription_active_until"])
		if raw != "" && applySubscriptionHint(c, raw, SubscriptionSourceJWTHint) {
			return true
		}
	}
	return false
}

func parseSubscriptionExpiry(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 128 || strings.ContainsAny(raw, "\r\n\x00") {
		return time.Time{}, false
	}
	if onlyDigits(raw) {
		seconds, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return time.Time{}, false
		}
		if seconds > 1_000_000_000_000 {
			seconds /= 1000
		}
		if seconds <= 0 || seconds >= 253402300800 {
			return time.Time{}, false
		}
		return time.Unix(seconds, 0).UTC(), true
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil || parsed.Unix() <= 0 || parsed.Unix() >= 253402300800 {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

func onlyDigits(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return value != ""
}

func subscriptionScalar(raw any) string {
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case json.Number:
		return value.String()
	default:
		return ""
	}
}

func stampSubscriptionHint(c *credential, observedAt time.Time) {
	if c != nil && !c.SubscriptionActiveUntil.IsZero() && c.SubscriptionObservedAt.IsZero() {
		c.SubscriptionObservedAt = observedAt.UTC()
	}
}

func subscriptionMetadata(c credential, now time.Time) SubscriptionMetadata {
	if c.SubscriptionActiveUntil.IsZero() {
		return SubscriptionMetadata{Status: SubscriptionStatusUnknown, Source: SubscriptionSourceUnknown, Freshness: SubscriptionFreshnessUnverified}
	}
	expiry := c.SubscriptionActiveUntil.UTC()
	remaining := expiry.Unix() - now.UTC().Unix()
	status := SubscriptionStatusActive
	if !now.Before(expiry) {
		status = SubscriptionStatusExpired
		remaining = 0
	}
	source := c.SubscriptionSource
	if source != SubscriptionSourceImportHint && source != SubscriptionSourceJWTHint {
		source = SubscriptionSourceUnknown
	}
	result := SubscriptionMetadata{Status: status, Source: source, Verified: false, ActiveUntil: &expiry, RemainingSeconds: &remaining, Freshness: SubscriptionFreshnessUnverified}
	if !c.SubscriptionObservedAt.IsZero() {
		observed := c.SubscriptionObservedAt.UTC()
		result.ObservedAt = &observed
	}
	return result
}
