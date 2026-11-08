package gateway

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
)

func TestGatewayCapacityIsStructuredAndNeverLeaksProviderBody(t *testing.T) {
	for _, stream := range []bool{false, true} {
		a := officialAccountFixture()
		h := New(func() string { return localTestKey }, func(string) (config.LocalAccount, bool) { return a, true })
		h.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
			body := `{"error":{"message":"Selected model is at capacity. Please try a different model. private-provider-key","code":"server_overloaded"},"private":"provider-private-url"}`
			status, content := http.StatusServiceUnavailable, "application/json"
			if stream {
				status, content = http.StatusOK, "text/event-stream"
				body = "event: response.failed\ndata: " + `{"type":"response.failed","response":` + body + "}\n\n"
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {content}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		}
		flag := "false"
		if stream {
			flag = "true"
		}
		w := callGateway(h, "codex", "responses", `{"model":"gpt-fixture","input":"hello","stream":`+flag+`}`)
		body := w.Body.String()
		if !strings.Contains(body, `"code":"model_capacity"`) || !strings.Contains(body, capacityMessage) || strings.Contains(body, "private") || strings.Contains(body, "response.completed") {
			t.Fatalf("capacity lost or private/completed content escaped: %s", body)
		}
	}
}

func TestProviderCapacityClassificationIsBoundedAndNotGenericRateLimit(t *testing.T) {
	for _, body := range []string{`{"error":{"message":"HTTP 429 rate limit"}}`, `{"error":"Selected model is at capacity"}`, `{"error":{"message":"Selected model is at capacity` + strings.Repeat("x", maxProviderErrorBytes) + `"}}`} {
		if providerExecutionCode([]byte(body)) != "" {
			t.Fatal("invalid, oversized, or rate error classified as capacity")
		}
	}
}
