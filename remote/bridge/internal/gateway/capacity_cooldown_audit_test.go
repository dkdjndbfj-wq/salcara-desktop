package gateway

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"salcara/bridge/internal/config"
)

type auditAdmissionError struct{}

func (auditAdmissionError) Error() string             { return "synthetic local reserve rejection" }
func (auditAdmissionError) LocalAdmissionStatus() int { return http.StatusServiceUnavailable }

type auditCapacityError struct{}

func (auditCapacityError) Error() string         { return "synthetic provider capacity rejection" }
func (auditCapacityError) StatusCode() int       { return http.StatusServiceUnavailable }
func (auditCapacityError) ExecutionCode() string { return "model_capacity" }

func TestAuditModelCapacityAndLocalAdmissionDoNotCoolOtherModels(t *testing.T) {
	for _, reason := range []string{"local-reserve", "typed-capacity", "http-capacity", "sse-capacity"} {
		t.Run(reason, func(t *testing.T) {
			a := officialAccountFixture()
			a.Models = append(a.Models, "gpt-available")
			h := New(func() string { return localTestKey }, func(string) (config.LocalAccount, bool) { return a, true })
			h.Pool, h.Accounts, h.PoolEnabled = NewAccountPool(), func(string) []config.LocalAccount { return []config.LocalAccount{a} }, func(string) bool { return false }
			var calls atomic.Int32
			h.Official = func(_ context.Context, _ config.LocalAccount, body []byte, _ bool, _ bool, _ func() bool) (*http.Response, error) {
				calls.Add(1)
				if strings.Contains(string(body), "gpt-available") {
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"available-response","object":"response","status":"completed","output":[]}`))}, nil
				}
				switch reason {
				case "local-reserve":
					return nil, auditAdmissionError{}
				case "typed-capacity":
					return nil, auditCapacityError{}
				case "http-capacity":
					return &http.Response{StatusCode: 503, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"model_capacity","message":"Selected model is at capacity. Please try a different model."}}`))}, nil
				default:
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"model_capacity\"}}}\n\n"))}, nil
				}
			}
			stream := "false"
			if reason == "sse-capacity" {
				stream = "true"
			}
			first := callGateway(h, "codex", "responses", `{"model":"gpt-fixture","input":"synthetic","stream":`+stream+`}`)
			if calls.Load() != 1 || reason != "sse-capacity" && first.Code != http.StatusServiceUnavailable {
				t.Fatal("first synthetic rejection did not execute", calls.Load(), first.Code, first.Body.String())
			}
			second := callGateway(h, "codex", "responses", `{"model":"gpt-available","input":"synthetic"}`)
			if second.Code != http.StatusOK || calls.Load() != 2 {
				t.Fatalf("other model was blocked by non-credential failure: status=%d calls=%d body=%s", second.Code, calls.Load(), second.Body.String())
			}
		})
	}
}

func TestAuditCredentialRateLimitStillCoolsFixedRoute(t *testing.T) {
	a := officialAccountFixture()
	h := New(func() string { return localTestKey }, func(string) (config.LocalAccount, bool) { return a, true })
	h.Pool, h.Accounts, h.PoolEnabled = NewAccountPool(), func(string) []config.LocalAccount { return []config.LocalAccount{a} }, func(string) bool { return false }
	var calls atomic.Int32
	h.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 429, Header: http.Header{"Content-Type": {"application/json"}, "Retry-After": {"60"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"rate_limit_exceeded"}}`))}, nil
	}
	callGateway(h, "codex", "responses", `{"model":"gpt-fixture","input":"synthetic"}`)
	if second := callGateway(h, "codex", "responses", `{"model":"gpt-fixture","input":"synthetic"}`); second.Code == 200 || calls.Load() != 1 {
		t.Fatal("real credential rate limit did not retain cooldown", second.Code, calls.Load())
	}
}
