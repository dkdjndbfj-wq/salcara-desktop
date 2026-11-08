package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/usageledger"
)

type attemptControl struct {
	account   config.LocalAccount
	safeRetry bool
	safeInput bool
}

// Only a known refusal establishes that a second credential can safely try.
// Generic 5xx, read failures and transport errors leave execution uncertain.
func responseRejection(status int, code string) (bool, string) {
	switch {
	case (status == 429 || status == 503 || status == 500) && code == "model_capacity":
		return true, "model_unavailable"
	case status == 429:
		return true, "rate_limited"
	case status == 401:
		return true, "auth_rejected"
	default:
		return false, "unknown_result"
	}
}

func executorRejection(err error) (int, time.Duration, bool, string) {
	var local interface{ LocalAdmissionStatus() int }
	if errors.As(err, &local) {
		return 0, 0, false, "unknown_result"
	}
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) {
		return 0, 0, false, "unknown_result"
	}
	code := ""
	var classified interface{ ExecutionCode() string }
	if errors.As(err, &classified) {
		code = classified.ExecutionCode()
	}
	var delay time.Duration
	var retry interface{ RetryAfter() *time.Duration }
	if errors.As(err, &retry) {
		if d := retry.RetryAfter(); d != nil && *d > 0 {
			delay = *d
			if delay > 24*time.Hour {
				delay = 24 * time.Hour
			}
		}
	}
	safe, reason := responseRejection(status.StatusCode(), code)
	return status.StatusCode(), delay, safe, reason
}

func replayableTextBody(body []byte) bool {
	var in map[string]json.RawMessage
	if json.Unmarshal(body, &in) != nil {
		return false
	}
	var structured any
	if json.Unmarshal(body, &structured) != nil || hasPrivateResource(structured) {
		return false
	}
	// Opaque provider state and server-executed tools cannot move between
	// credentials. Client function declarations are safe before execution.
	for _, key := range []string{"previous_response_id", "conversation", "background"} {
		v := strings.TrimSpace(string(in[key]))
		if v != "" && v != "null" && v != `""` && v != "false" {
			return false
		}
	}
	var tools []struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if v := in["tools"]; len(v) != 0 && json.Unmarshal(v, &tools) != nil {
		return false
	}
	for _, tool := range tools {
		if tool.Type != "function" && !(tool.Type == "" && tool.Name != "") {
			return false
		}
	}
	return true
}

// Inspect structural resource fields only. A user's prose mentioning file_id
// is ordinary text; a provider resource reference requires its owning account.
func hasPrivateResource(value any) bool {
	switch node := value.(type) {
	case map[string]any:
		if node["type"] == "item_reference" {
			return true
		}
		for key, child := range node {
			switch strings.ToLower(key) {
			case "file_id", "file_ids", "vector_store_id", "vector_store_ids", "container", "container_id", "encrypted_content", "encrypted_reasoning", "previous_response_id", "conversation":
				if child != nil && child != "" {
					return true
				}
			}
			if hasPrivateResource(child) {
				return true
			}
		}
	case []any:
		for _, child := range node {
			if hasPrivateResource(child) {
				return true
			}
		}
	}
	return false
}

// Error-only buffering retains at most one small local diagnostic. Successful
// HTTP/SSE data is forwarded immediately; emitted bytes can never be replayed.
type attemptWriter struct {
	w         http.ResponseWriter
	header    http.Header
	status    int
	errBody   bytes.Buffer
	committed bool
}

func newAttemptWriter(w http.ResponseWriter) *attemptWriter {
	return &attemptWriter{w: w, header: make(http.Header)}
}
func (w *attemptWriter) Header() http.Header { return w.header }
func (w *attemptWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	if status < 400 {
		w.commit()
	}
}
func (w *attemptWriter) commit() {
	if w.committed {
		return
	}
	w.committed = true
	for k, values := range w.header {
		w.w.Header()[k] = append([]string(nil), values...)
	}
	if w.status == 0 {
		w.status = 200
	}
	w.w.WriteHeader(w.status)
}
func (w *attemptWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	if !w.committed && w.status >= 400 && w.errBody.Len()+len(body) <= maxProviderErrorBytes {
		return w.errBody.Write(body)
	}
	w.commit()
	if w.errBody.Len() != 0 {
		if _, err := w.w.Write(w.errBody.Bytes()); err != nil {
			return 0, err
		}
		w.errBody.Reset()
	}
	return w.w.Write(body)
}
func (w *attemptWriter) Flush() {
	w.commit()
	if w.errBody.Len() != 0 {
		_, _ = w.w.Write(w.errBody.Bytes())
		w.errBody.Reset()
	}
	_ = http.NewResponseController(w.w).Flush()
}
func (w *attemptWriter) finish() {
	w.commit()
	if w.errBody.Len() != 0 {
		_, _ = w.w.Write(w.errBody.Bytes())
		w.errBody.Reset()
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.RetryAccount == nil || r.Method != http.MethodPost || strings.HasSuffix(r.URL.Path, "/compact") || strings.HasSuffix(r.URL.Path, "/count_tokens") {
		h.serveHTTP(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		fail(w, 400, "请求内容无效或超过本地网关限制")
		return
	}
	started := time.Now()
	// The execution deadline is shared across all attempts, not renewed by retry.
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Hour)
	defer cancel()
	inner := *h
	inner.RetryAccount = nil
	release := h.InitialAttemptRelease
	defer func() {
		if release != nil {
			release()
		}
	}()
	var attempts []usageledger.Attempt
	var excluded []string
	var final usageledger.Observation
	for n := 0; n < usageledger.MaxAttempts; n++ {
		control := &attemptControl{}
		inner.attempt = control
		var observation usageledger.Observation
		inner.Observe = func(o usageledger.Observation) { observation = o }
		req := r.Clone(ctx)
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		buffer := newAttemptWriter(w)
		inner.serveHTTP(buffer, req)
		if n == 0 {
			observation.RoutingReason = "pool_selected"
		} else {
			observation.RoutingReason = "failover"
		}
		if len(observation.Attempts) > 0 {
			attempt := observation.Attempts[0]
			if attempt.Reason == "" || attempt.Reason == "fixed_account" {
				attempt.Reason = observation.RoutingReason
			}
			attempts = append(attempts, attempt)
		}
		if observation.FirstByteMS != nil {
			elapsed := observation.StartedAt.Sub(started).Milliseconds() + *observation.FirstByteMS
			observation.FirstByteMS = &elapsed
		}
		final = observation
		identity, valid := CredentialBudgetIdentity(control.account)
		if valid {
			excluded = append(excluded, identity)
		}
		if ctx.Err() != nil || buffer.committed || !valid || !control.safeRetry || !control.safeInput || !replayableTextBody(body) || n == usageledger.MaxAttempts-1 {
			buffer.finish()
			break
		}
		if release != nil {
			release()
			release = nil
		}
		next, current, nextRelease, status := h.RetryAccount(ctx, body, excluded)
		if nextRelease != nil {
			release = nextRelease
		}
		if status != 0 || current == nil || !current() {
			buffer.finish()
			break
		}
		inner.Source = func(string) (config.LocalAccount, bool) { return next, current() }
		inner.Accounts, inner.PoolGroup, inner.PoolEnabled = nil, nil, nil
	}
	if h.Observe != nil && !final.StartedAt.IsZero() {
		final.StartedAt, final.DurationMS, final.Attempts = started, time.Since(started).Milliseconds(), attempts
		h.Observe(final)
	}
}
