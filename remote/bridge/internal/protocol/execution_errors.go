package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
)

// CapacityTextCode supports older runtimes that expose no typed provider
// condition. HTTP 429 alone is deliberately insufficient: rate limiting and a
// full selected model require different actions on the phone.
var modelCapacityText = regexp.MustCompile(`(?i)selected\s+model\s+is\s+at\s+capacity|model\s+(?:is\s+at\s+)?capacity|模型(?:当前)?(?:容量|已满|繁忙)|容量已满`)

func CapacityTextCode(text string) string {
	if modelCapacityText.MatchString(text) {
		return "model_capacity"
	}
	return ""
}

// CodexExecutionErrorCode normalizes only documented app-server error variants.
// Typed conditions take precedence over human-readable messages: a context or
// stream failure must not be mislabeled as an invalid Key by matching text.
// Unknown variant properties are never forwarded to the phone or a cache.
func CodexExecutionErrorCode(info json.RawMessage, message string) string {
	info = bytes.TrimSpace(info)
	if len(info) == 0 || bytes.Equal(info, []byte("null")) {
		// Older runtimes expose no typed condition. Only their known capacity
		// phrase has a compatibility fallback; other prose remains unclassified.
		return CapacityTextCode(message)
	}
	var value string
	if json.Unmarshal(info, &value) == nil {
		switch value {
		case "contextWindowExceeded":
			return "context_limit"
		case "sessionBudgetExceeded":
			return "session_budget"
		case "usageLimitExceeded":
			return "usage_limit"
		case "serverOverloaded":
			return "model_capacity"
		case "unauthorized":
			return "upstream_unauthorized"
		case "badRequest":
			return "upstream_bad_request"
		case "internalServerError":
			return "upstream_internal_error"
		case "cyberPolicy":
			return "policy_rejected"
		case "sandboxError":
			return "sandbox_error"
		case "threadRollbackFailed":
			return "thread_rollback_failed"
		}
	}
	var variants map[string]json.RawMessage
	if json.Unmarshal(info, &variants) == nil && len(variants) == 1 {
		for variant, raw := range variants {
			var payload map[string]json.RawMessage
			if json.Unmarshal(raw, &payload) != nil || payload == nil {
				return "unknown_execution_error"
			}
			code := ""
			switch variant {
			case "httpConnectionFailed", "responseStreamConnectionFailed":
				code = "upstream_connection_failed"
			case "responseStreamDisconnected":
				code = "response_stream_interrupted"
			case "responseTooManyFailedAttempts":
				code = "response_retry_exhausted"
			case "activeTurnNotSteerable":
				var kind string
				if json.Unmarshal(payload["turnKind"], &kind) == nil && (kind == "review" || kind == "compact") {
					return "turn_not_steerable"
				}
				return "unknown_execution_error"
			}
			if code == "" {
				break
			}
			statusRaw, exists := payload["httpStatusCode"]
			var status *int
			if !exists || json.Unmarshal(statusRaw, &status) != nil {
				return "unknown_execution_error"
			}
			if status != nil {
				switch *status {
				case 400:
					return "upstream_bad_request"
				case 401:
					return "upstream_unauthorized"
				case 403:
					return "upstream_forbidden"
				case 429:
					return "upstream_rate_limit"
				}
			}
			return code
		}
	}
	// An explicitly present but unknown/malformed typed condition is not an
	// older-runtime absence. Keep it neutral so the phone cannot turn its prose
	// into a capacity, credential, or quota diagnosis.
	return "unknown_execution_error"
}

// ExecutionErrorCode survives wrapped native/RPC failures without conflating
// execution errors with Hub authentication or delivery uncertainty codes.
func ExecutionErrorCode(err error) string {
	if err == nil {
		return ""
	}
	var coded interface{ ExecutionErrorCode() string }
	if errors.As(err, &coded) {
		if code := coded.ExecutionErrorCode(); code != "" {
			return code
		}
	}
	return CapacityTextCode(err.Error())
}
