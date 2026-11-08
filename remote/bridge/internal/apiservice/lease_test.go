package apiservice

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
)

func TestPersistedCodexAPIEnablementNeedsCurrentRuntimeLease(t *testing.T) {
	s, live := fixtureService(t)
	s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
		return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
	}
	live.change(func(c *config.Config) {
		c.GatewayRuntimeKey = "runtime-one"
		c.CodexAPI.RuntimeLease = ""
	})
	if got := callAPI(s, syntheticClientKey, `{"model":"gpt-fixture","input":"fixture"}`).Code; got != 503 {
		t.Fatalf("persisted enablement exposed API without lease: status=%d", got)
	}
	live.change(func(c *config.Config) { c.CodexAPI.RuntimeLease = "runtime-one" })
	if got := callAPI(s, syntheticClientKey, `{"model":"gpt-fixture","input":"fixture"}`).Code; got != 200 {
		t.Fatalf("current runtime lease did not enable API: status=%d", got)
	}
	live.change(func(c *config.Config) { c.GatewayRuntimeKey = "runtime-two" })
	if got := callAPI(s, syntheticClientKey, `{"model":"gpt-fixture","input":"fixture"}`).Code; got != 503 {
		t.Fatalf("rotated runtime lease left API active: status=%d", got)
	}
	live.change(func(c *config.Config) { c.CodexAPI.RuntimeLease = "runtime-two" })
	if got := callAPI(s, syntheticClientKey, `{"model":"gpt-fixture","input":"fixture"}`).Code; got != 200 {
		t.Fatalf("re-arming current runtime lease failed: status=%d", got)
	}
}
