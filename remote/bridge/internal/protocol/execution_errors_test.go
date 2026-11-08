package protocol

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestCodexTypedExecutionConditionsPrecedeText(t *testing.T) {
	for _, tc := range []struct{ info, want string }{
		{`"contextWindowExceeded"`, "context_limit"},
		{`"sessionBudgetExceeded"`, "session_budget"},
		{`"usageLimitExceeded"`, "usage_limit"},
		{`"serverOverloaded"`, "model_capacity"},
		{`"unauthorized"`, "upstream_unauthorized"},
		{`"badRequest"`, "upstream_bad_request"},
		{`"internalServerError"`, "upstream_internal_error"},
		{`"cyberPolicy"`, "policy_rejected"},
		{`"sandboxError"`, "sandbox_error"},
		{`"threadRollbackFailed"`, "thread_rollback_failed"},
		{`{"httpConnectionFailed":{"httpStatusCode":null}}`, "upstream_connection_failed"},
		{`{"responseStreamConnectionFailed":{"httpStatusCode":502}}`, "upstream_connection_failed"},
		{`{"responseStreamDisconnected":{"httpStatusCode":null}}`, "response_stream_interrupted"},
		{`{"responseTooManyFailedAttempts":{"httpStatusCode":null}}`, "response_retry_exhausted"},
		{`{"activeTurnNotSteerable":{"turnKind":"review"}}`, "turn_not_steerable"},
		{`{"httpConnectionFailed":{"httpStatusCode":400}}`, "upstream_bad_request"},
		{`{"httpConnectionFailed":{"httpStatusCode":401}}`, "upstream_unauthorized"},
		{`{"httpConnectionFailed":{"httpStatusCode":403}}`, "upstream_forbidden"},
		{`{"httpConnectionFailed":{"httpStatusCode":429}}`, "upstream_rate_limit"},
		{`{"responseStreamDisconnected":{"httpStatusCode":429}}`, "upstream_rate_limit"},
	} {
		t.Run(tc.want+tc.info, func(t *testing.T) {
			if got := CodexExecutionErrorCode(json.RawMessage(tc.info), "Selected model is at capacity. Invalid API Key."); got != tc.want {
				t.Fatalf("typed condition became a text guess: got %q want %q", got, tc.want)
			}
		})
	}
}

func TestCodexUnknownConditionIsNotCredentialOrRateGuess(t *testing.T) {
	for _, raw := range []string{`"other"`, `"futureCondition"`, `{"futureFailure":{"httpStatusCode":401,"private":"must not project"}}`, `{"httpConnectionFailed":{},"other":{}}`, `{"httpConnectionFailed":{}}`, `{"httpConnectionFailed":null}`, `{"httpConnectionFailed":{"HTTPStatusCode":401}}`, `{"responseStreamDisconnected":{"httpStatusCode":"401"}}`, `{"activeTurnNotSteerable":{}}`, `[]`, `{broken`} {
		if code := CodexExecutionErrorCode(json.RawMessage(raw), "Selected model is at capacity; HTTP 429; invalid Key; retry in 15 seconds"); code != "unknown_execution_error" {
			t.Fatalf("unknown payload guessed execution condition: %q (%s)", code, raw)
		}
	}
	for _, raw := range []string{"", `null`, " \n null \t"} {
		if got := CodexExecutionErrorCode(json.RawMessage(raw), "Selected model is at capacity. Please try a different model."); got != "model_capacity" {
			t.Fatalf("legacy exact condition lost: %q", got)
		}
		if got := CodexExecutionErrorCode(json.RawMessage(raw), "HTTP 401 invalid Key"); got != "" {
			t.Fatalf("legacy credentials were guessed by this mapper: %q", got)
		}
	}
}

type fixtureExecutionError struct{ code string }

func (e fixtureExecutionError) Error() string              { return "Selected model is at capacity" }
func (e fixtureExecutionError) ExecutionErrorCode() string { return e.code }

func TestWrappedExecutionErrorRetainsStructuredCondition(t *testing.T) {
	if got := ExecutionErrorCode(fmt.Errorf("transport: %w", fixtureExecutionError{code: "context_limit"})); got != "context_limit" {
		t.Fatalf("wrapped context failure became capacity: %q", got)
	}
}
