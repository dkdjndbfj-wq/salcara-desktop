package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/config"
)

func officialAccountFixture() config.LocalAccount {
	return config.LocalAccount{ID: "official-fixture", Kind: "codex", SourceType: "official_account", CredentialRef: "synthetic-credential-ref", CredentialEpoch: 1, BaseURL: "https://chatgpt.com/backend-api/codex", Model: "gpt-fixture", Protocol: "responses"}
}

const officialFixtureResponse = `{"id":"synthetic-response","object":"response","model":"gpt-fixture","output":[]}`

func TestOfficialContinuationCannotSilentlyLoseContext(t *testing.T) {
	for _, field := range []string{`"previous_response_id":"old-response"`, `"conversation":"old-conversation"`, `"conversation":{"id":"old-conversation"}`} {
		for _, stream := range []bool{false, true} {
			a := officialAccountFixture()
			h := New(func() string { return localTestKey }, func(string) (config.LocalAccount, bool) { return a, true })
			h.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
				t.Error("stateless SDK called with unsupported continuation")
				return nil, errors.New("must not call")
			}
			streamJSON := "false"
			if stream {
				streamJSON = "true"
			}
			w := callGateway(h, "codex", "responses", `{"model":"gpt-fixture","input":"only new input","stream":`+streamJSON+`,`+field+`}`)
			if w.Code != 400 {
				t.Fatalf("continuation accepted: %s status=%d", field, w.Code)
			}
		}
	}
}

type officialBody struct {
	io.ReadCloser
	closed atomic.Bool
}

func (b *officialBody) Close() error { b.closed.Store(true); return b.ReadCloser.Close() }

func TestOfficialCallbackHTTPAndCompactNeedNoPlaintextAccountKey(t *testing.T) {
	for _, compact := range []bool{false, true} {
		t.Run(map[bool]string{false: "responses", true: "compact"}[compact], func(t *testing.T) {
			a := officialAccountFixture()
			h := New(func() string { return localTestKey }, func(string) (config.LocalAccount, bool) { return a, true })
			var calls atomic.Int32
			body := &officialBody{ReadCloser: io.NopCloser(strings.NewReader(officialFixtureResponse))}
			h.Official = func(_ context.Context, selected config.LocalAccount, input []byte, stream, gotCompact bool, valid func() bool) (*http.Response, error) {
				calls.Add(1)
				if selected.CredentialRef != a.CredentialRef || selected.ID != a.ID || selected.Key != "" || stream || gotCompact != compact || valid == nil || !valid() {
					t.Error("selected official account or flags changed")
				}
				if !strings.Contains(string(input), "fixture input") {
					t.Error("input lost")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: body}, nil
			}
			path := "responses"
			if compact {
				path = "responses/compact"
			}
			w := callGateway(h, "codex", path, `{"model":"gpt-fixture","input":"fixture input"}`)
			if w.Code != 200 || calls.Load() != 1 || !body.closed.Load() {
				t.Fatalf("official reply rejected: status=%d calls=%d closed=%v body=%s", w.Code, calls.Load(), body.closed.Load(), w.Body)
			}
		})
	}
}

func TestOfficialCallbackNativeSSEClosesBodyAndPreservesTerminal(t *testing.T) {
	a := officialAccountFixture()
	h := New(func() string { return localTestKey }, func(string) (config.LocalAccount, bool) { return a, true })
	body := &officialBody{ReadCloser: io.NopCloser(strings.NewReader("event: response.completed\ndata: " + officialFixtureResponse[:len(officialFixtureResponse)-1] + `,"type":"response.completed"}` + "\n\n"))}
	h.Official = func(_ context.Context, _ config.LocalAccount, _ []byte, stream, compact bool, valid func() bool) (*http.Response, error) {
		if !stream || compact || !valid() {
			t.Error("incorrect stream request")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body}, nil
	}
	w := callGateway(h, "codex", "responses", `{"model":"gpt-fixture","stream":true,"input":"fixture input"}`)
	if w.Code != 200 || !body.closed.Load() || !strings.Contains(w.Body.String(), "response.completed") || strings.Contains(w.Body.String(), "upstream_stream_error") {
		t.Fatalf("official SSE failed: %d %s", w.Code, w.Body)
	}
}

type officialSlowBody struct {
	entered, release chan struct{}
	reader           *strings.Reader
	once             sync.Once
}

func (b *officialSlowBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered); <-b.release })
	return b.reader.Read(p)
}
func (*officialSlowBody) Close() error { return nil }
func TestOfficialCredentialChangeDuringBodyReadNeverCallsExecutor(t *testing.T) {
	var mu sync.Mutex
	a := officialAccountFixture()
	h := New(func() string { return localTestKey }, func(string) (config.LocalAccount, bool) { mu.Lock(); defer mu.Unlock(); return a, true })
	var calls atomic.Int32
	h.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("must not execute")
	}
	body := &officialSlowBody{entered: make(chan struct{}), release: make(chan struct{}), reader: strings.NewReader(`{"model":"gpt-fixture","input":"fixture"}`)}
	request := httptest.NewRequest(http.MethodPost, "/gateway/codex/v1/responses", body)
	request.RemoteAddr = "127.0.0.1:15321"
	request.Header.Set("Authorization", "Bearer "+localTestKey)
	request.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); h.ServeHTTP(w, request) }()
	select {
	case <-body.entered:
	case <-done:
		t.Fatalf("request refused before fixture body read: %d %s", w.Code, w.Body)
	case <-time.After(3 * time.Second):
		t.Fatal("fixture body read did not begin")
	}
	mu.Lock()
	a.CredentialEpoch++
	mu.Unlock()
	close(body.release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("gateway request stuck")
	}
	if calls.Load() != 0 || w.Code != 409 {
		t.Fatalf("stale official snapshot executed: status=%d calls=%d body=%s", w.Code, calls.Load(), w.Body)
	}
}

func TestOfficialWebSocketRequiresProtectedExecutor(t *testing.T) {
	a := officialAccountFixture()
	h := New(func() string { return localTestKey }, func(string) (config.LocalAccount, bool) { return a, true })
	var calls atomic.Int32
	r := httptest.NewRequest(http.MethodGet, "/gateway/codex/v1/responses", nil)
	r.RemoteAddr = "127.0.0.1:15321"
	r.Header.Set("Authorization", "Bearer "+localTestKey)
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 501 || calls.Load() != 0 {
		t.Fatalf("official websocket reached executor: %d calls=%d", w.Code, calls.Load())
	}
}
