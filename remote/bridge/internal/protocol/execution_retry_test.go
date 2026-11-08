package protocol

import (
	"strings"
	"testing"
)

func TestExecutionRetryAtRequiresExplicitLimitMetadata(t *testing.T) {
	const observed = int64(1794016800000)
	for _, tc := range []struct {
		name, code, detail string
		base, want         int64
	}{
		{"usage reset seconds", "usage_limit", `{"error":{"resets_at":1794016860}}`, observed, 1794016860000},
		{"embedded response body", "upstream_rate_limit", `HTTP 429 response body: {"error":{"retry_after":60}}`, observed, observed + 60000},
		{"JSON header", "upstream_rate_limit", `{"headers":{"retry-after":"90"}}`, observed, observed + 90000},
		{"HTTP header", "upstream_rate_limit", "HTTP/1.1 429\r\nRetry-After: 60\r\n", observed, observed + 60000},
		{"absolute HTTP date", "upstream_rate_limit", "Retry-After: Sat, 07 Nov 2026 02:00:00 GMT", 0, observed},
		{"absolute ISO time", "usage_limit", `{"resets_at":"2026-11-07T02:00:00Z"}`, 0, observed},
		{"missing history anchor", "usage_limit", `{"retry_after":60}`, 0, 0},
		{"not a quota failure", "model_capacity", `{"resets_at":1794016860}`, observed, 0},
		{"untyped prose", "", `{"resets_at":1794016860}`, observed, 0},
		{"no metadata", "usage_limit", `Try again in one hour`, observed, 0},
		{"mentioned header", "usage_limit", `The Retry-After: 60 is an example`, observed, 0},
		{"SDK backoff", "usage_limit", `{"retry_delay_ms":60000}`, observed, 0},
		{"unrelated nested field", "usage_limit", `{"user_input":{"resets_at":1794016860}}`, observed, 0},
		{"negative", "usage_limit", `{"retry_after":-1}`, observed, 0},
		{"fractional", "usage_limit", `{"retry_after":1.5}`, observed, 0},
		{"overflow", "usage_limit", `{"retry_after":9223372036854775807}`, observed, 0},
		{"no unit guessing", "usage_limit", `{"resets_at":1794016860000}`, observed, 0},
		{"oversized", "usage_limit", strings.Repeat(" ", 65536) + `{"retry_after":60}`, observed, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExecutionRetryAt(tc.code, tc.base, tc.detail); got != tc.want {
				t.Fatalf("retryAt = %d, want %d", got, tc.want)
			}
		})
	}
}
