package codexaccounts

import (
	"encoding/base64"
	"testing"
	"time"
)

func subscriptionJWT(payload string) string {
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".synthetic-signature"
}

func TestSubscriptionHintFromTokensPrefersIDToken(t *testing.T) {
	idExpiry := "2031-02-03T04:05:06Z"
	accessExpiry := "2032-03-04T05:06:07Z"
	c := credential{
		IDToken:     subscriptionJWT(`{"https://api.openai.com/auth":{"chatgpt_subscription_active_until":"` + idExpiry + `"}}`),
		AccessToken: subscriptionJWT(`{"https://api.openai.com/auth":{"chatgpt_subscription_active_until":"` + accessExpiry + `"}}`),
	}
	if !applySubscriptionHintFromTokens(&c) {
		t.Fatal("expected a subscription hint")
	}
	if got := c.SubscriptionActiveUntil.Format(time.RFC3339); got != idExpiry {
		t.Fatalf("ID token did not win: %s", got)
	}
	if c.SubscriptionSource != SubscriptionSourceJWTHint {
		t.Fatalf("source = %q", c.SubscriptionSource)
	}

	if !applySubscriptionHint(&c, "2033-04-05T06:07:08Z", SubscriptionSourceImportHint) {
		t.Fatal("explicit import hint rejected")
	}
	if applySubscriptionHintFromTokens(&c) {
		t.Fatal("JWT hint replaced an explicit import hint")
	}
	if c.SubscriptionSource != SubscriptionSourceImportHint {
		t.Fatalf("source = %q", c.SubscriptionSource)
	}
}

func TestSubscriptionHintParsingAndProjection(t *testing.T) {
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	want := now.Add(36 * time.Hour)
	for _, raw := range []string{
		want.Format(time.RFC3339),
		"1893683045",
		"1893683045000",
	} {
		got, ok := parseSubscriptionExpiry(raw)
		if !ok || !got.Equal(want) {
			t.Fatalf("parse %q = %s, %v; want %s", raw, got, ok, want)
		}
	}
	for _, raw := range []string{"", "-1", "0", "not-a-date", "253402300800", "2030-01-01T00:00:00Z\nsecret"} {
		if _, ok := parseSubscriptionExpiry(raw); ok {
			t.Fatalf("accepted invalid expiry %q", raw)
		}
	}

	c := credential{SubscriptionActiveUntil: want, SubscriptionObservedAt: now.Add(-time.Minute), SubscriptionSource: SubscriptionSourceImportHint}
	metadata := c.account(now).Subscription
	if metadata.Status != SubscriptionStatusActive || metadata.Verified || metadata.Source != SubscriptionSourceImportHint {
		t.Fatalf("unexpected active metadata: %+v", metadata)
	}
	if metadata.ActiveUntil == nil || !metadata.ActiveUntil.Equal(want) || metadata.ObservedAt == nil || metadata.RemainingSeconds == nil || *metadata.RemainingSeconds != 36*60*60 {
		t.Fatalf("incomplete active metadata: %+v", metadata)
	}

	metadata = c.account(want.Add(time.Second)).Subscription
	if metadata.Status != SubscriptionStatusExpired || metadata.RemainingSeconds == nil || *metadata.RemainingSeconds != 0 {
		t.Fatalf("unexpected expired metadata: %+v", metadata)
	}
}

func TestSubscriptionHintNeverUsesCredentialExpiry(t *testing.T) {
	expOnly := credential{AccessToken: subscriptionJWT(`{"exp":1893711845}`)}
	if applySubscriptionHintFromTokens(&expOnly) {
		t.Fatal("credential exp was accepted as subscription expiry")
	}
	metadata := expOnly.account(time.Now()).Subscription
	if metadata.Status != SubscriptionStatusUnknown || metadata.Source != SubscriptionSourceUnknown || metadata.ActiveUntil != nil || metadata.RemainingSeconds != nil {
		t.Fatalf("unexpected metadata from credential exp: %+v", metadata)
	}

	accessFallback := credential{IDToken: "malformed", AccessToken: subscriptionJWT(`{"https://api.openai.com/auth":{"chatgpt_subscription_active_until":1893711845}}`)}
	if !applySubscriptionHintFromTokens(&accessFallback) {
		t.Fatal("access-token named subscription claim was not used as fallback")
	}
}

func TestRefreshOnlyCredentialHidesInternalExpirySentinel(t *testing.T) {
	now := time.Now().UTC()
	refreshOnly := credential{RefreshToken: "synthetic-refresh-only", ExpiresAt: time.Unix(1, 0), Status: StatusExpired}
	account := refreshOnly.account(now)
	if account.ExpiresAt != nil || account.Status != StatusExpired {
		t.Fatalf("internal refresh sentinel leaked to account DTO: %+v", account)
	}
	withAccess := refreshOnly
	withAccess.AccessToken = "synthetic-access"
	if withAccess.account(now).ExpiresAt == nil {
		t.Fatal("real access-token expiry was hidden")
	}
}
