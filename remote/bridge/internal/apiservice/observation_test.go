package apiservice

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/usageledger"
)

func TestNamedAPIObservesAliasProviderUsageOnceWithoutNestedGatewayDuplicates(t *testing.T) {
	s, _ := fixtureService(t)
	var observations []usageledger.Observation
	s.Observe = func(o usageledger.Observation) { observations = append(observations, o) }
	s.Official = func(_ context.Context, a config.LocalAccount, body []byte, stream, compact bool, _ func() bool) (*http.Response, error) {
		return successfulResponse(io.NopCloser(strings.NewReader(`{"object":"response","status":"completed","model":"gpt-fixture","output":[],"usage":{"input_tokens":11,"output_tokens":0}}`)), false), nil
	}
	w := callAPI(s, syntheticClientKey, `{"model":"friendly","input":"private fixture prompt"}`)
	if w.Code != 200 || len(observations) != 1 {
		t.Fatalf("named API duplicates/lost observation: status=%d records=%d", w.Code, len(observations))
	}
	o := observations[0]
	if o.AccountID != "account-A" || o.ClientID != "client-A" || o.Route != "codex-api/v1/responses" || o.Model != "gpt-fixture" || o.HTTPStatus != 200 || o.UpstreamHTTPStatus != 200 || o.State != "completed" || !o.UsageFinal {
		t.Fatalf("named metadata mismatched: %+v", o)
	}
	if o.Tokens.Input == nil || *o.Tokens.Input != 11 || o.Tokens.Output == nil || *o.Tokens.Output != 0 || o.Tokens.Total != nil {
		t.Fatal("unknown or zero upstream usage changed")
	}
}

func TestNamedAPIRecordsAuthenticatedRejectionWithoutExecutingOrFakeUsage(t *testing.T) {
	s, live := fixtureService(t)
	var observations []usageledger.Observation
	s.Observe = func(o usageledger.Observation) { observations = append(observations, o) }
	s.mu.Lock()
	s.inflight = maxServiceInflight
	s.mu.Unlock()
	w := callAPI(s, syntheticClientKey, `{"model":"gpt-fixture","input":"fixture"}`)
	if w.Code != 429 || len(observations) != 1 || observations[0].State != "rejected" || observations[0].HTTPStatus != 429 || observations[0].Tokens.Input != nil {
		t.Fatal("admission rejection lost or acquired fake usage")
	}
	live.change(func(c *config.Config) { c.LocalAccounts = nil })
	w = callAPI(s, syntheticClientKey, `{"model":"gpt-fixture","input":"fixture"}`)
	if w.Code != 409 || len(observations) != 2 || observations[1].HTTPStatus != 409 {
		t.Fatal("removed bound account rejection lost")
	}
	w = callAPI(s, "invalid", `{"model":"gpt-fixture","input":"fixture"}`)
	if w.Code != 401 || len(observations) != 2 {
		t.Fatal("unauthenticated attempt entered account usage ledger")
	}
}

func TestNamedAPIObserverFailureDoesNotChangeInference(t *testing.T) {
	s, _ := fixtureService(t)
	s.Observe = func(usageledger.Observation) { panic("ledger failure") }
	s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
		return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
	}
	if w := callAPI(s, syntheticClientKey, `{"model":"gpt-fixture","input":"fixture"}`); w.Code != 200 {
		t.Fatal("ledger failure broke named inference")
	}
}

func TestNamedAPIUsageActivityHeldThroughOuterObservation(t *testing.T) {
	s, _ := fixtureService(t)
	var steps []string
	s.BeginUsage = func(id string, epoch uint64) func() {
		if id != "account-A" {
			t.Fatal(id)
		}
		steps = append(steps, "begin")
		return func() { steps = append(steps, "release") }
	}
	s.Observe = func(o usageledger.Observation) { steps = append(steps, "observe") }
	s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
		return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
	}
	w := callAPI(s, syntheticClientKey, `{"model":"gpt-fixture","input":"fixture"}`)
	if w.Code != 200 || strings.Join(steps, ",") != "begin,observe,release" {
		t.Fatal(w.Code, steps)
	}
}

func TestNamedAPIPostUpgradeHeadersKeepUsageActivityUntilObservation(t *testing.T) {
	for _, path := range []string{"v1/responses", "v1/images/generations"} {
		t.Run(path, func(t *testing.T) {
			s, _ := imageService(t)
			var steps []string
			active := false
			s.BeginUsage = func(id string, epoch uint64) func() {
				if active || id != "account-A" || epoch != 1 {
					t.Errorf("invalid lease admission: active=%t id=%q epoch=%d", active, id, epoch)
				}
				active = true
				steps = append(steps, "begin")
				return func() {
					if !active {
						t.Error("usage lease released more than once")
					}
					active = false
					steps = append(steps, "release")
				}
			}
			s.Observe = func(o usageledger.Observation) {
				if !active || o.AccountID != "account-A" || o.AccountEpoch != 1 {
					t.Errorf("final observation was not protected by its account lease: active=%t account=%q epoch=%d", active, o.AccountID, o.AccountEpoch)
				}
				steps = append(steps, "observe")
			}
			s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
				steps = append(steps, "provider")
				return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
			}
			s.OfficialImages = func(context.Context, config.LocalAccount, []byte, bool, string, func() bool) (*http.Response, error) {
				steps = append(steps, "provider")
				return successfulResponse(io.NopCloser(strings.NewReader(syntheticImageResult)), false), nil
			}
			body := `{"model":"gpt-fixture","input":"fixture"}`
			if path == "v1/images/generations" {
				body = `{"model":"gpt-image-2","prompt":"fixture"}`
			}
			r := apiRequest(context.Background(), syntheticClientKey, strings.NewReader(body))
			r.URL.Path = "/codex-api/" + path
			r.Header.Set("Connection", "Upgrade")
			r.Header.Set("Upgrade", "websocket")
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != http.StatusOK || strings.Join(steps, ",") != "begin,provider,observe,release" || active {
				t.Fatalf("POST lease lifecycle: status=%d steps=%v active=%t", w.Code, steps, active)
			}
		})
	}
}
