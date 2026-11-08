package console

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"salcara/bridge/internal/relay"
)

type accountUsageGroup struct {
	Name       string `json:"name"`
	Source     string `json:"source"`
	FreshUntil int64  `json:"freshUntil"`
}

func accountUsageGroupName(raw any) (string, bool) {
	name, ok := raw.(string)
	if !ok || !utf8.ValidString(name) {
		return "", false
	}
	// Reject controls rather than deleting characters and changing the name.
	// Format controls include invisible direction overrides; card content must
	// remain a single plain-text label, not an additional UI control surface.
	for _, r := range name {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
			return "", false
		}
	}
	name = strings.TrimSpace(name)
	return name, name != "" && utf8.RuneCountInString(name) <= 120
}

func normalizeAccountUsageGroup(raw map[string]any, billing *relay.KeyBilling, now time.Time, freshUntil int64) *accountUsageGroup {
	if freshUntil <= now.UnixMilli() || !accountUsageScoped(raw) {
		return nil
	}
	if valid, present := raw["isValid"]; present && valid != true {
		return nil
	}
	if raw["error"] != nil {
		return nil
	}
	name := ""
	add := func(value any) bool {
		next, ok := accountUsageGroupName(value)
		if !ok || name != "" && name != next {
			return false
		}
		name = next
		return true
	}
	// Only current, explicit root-level group fields belong to this API key.
	// Never scan model_stats/history for the most recently used group's name.
	if value, exists := raw["group_name"]; exists && !add(value) {
		return nil
	}
	if value, exists := raw["group"]; exists {
		group, ok := value.(map[string]any)
		if !ok || !accountUsageScoped(group) {
			return nil
		}
		if value, exists := group["name"]; exists && !add(value) {
			return nil
		}
	}
	if accountUsageSubscriptionGroup(raw, billing, now) && !add(raw["planName"]) {
		return nil
	}
	if name == "" {
		return nil
	}
	return &accountUsageGroup{Name: name, Source: "provider_key_usage", FreshUntil: freshUntil}
}

// Sub2API usageUnrestricted uses Group.Name as planName only in its subscription
// branch. The wallet branch's planName is NOT a group. Require its authenticated
// typed billing protocol and the verified unrestricted response fingerprint.
// The subscription object may be absent when no active subscription was loaded;
// if supplied, contradictory/malformed subscription details remain unknown.
// Source: Wei-Shaw/sub2api backend/internal/handler/gateway_handler.go.
func accountUsageSubscriptionGroup(raw map[string]any, billing *relay.KeyBilling, now time.Time) bool {
	if billing == nil || billing.Source != "sub2api.key_billing" || billing.Scope != "token" || billing.FreshUntil <= now.UnixMilli() ||
		raw["mode"] != "unrestricted" || raw["isValid"] != true || raw["unit"] != "USD" {
		return false
	}
	if _, present := raw["balance"]; present {
		return false
	}
	if _, present := raw["quota"]; present {
		return false
	}
	name, ok := accountUsageGroupName(raw["planName"])
	if !ok || name == "钱包余额" {
		return false
	}
	value, exists := raw["subscription"]
	if !exists {
		return true
	}
	subscription, ok := value.(map[string]any)
	if !ok || !accountUsageScoped(subscription) {
		return false
	}
	for _, window := range []string{"daily", "weekly", "monthly"} {
		if value, present := subscription[window+"_usage_usd"]; present {
			used := accountUsageNumber(value)
			if used == nil || *used < 0 {
				return false
			}
		}
		if limit, present := subscription[window+"_limit_usd"]; present && limit != nil {
			value := accountUsageNumber(limit)
			if value == nil || *value < 0 {
				return false
			}
		}
	}
	return true
}
