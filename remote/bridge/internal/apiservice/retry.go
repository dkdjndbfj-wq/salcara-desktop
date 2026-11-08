// retry.go — multi-account retry loop for the named-text-pool path.
//
// When a request to an account fails with a retryable error (429, 5xx, network),
// RetryPolicy selects the next best candidate from the pool and retries
// transparently, up to len(candidates) attempts. The caller's response writer
// is only written on the final attempt (or the first non-retryable failure).
//
// This file belongs in internal/apiservice/ alongside service.go.
package apiservice

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"salcara/bridge/internal/accounthealth"
	"salcara/bridge/internal/apierror"
	"salcara/bridge/internal/config"
)

// RetryableStatus reports whether an HTTP status code warrants an account
// switch. We retry on 429 and all 5xx except 501 (not implemented), which is
// a permanent signal that the endpoint doesn't support the operation.
func RetryableStatus(status int) bool {
	if status == http.StatusTooManyRequests {
		return true
	}
	return status >= 500 && status != http.StatusNotImplemented
}

// DispatchFunc is the signature of the function that actually sends one
// attempt to a specific account. It must:
//   - Write the full response to w on success.
//   - Return (status, nil) where status is the HTTP status code written.
//   - Return (status, err) if the attempt fails hard (network error, etc.).
//
// The implementation lives in service.go and wraps the gateway.Handler call.
type DispatchFunc func(ctx context.Context, w http.ResponseWriter, account config.LocalAccount, body []byte) (status int, err error)

// bufferingWriter captures a response until we decide whether to retry.
type bufferingWriter struct {
	header      http.Header
	code        int
	buf         bytes.Buffer
	wroteHeader bool
}

func newBufferingWriter() *bufferingWriter {
	return &bufferingWriter{header: make(http.Header), code: http.StatusOK}
}

func (b *bufferingWriter) Header() http.Header { return b.header }

func (b *bufferingWriter) WriteHeader(code int) {
	if !b.wroteHeader {
		b.code = code
		b.wroteHeader = true
	}
}

func (b *bufferingWriter) Write(p []byte) (int, error) {
	if !b.wroteHeader {
		b.code = http.StatusOK
		b.wroteHeader = true
	}
	return b.buf.Write(p)
}

func (b *bufferingWriter) flush(w http.ResponseWriter) {
	for k, vs := range b.header {
		// Never forward Content-Length — the buffered body size is authoritative
		// and the upstream value may be stale (chunked encoding, compression, etc.).
		if strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(b.code)
	_, _ = io.Copy(w, &b.buf)
}

// RetryRequest attempts to send a request using accounts from candidates in
// health-score order. On a retryable failure it records the outcome in Health,
// marks the account cooling, then picks the next candidate.
//
// It writes the response to w exactly once — either the first success or the
// last failure if all accounts are exhausted.
//
// accounts must be pre-filtered (HealthyAccounts) and pre-sorted (ScoreAccounts)
// by the caller before passing here.
func RetryRequest(
	ctx context.Context,
	w http.ResponseWriter,
	accounts []config.LocalAccount,
	body []byte,
	health *accounthealth.Tracker,
	dispatch DispatchFunc,
) {
	if len(accounts) == 0 {
		failJSON(w, http.StatusServiceUnavailable, "no_accounts", "没有可用的账号")
		return
	}

	var lastBuf *bufferingWriter
	var lastAccount string

	for i, acct := range accounts {
		// Check context before each attempt.
		if ctx.Err() != nil {
			break
		}

		buf := newBufferingWriter()
		start := time.Now()
		status, err := dispatch(ctx, buf, acct, body)
		latency := time.Since(start)

		if err != nil {
			// Hard network error — record and try next account.
			if health != nil {
				health.Record(acct.ID, apierror.Classify(0, http.Header{}, nil, latency))
			}
			lastBuf = buf
			lastAccount = acct.ID
			continue
		}

		// Classify the response.
		var respBody []byte
		if buf.buf.Len() > 0 && buf.buf.Len() <= 64*1024 {
			respBody = buf.buf.Bytes()
		}
		result := apierror.Classify(status, buf.header, respBody, latency)
		if health != nil {
			health.Record(acct.ID, result)
		}

		if !RetryableStatus(status) || result.Kind == accounthealth.ModelUnavailable {
			// Success, or a non-retryable error (bad credentials, wrong model, etc.)
			// — flush the buffered response and return.
			buf.flush(w)
			return
		}

		// Retryable: record, keep the buffer as last-known, try next account.
		lastBuf = buf
		lastAccount = acct.ID
		_ = lastAccount // used for logging extension points

		isLast := i == len(accounts)-1
		if isLast {
			break
		}
	}

	// All accounts failed or exhausted — flush the last buffered response.
	if lastBuf != nil {
		lastBuf.flush(w)
		return
	}
	failJSON(w, http.StatusBadGateway, "retry_exhausted", "所有账号均不可用，请稍后重试")
}

// failJSON writes a simple JSON error body.
func failJSON(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"type":    "salcara_retry_error",
			"code":    code,
			"message": message,
		},
	})
}
