package codexaccounts

import (
	"strconv"
	"time"
)

// A portable OAuth record can repeat one expiry as Unix seconds and RFC3339.
// Limit semantic equivalence to this display/lifetime hint; token and identity
// fields continue to require the existing scalar/type agreement.
func equivalentImportExpiry(a, b any) (time.Time, bool) {
	left, leftOK := exactImportExpiry(a)
	right, rightOK := exactImportExpiry(b)
	return left, leftOK && rightOK && left.Equal(right)
}

func exactImportExpiry(value any) (time.Time, bool) {
	raw := subscriptionScalar(value)
	parsed, ok := parseSubscriptionExpiry(raw)
	if !ok {
		return time.Time{}, false
	}
	if onlyDigits(raw) {
		// The shared metadata parser checks formats and bounds, but intentionally
		// rounds millisecond epochs to seconds. Equality must retain that precision.
		if epoch, err := strconv.ParseInt(raw, 10, 64); err == nil && epoch > 1_000_000_000_000 {
			parsed = time.Unix(epoch/1000, (epoch%1000)*int64(time.Millisecond)).UTC()
		}
	}
	return parsed, true
}
