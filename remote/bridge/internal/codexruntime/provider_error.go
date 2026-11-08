package codexruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

const maxProviderErrorBytes = 64 << 10

// providerExecutionError deliberately retains only routing-safe metadata.
// Error never returns the SDK/provider body, which may contain credentials,
// URLs, request content, or other account-private diagnostics.
type providerExecutionError struct {
	status        int
	executionCode string
	retryAfter    time.Duration
}

func (e providerExecutionError) Error() string         { return ErrUnavailable.Error() }
func (e providerExecutionError) StatusCode() int       { return e.status }
func (e providerExecutionError) ExecutionCode() string { return e.executionCode }
func (e providerExecutionError) Unwrap() error         { return ErrUnavailable }
func (e providerExecutionError) RetryAfter() *time.Duration {
	if e.retryAfter <= 0 {
		return nil
	}
	d := e.retryAfter
	return &d
}

// normalizeProviderError preserves canonical local cancellation and a valid
// upstream HTTP status. The only provider condition allowed across this
// boundary is model_capacity; every provider string remains private.
func normalizeProviderError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	var status executor.StatusError
	if !errors.As(err, &status) || status.StatusCode() < 400 || status.StatusCode() > 599 {
		return ErrUnavailable
	}
	var retry interface{ RetryAfter() *time.Duration }
	var delay time.Duration
	if errors.As(err, &retry) {
		if value := retry.RetryAfter(); value != nil && *value > 0 {
			delay = *value
			if delay > 24*time.Hour {
				delay = 24 * time.Hour
			}
		}
	}
	return providerExecutionError{
		status:        status.StatusCode(),
		executionCode: providerErrorExecutionCode(err.Error()),
		retryAfter:    delay,
	}
}

// providerErrorExecutionCode reads only bounded, known SDK error fields. Exact
// legacy phrases cover pinned SDK variants that expose a message without a
// stable code. Substring matching is intentionally rejected: arbitrary error
// prose must not manufacture a capacity condition.
func providerErrorExecutionCode(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxProviderErrorBytes {
		return ""
	}
	if isExactCapacityMessage(raw) {
		return "model_capacity"
	}
	type providerErrorFields struct {
		Message string `json:"message"`
		Code    string `json:"code"`
		Type    string `json:"type"`
	}
	var body struct {
		providerErrorFields
		Error    *providerErrorFields `json:"error"`
		Response *struct {
			Error *providerErrorFields `json:"error"`
		} `json:"response"`
	}
	if json.Unmarshal([]byte(raw), &body) != nil {
		return ""
	}
	fields := []*providerErrorFields{&body.providerErrorFields, body.Error}
	if body.Response != nil {
		fields = append(fields, body.Response.Error)
	}
	for _, field := range fields {
		if field != nil && isCapacityFields(*field) {
			return "model_capacity"
		}
	}
	return ""
}

func isCapacityFields(field struct {
	Message string `json:"message"`
	Code    string `json:"code"`
	Type    string `json:"type"`
}) bool {
	for _, value := range []string{field.Code, field.Type} {
		if len(value) > 128 {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "model_capacity", "model_at_capacity", "model_is_at_capacity", "server_overloaded", "server_is_overloaded":
			return true
		}
	}
	return len(field.Message) <= 512 && isExactCapacityMessage(field.Message)
}

func isExactCapacityMessage(message string) bool {
	message = strings.TrimSpace(message)
	for _, known := range []string{
		"Selected model is at capacity. Please try a different model.",
		"Model is at capacity. Please try again later.",
		"Our servers are currently overloaded. Please try again later.",
	} {
		if strings.EqualFold(message, known) {
			return true
		}
	}
	return false
}
