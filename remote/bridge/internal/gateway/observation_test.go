package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/usageledger"
)

func observedRequest(t *testing.T, h *Handler, body string) (*httptest.ResponseRecorder, usageledger.Observation) {
	t.Helper()
	var observations []usageledger.Observation
	h.Observe = func(o usageledger.Observation) { observations = append(observations, o) }
	w := callGateway(h, "codex", "responses", body)
	if len(observations) != 1 {
		t.Fatalf("request produced %d observations", len(observations))
	}
	return w, observations[0]
}
func actualToken(t *testing.T, p *int64, want int64) {
	t.Helper()
	if p == nil || *p != want {
		t.Fatalf("actual token value %v want %d", p, want)
	}
}

func TestObservationUsesRawUsageBeforeTranslatorAndDoesNotManufactureMissingValues(t *testing.T) {
	for _, usage := range []string{`,"usage":{"prompt_tokens":11,"completion_tokens":7}`, ""} {
		h := fixtureGateway(t, "codex", "chat", `{"id":"chat-ledger","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"private output"},"finish_reason":"stop"}]`+usage+`}`, "application/json", nil)
		w, o := observedRequest(t, h, `{"model":"chosen-model","input":"private prompt"}`)
		if w.Code != 200 || o.State != "completed" || !o.UsageFinal || o.AccountID != "universal" || o.Model != "chosen-model" || o.Route != "codex/v1/responses" || o.UpstreamHTTPStatus != 200 {
			t.Fatalf("incorrect observation: status=%d %+v", w.Code, o)
		}
		if usage != "" {
			actualToken(t, o.Tokens.Input, 11)
			actualToken(t, o.Tokens.Output, 7)
		} else if o.Tokens.Input != nil || o.Tokens.Output != nil {
			t.Fatal("translated default zero usage entered the ledger")
		}
		if o.Tokens.Total != nil {
			t.Fatal("translator synthesized an unknown total")
		}
	}
}

func TestUsageActivityReleasesAfterFinalObservation(t *testing.T) {
	h := fixtureGateway(t, "codex", "chat", `{"id":"fixture","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`, "application/json", nil)
	var steps []string
	h.BeginUsage = func(id string, epoch uint64) func() {
		if id != "universal" {
			t.Fatal(id)
		}
		steps = append(steps, "begin")
		return func() { steps = append(steps, "release") }
	}
	h.Observe = func(o usageledger.Observation) { steps = append(steps, "observe") }
	w := callGateway(h, "codex", "responses", `{"model":"chosen-model","input":"fixture"}`)
	if w.Code != 200 || strings.Join(steps, ",") != "begin,observe,release" {
		t.Fatal(w.Code, steps)
	}
}

func TestStreamAndBufferedSSECreateOneObservationWithProviderSnapshots(t *testing.T) {
	for _, stream := range []string{"true", "false"} {
		h := fixtureGateway(t, "codex", "anthropic", anthropicFixtureSSE, "text/event-stream", nil)
		w, o := observedRequest(t, h, `{"model":"chosen-model","input":"fixture","stream":`+stream+`}`)
		if w.Code != 200 || o.State != "completed" || !o.UsageFinal {
			t.Fatalf("stream=%s status=%d %+v", stream, w.Code, o)
		}
		actualToken(t, o.Tokens.Input, 12)
		actualToken(t, o.Tokens.Output, 8)
		if o.Tokens.Total != nil {
			t.Fatal("SSE merge fabricated a total")
		}
	}
}

func TestTruncatedAndErrorStreamsKeepOnlyActuallyObservedUsage(t *testing.T) {
	for _, fixture := range []struct {
		raw, state string
		final      bool
	}{
		{"data: {\"type\":\"response.created\",\"response\":{\"id\":\"partial\",\"usage\":{\"input_tokens\":4}}}\n\n", "interrupted", false},
		{"data: {\"type\":\"response.created\",\"response\":{\"id\":\"partial\",\"usage\":{\"input_tokens\":4}}}\n\ndata: {\"type\":\"error\",\"error\":{\"message\":\"private provider error\"}}\n\n", "failed", false},
		{"data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"usage\":{\"input_tokens\":4}}}\n\n", "incomplete", true},
	} {
		h := fixtureGateway(t, "codex", "responses", fixture.raw, "text/event-stream", nil)
		_, o := observedRequest(t, h, `{"model":"chosen-model","input":"fixture","stream":true}`)
		if o.State != fixture.state || o.UsageFinal != fixture.final {
			t.Fatalf("terminal result fabricated: %+v", o)
		}
		actualToken(t, o.Tokens.Input, 4)
		if o.Tokens.Output != nil || o.Tokens.Total != nil {
			t.Fatal("missing interrupted usage became zero")
		}
	}
}

type failedObservationWriter struct{ header http.Header }

func (w *failedObservationWriter) Header() http.Header       { return w.header }
func (w *failedObservationWriter) WriteHeader(int)           {}
func (w *failedObservationWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func (w *failedObservationWriter) Flush()                    {}

func TestClientWriteFailureAndCancelledRequestAreObservedWithoutAffectingInference(t *testing.T) {
	h := fixtureGateway(t, "codex", "responses", `{"object":"response","status":"completed","usage":{"input_tokens":2,"output_tokens":1},"output":[]}`, "application/json", nil)
	observations := make(chan usageledger.Observation, 1)
	h.Observe = func(o usageledger.Observation) { observations <- o }
	r := httptest.NewRequest(http.MethodPost, "/gateway/codex/v1/responses", strings.NewReader(`{"model":"chosen-model","input":"fixture"}`))
	r.RemoteAddr = "127.0.0.1:13000"
	r.Header.Set("Authorization", "Bearer "+localTestKey)
	r.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(&failedObservationWriter{header: make(http.Header)}, r)
	o := <-observations
	if o.State != "interrupted" || !o.UsageFinal {
		t.Fatalf("delivery failure lost provider completion truth: %+v", o)
	}
	actualToken(t, o.Tokens.Input, 2)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r = r.Clone(ctx)
	r.Body = io.NopCloser(strings.NewReader(`{"model":"chosen-model","input":"fixture"}`))
	h.ServeHTTP(httptest.NewRecorder(), r)
	if o := <-observations; o.State != "cancelled" || o.Tokens.Input != nil {
		t.Fatalf("cancellation created usage: %+v", o)
	}
	h.Observe = func(usageledger.Observation) { panic("storage callback failure") }
	if w := callGateway(h, "codex", "responses", `{"model":"chosen-model","input":"fixture"}`); w.Code != 200 {
		t.Fatal("observer failure changed inference")
	}
}

func TestProviderHTTPErrorRecordsStatusAndUsageWithoutRawError(t *testing.T) {
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		_, _ = io.WriteString(w, `{"error":{"message":"private provider error"},"usage":{"input_tokens":3}}`)
	}))
	defer u.Close()
	h := fixtureGateway(t, "codex", "responses", `{}`, "application/json", nil)
	h.Source = func(string) (config.LocalAccount, bool) {
		return config.LocalAccount{ID: "error-fixture", Kind: "codex", Protocol: "responses", BaseURL: u.URL, Key: upstreamTestKey, Model: "chosen-model"}, true
	}
	w, o := observedRequest(t, h, `{"model":"chosen-model","input":"fixture"}`)
	if w.Code != 503 || o.HTTPStatus != 503 || o.UpstreamHTTPStatus != 503 || o.State != "failed" || o.UsageFinal {
		t.Fatalf("provider error observation wrong: %+v", o)
	}
	actualToken(t, o.Tokens.Input, 3)
}

func TestWebSocketObservesOneRealTurnAndDoesNotCountLocalWarmup(t *testing.T) {
	output := `data: {"type":"response.completed","response":{"id":"ledger-socket","object":"response","status":"completed","model":"chosen-model","output":[],"usage":{"input_tokens":4,"output_tokens":2}}}` + "\n\n"
	h := fixtureGateway(t, "codex", "responses", output, "text/event-stream", nil)
	observations := make(chan usageledger.Observation, 4)
	h.Observe = func(o usageledger.Observation) { observations <- o }
	c := wsFixture(t, h)
	wsSend(t, c, map[string]any{"type": "response.create", "model": "chosen-model", "generate": false, "input": []any{map[string]any{"role": "user", "content": "private prompt"}}})
	warm := wsUntil(t, c, "response.completed")["response"].(map[string]any)
	select {
	case <-observations:
		t.Fatal("local warmup created a provider usage record")
	default:
	}
	wsSend(t, c, map[string]any{"type": "response.create", "model": "chosen-model", "previous_response_id": warm["id"], "input": []any{}})
	wsUntil(t, c, "response.completed")
	select {
	case o := <-observations:
		if o.State != "completed" || !o.UsageFinal {
			t.Fatalf("socket provider result lost: %+v", o)
		}
		actualToken(t, o.Tokens.Input, 4)
	case <-time.After(3 * time.Second):
		t.Fatal("socket turn did not produce a usage record")
	}
	_ = c.Close()
	select {
	case <-observations:
		t.Fatal("websocket envelope duplicated a real turn")
	default:
	}
}
