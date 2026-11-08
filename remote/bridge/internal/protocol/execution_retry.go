package protocol

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ExecutionRetryAt projects explicit provider reset metadata, never prose or a
// client's exponential backoff. observedAt is the error's Unix millisecond
// timestamp; zero means an old record has no reliable anchor for relative time.
func ExecutionRetryAt(code string, observedAt int64, details ...string) int64 {
	if code != "usage_limit" && code != "upstream_rate_limit" {
		return 0
	}
	for _, detail := range details {
		if len(detail) == 0 || len(detail) > 64*1024 {
			continue
		}
		if at := retryJSON(detail, observedAt, 0); at != 0 {
			return at
		}
		// HTTP adapters may include a JSON response body after a status line.
		if first, last := strings.IndexByte(detail, '{'), strings.LastIndexByte(detail, '}'); first >= 0 && last > first {
			if at := retryJSON(detail[first:last+1], observedAt, 0); at != 0 {
				return at
			}
		}
		for _, line := range strings.Split(detail, "\n") {
			name, value, ok := strings.Cut(strings.TrimSpace(line), ":")
			if ok && strings.EqualFold(strings.TrimSpace(name), "Retry-After") {
				if at := retryAfter(strings.TrimSpace(value), observedAt); at != 0 {
					return at
				}
			}
		}
	}
	return 0
}

// Stay within a well-defined calendar range, safely representable in JS.
const maxRetryAt = int64(253402300799999) // 9999-12-31T23:59:59.999Z

func retryJSON(detail string, observedAt int64, depth int) int64 {
	if depth > 5 {
		return 0
	}
	var value map[string]json.RawMessage
	if json.Unmarshal([]byte(detail), &value) != nil || value == nil {
		return 0
	}
	if raw, ok := value["resets_at"]; ok {
		text := retryScalar(raw)
		// Codex's resets_at is Unix seconds. Do not infer milliseconds by size.
		if seconds, err := strconv.ParseInt(text, 10, 64); err == nil && seconds > 0 && seconds <= maxRetryAt/1000 {
			return seconds * 1000
		}
		if at, err := time.Parse(time.RFC3339Nano, text); err == nil && at.UnixMilli() > 0 && at.UnixMilli() <= maxRetryAt {
			return at.UnixMilli()
		}
	}
	if raw, ok := value["retry_after"]; ok {
		if at := retryAfter(retryScalar(raw), observedAt); at != 0 {
			return at
		}
	}
	for key, raw := range value {
		if strings.EqualFold(key, "Retry-After") {
			if at := retryAfter(retryScalar(raw), observedAt); at != 0 {
				return at
			}
		}
	}
	for _, key := range []string{"error", "headers", "response", "body"} {
		if raw, ok := value[key]; ok {
			if at := retryJSON(string(raw), observedAt, depth+1); at != 0 {
				return at
			}
			var text string
			if json.Unmarshal(raw, &text) == nil {
				if at := retryJSON(text, observedAt, depth+1); at != 0 {
					return at
				}
			}
		}
	}
	return 0
}

func retryScalar(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	return strings.TrimSpace(string(raw))
}

func retryAfter(value string, observedAt int64) int64 {
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 && observedAt > 0 && observedAt <= maxRetryAt && seconds <= (maxRetryAt-observedAt)/1000 {
		return observedAt + seconds*1000
	}
	if at, err := http.ParseTime(value); err == nil && at.UnixMilli() > 0 && at.UnixMilli() <= maxRetryAt {
		return at.UnixMilli()
	}
	return 0
}
