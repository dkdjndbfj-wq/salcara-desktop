package apiservice

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"salcara/bridge/internal/config"
)

func TestNamedResponsesRejectsCaseConfusableModelBeforeExecutor(t *testing.T) {
	s, live := fixtureService(t)
	live.change(func(c *config.Config) { c.CodexAPI.Clients[0].ModelAllowlist = []string{"gpt-fixture"} })
	var calls atomic.Int32
	s.Official = func(_ context.Context, _ config.LocalAccount, body []byte, _, _ bool, _ func() bool) (*http.Response, error) {
		calls.Add(1)
		return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
	}
	for _, path := range []string{"v1/responses", "v1/responses/compact"} {
		for _, body := range []string{
			`{"model":"gpt-fixture","Model":"gpt-denied","input":"synthetic"}`,
			`{"model":"gpt-denied","model":"gpt-fixture","input":"synthetic"}`,
			`{"model":"gpt-fixture","input":"synthetic","Stream":true}`,
			`{"model":"gpt-fixture","input":"synthetic","Previous_Response_ID":"external"}`,
		} {
			r := apiRequest(context.Background(), syntheticClientKey, strings.NewReader(body))
			r.URL.Path = "/codex-api/" + path
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != 400 || calls.Load() != 0 {
				t.Fatalf("ambiguous client model passed policy boundary: path=%s status=%d calls=%d", path, w.Code, calls.Load())
			}
		}
	}
}
