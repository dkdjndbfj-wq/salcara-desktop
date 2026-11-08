package desktopcompanion

import (
	"encoding/json"
	"testing"
)

func TestNativeHistoryRetryAtUsesOnlyReportedLimitTime(t *testing.T) {
	for _, tc := range []struct {
		code, details   string
		completed, want int64
	}{
		{`"usageLimitExceeded"`, `{"resets_at":1794016860}`, 0, 1794016860000},
		{`{"httpConnectionFailed":{"httpStatusCode":429}}`, `{"retry_after":60}`, 1794016800, 1794016860000},
		{`"usageLimitExceeded"`, "Retry-After: 60", 1794016800000, 1794016860000},
		{`"usageLimitExceeded"`, `{"retry_after":60}`, 0, 0},
		{`"serverOverloaded"`, `{"resets_at":1794016860}`, 1794016800, 0},
	} {
		raw, _ := json.Marshal(map[string]any{"message": "failed", "codexErrorInfo": json.RawMessage(tc.code), "additionalDetails": tc.details})
		events := nativeEvents("codex:fixture", nativeRead{Turns: []nativeTurn{{ID: "fixture-turn", Status: "failed", Error: raw, StartedAt: 1794016700, CompletedAt: tc.completed}}})
		if len(events) != 1 || events[0].RetryAt != tc.want {
			t.Fatalf("native reset metadata changed: %+v", events)
		}
	}
}
