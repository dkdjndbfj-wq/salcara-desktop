// Package apierror classifies upstream HTTP responses from Claude and Codex
// (OpenAI-compatible) into stable ErrorKind values that the selection and
// health-tracking layers can act on without parsing raw error strings.
//
// All public functions are pure (no I/O) and safe for concurrent use.
package apierror

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"salcara/bridge/internal/accounthealth"
)

// ── upstream error envelope ──────────────────────────────────────────────────

// upstreamError is the common error shape for both OpenAI and Anthropic APIs.
type upstreamError struct {
	Error struct {
		Type    string `json:"type"`
		Code    any    `json:"code"` // string or number
		Message string `json:"message"`
		// Anthropic uses "error" at the top level.
		Param string `json:"param"`
	} `json:"error"`
	// Anthropic top-level shape.
	Type    string `json:"type"`
	Message string `json:"message"`
}

func (e upstreamError) errorType() string {
	if e.Error.Type != "" {
		return e.Error.Type
	}
	return e.Type
}

func (e upstreamError) message() string {
	if e.Error.Message != "" {
		return e.Error.Message
	}
	return e.Message
}

func (e upstreamError) codeString() string {
	switch v := e.Error.Code.(type) {
	case string:
		return v
	case float64:
		return strconv.Itoa(int(v))
	}
	return ""
}

// ── public API ───────────────────────────────────────────────────────────────

// Classify reads the HTTP response status, headers, and body to produce a
// structured Result for the accounthealth tracker.
//
// body may be nil or empty; the status code alone is enough for a coarse
// classification. When body is provided it is consumed only once and must
// already be fully read into memory by the caller.
//
// latency is the end-to-end request duration.
func Classify(status int, header http.Header, body []byte, latency time.Duration) accounthealth.Result {
	r := accounthealth.Result{
		Status:  status,
		Latency: latency,
	}

	if status >= 200 && status < 300 {
		r.Kind = accounthealth.OK
		// Try to extract token usage from a non-streaming response.
		r.Tokens = extractTokens(body)
		return r
	}

	// Parse the retry-after header first — it applies regardless of body.
	r.RetryAfter = RetryAfterDuration(header)

	var env upstreamError
	if len(body) > 0 {
		_ = json.Unmarshal(body, &env)
	}

	r.Kind = classifyStatus(status, env, header)
	return r
}

// ClassifyStreamEnd is called after a streaming response finishes draining.
// It updates the token count from the final usage chunk parsed by the streaming
// layer, which Classify cannot see because the body arrives in pieces.
func ClassifyStreamEnd(r *accounthealth.Result, tokens int) {
	if r == nil {
		return
	}
	if tokens > 0 {
		r.Tokens = tokens
	}
}

// ── internals ────────────────────────────────────────────────────────────────

func classifyStatus(status int, env upstreamError, header http.Header) accounthealth.ErrorKind {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		// 401 / 403 — credentials invalid or revoked. Fatal: need operator action.
		return accounthealth.Fatal

	case http.StatusPaymentRequired:
		// 402 — billing issue. Treat like Fatal but shorter — operator may top up.
		return accounthealth.Fatal

	case http.StatusTooManyRequests:
		return accounthealth.RateLimit

	case http.StatusServiceUnavailable, http.StatusGatewayTimeout, http.StatusBadGateway:
		// 502/503/504 from the provider — transient, worth retrying.
		return accounthealth.NetworkRetry

	case http.StatusInternalServerError:
		// 500 can be transient (overload) or persistent (broken account).
		// Treat as NetworkRetry on first hit; back-off handles repeated failures.
		return accounthealth.NetworkRetry
	}

	// Status is 4xx other than the above — inspect the body for specifics.
	if status >= 400 && status < 500 {
		return classifyBody(env, header)
	}

	// Anything else (1xx, 3xx unexpected, network errors mapped to 0) —
	// treat as a network blip.
	return accounthealth.NetworkRetry
}

func classifyBody(env upstreamError, _ http.Header) accounthealth.ErrorKind {
	errType := strings.ToLower(env.errorType())
	code := strings.ToLower(env.codeString())
	msg := strings.ToLower(env.message())

	// ── Rate-limit signals ─────────────────────────────────────────────────
	if strings.Contains(errType, "rate_limit") ||
		strings.Contains(errType, "rate_limited") ||
		code == "rate_limit_exceeded" ||
		strings.Contains(msg, "rate limit") ||
		strings.Contains(msg, "too many requests") {
		return accounthealth.RateLimit
	}

	// ── Wind-control / suspension signals ─────────────────────────────────
	if code == "20" || // OpenAI internal: account flagged
		strings.Contains(errType, "overloaded") || // Anthropic overload
		strings.Contains(msg, "account has been suspended") ||
		strings.Contains(msg, "your account has been flagged") ||
		strings.Contains(msg, "您的账号") || // Chinese Codex variant
		strings.Contains(msg, "blocked") ||
		strings.Contains(msg, "suspended") {
		return accounthealth.WindControlled
	}

	// ── Model-unavailable signals ──────────────────────────────────────────
	if strings.Contains(errType, "model_not_found") ||
		strings.Contains(errType, "engine_not_found") ||
		code == "model_not_found" ||
		strings.Contains(msg, "model not found") ||
		strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "no access to model") ||
		strings.Contains(msg, "not available in your region") {
		return accounthealth.ModelUnavailable
	}

	// ── Generic credential / permission errors ─────────────────────────────
	if strings.Contains(errType, "invalid_api_key") ||
		strings.Contains(errType, "authentication") ||
		code == "invalid_api_key" {
		return accounthealth.Fatal
	}

	// Unknown 4xx — treat as fatal to avoid hammering a broken account.
	return accounthealth.Fatal
}

// RetryAfterDuration accepts bounded finite seconds, an HTTP date, or the
// provider reset-duration extension. Untrusted headers cannot overflow time.
func RetryAfterDuration(h http.Header) time.Duration {
	for _, name := range []string{"Retry-After", "X-Ratelimit-Reset-Requests"} {
		raw := strings.TrimSpace(h.Get(name))
		if raw == "" || len(raw) > 256 {
			continue
		}
		if secs, err := strconv.ParseFloat(raw, 64); err == nil && !math.IsNaN(secs) && !math.IsInf(secs, 0) && secs >= 0 {
			return time.Duration(math.Min(secs, (24*time.Hour).Seconds()) * float64(time.Second))
		}
		if stamp, err := http.ParseTime(raw); err == nil {
			if delay := time.Until(stamp); delay > 0 {
				return min(delay, 24*time.Hour)
			}
		}
		if name == "X-Ratelimit-Reset-Requests" {
			if delay, err := time.ParseDuration(raw); err == nil && delay > 0 {
				return min(delay, 24*time.Hour)
			}
		}
	}
	return 0
}

// ── token extraction from non-streaming responses ───────────────────────────

type usageEnvelope struct {
	Usage struct {
		// OpenAI / Codex fields
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
		// Anthropic fields
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// extractTokens tries to read token usage from a completed (non-streaming) body.
func extractTokens(body []byte) int {
	if len(body) == 0 {
		return 0
	}
	var u usageEnvelope
	if json.Unmarshal(body, &u) != nil {
		return 0
	}
	if u.Usage.TotalTokens > 0 {
		return u.Usage.TotalTokens
	}
	return u.Usage.InputTokens + u.Usage.OutputTokens
}

// ── streaming token accumulator ──────────────────────────────────────────────

// StreamCounter accumulates token usage from SSE chunks. Create one per
// request and feed each parsed chunk line to Update. Call Total when the
// stream ends.
type StreamCounter struct {
	input  int
	output int
	total  int // if the provider sends a flat total
}

// Update parses one SSE data line and accumulates token counts.
// The line should be the raw JSON after stripping the "data: " prefix.
func (c *StreamCounter) Update(line []byte) {
	if len(line) == 0 || string(line) == "[DONE]" {
		return
	}
	var chunk struct {
		// OpenAI streaming usage (last chunk) and Anthropic message_delta usage.
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
			InputTokens      int `json:"input_tokens"`
			OutputTokens     int `json:"output_tokens"`
		} `json:"usage"`
		// Anthropic message_delta
		Type  string `json:"type"`
		Index int    `json:"index"`
		Delta struct {
			Type string `json:"type"`
		} `json:"delta"`
	}
	if json.Unmarshal(line, &chunk) != nil {
		return
	}
	// OpenAI path
	if chunk.Usage.TotalTokens > 0 {
		c.total = chunk.Usage.TotalTokens
		c.input = chunk.Usage.PromptTokens
		c.output = chunk.Usage.CompletionTokens
		return
	}
	// Anthropic path: usage arrives on message_delta events
	if chunk.Type == "message_delta" {
		if chunk.Usage.InputTokens > 0 {
			c.input = chunk.Usage.InputTokens
		}
		if chunk.Usage.OutputTokens > 0 {
			c.output = chunk.Usage.OutputTokens
		}
	}
}

// Total returns the best total token count seen so far. Returns 0 if no
// usage data was received.
func (c *StreamCounter) Total() int {
	if c.total > 0 {
		return c.total
	}
	return c.input + c.output
}

// Input returns the prompt / input token count.
func (c *StreamCounter) Input() int { return c.input }

// Output returns the completion / output token count.
func (c *StreamCounter) Output() int { return c.output }
