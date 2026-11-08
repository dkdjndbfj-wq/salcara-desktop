package apiservice

import (
	"context"
	"encoding/json"
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

const syntheticClientKey = "synthetic-client-A-secret-not-real"
const syntheticKeyB = "synthetic-client-B-secret-not-real"
const fixtureResponse = `{"id":"synthetic-response","object":"response","model":"gpt-fixture","output":[]}`

type liveConfig struct {
	mu    sync.Mutex
	value config.Config
}

func (c *liveConfig) get() config.Config             { c.mu.Lock(); defer c.mu.Unlock(); return c.value.Clone() }
func (c *liveConfig) change(fn func(*config.Config)) { c.mu.Lock(); defer c.mu.Unlock(); fn(&c.value) }
func fixtureService(t *testing.T) (*Service, *liveConfig) {
	t.Helper()
	live := &liveConfig{value: config.Config{CodexAPI: config.CodexAPIService{Enabled: true, Clients: []config.NamedAPIClient{
		{ID: "client-A", Name: "Fixture A", AccountID: "account-A", KeyDigest: config.APIClientDigest(syntheticClientKey), MaxConcurrency: 1, ModelAliases: map[string]string{"friendly": "gpt-fixture"}},
		{ID: "client-B", Name: "Fixture B", AccountID: "account-B", KeyDigest: config.APIClientDigest(syntheticKeyB), MaxConcurrency: 1},
	}}, LocalAccounts: []config.LocalAccount{
		{ID: "account-A", Name: "A", Kind: "api", SourceType: "official_account", CredentialRef: "credential-A", CredentialEpoch: 1, BaseURL: "https://chatgpt.com/backend-api/codex", Model: "gpt-fixture", Models: []string{"gpt-fixture"}, PoolEnabled: true, PoolGroup: "shared"},
		{ID: "account-B", Name: "B", Kind: "api", SourceType: "official_account", CredentialRef: "credential-B", CredentialEpoch: 1, BaseURL: "https://chatgpt.com/backend-api/codex", Model: "gpt-fixture", Models: []string{"gpt-fixture"}, PoolEnabled: true, PoolGroup: "shared"},
	}}}
	return New(live.get), live
}
func apiRequest(ctx context.Context, key string, body io.Reader) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/codex-api/v1/responses", body).WithContext(ctx)
	r.RemoteAddr = "127.0.0.1:14551"
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("Content-Type", "application/json")
	return r
}
func callAPI(s *Service, key, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.ServeHTTP(w, apiRequest(context.Background(), key, strings.NewReader(body)))
	return w
}
func successfulResponse(body io.ReadCloser, stream bool) *http.Response {
	kind := "application/json"
	if stream {
		kind = "text/event-stream"
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {kind}}, Body: body}
}
func awaitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("request or producer did not stop")
	}
}

func TestNamedClientsRemainPinnedDespiteOtherPoolMembers(t *testing.T) {
	s, _ := fixtureService(t)
	var mu sync.Mutex
	var selected []string
	s.Official = func(_ context.Context, a config.LocalAccount, body []byte, stream, compact bool, _ func() bool) (*http.Response, error) {
		mu.Lock()
		selected = append(selected, a.ID+":"+a.CredentialRef)
		mu.Unlock()
		var input map[string]any
		if json.Unmarshal(body, &input) != nil || input["model"] != "gpt-fixture" || stream || compact {
			t.Error("wrong protocol, alias, or flags")
		}
		return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
	}
	for _, key := range []string{syntheticClientKey, syntheticKeyB} {
		w := callAPI(s, key, `{"model":"gpt-fixture","input":"fixture input"}`)
		if w.Code != 200 {
			t.Fatalf("fixed-account request: %d %s", w.Code, w.Body)
		}
	}
	if len(selected) != 2 || selected[0] != "account-A:credential-A" || selected[1] != "account-B:credential-B" {
		t.Fatalf("account substitution: %v", selected)
	}
	w := callAPI(s, syntheticClientKey, `{"model":"friendly","input":"fixture input"}`)
	if w.Code != 200 {
		t.Fatalf("model alias: %d %s", w.Code, w.Body)
	}
	if selected[2] != "account-A:credential-A" {
		t.Fatal("alias changed granted account")
	}
}

type blockedBody struct {
	entered chan struct{}
	release chan struct{}
	reader  *strings.Reader
	once    sync.Once
}

func (b *blockedBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered); <-b.release })
	return b.reader.Read(p)
}
func (*blockedBody) Close() error { return nil }

func TestClientRevocationAliasAndAccountChangesDuringBodyReadNeverReachUpstream(t *testing.T) {
	for _, change := range []string{"disable", "disable-client", "client-policy", "revoke", "alias", "account-epoch", "account-removal"} {
		t.Run(change, func(t *testing.T) {
			s, live := fixtureService(t)
			var calls atomic.Int32
			s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
				calls.Add(1)
				return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
			}
			body := &blockedBody{entered: make(chan struct{}), release: make(chan struct{}), reader: strings.NewReader(`{"model":"friendly","input":"fixture"}`)}
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { defer close(done); s.ServeHTTP(w, apiRequest(context.Background(), syntheticClientKey, body)) }()
			<-body.entered
			live.change(func(c *config.Config) {
				switch change {
				case "disable":
					c.CodexAPI.Enabled = false
				case "disable-client":
					c.CodexAPI.Clients[0].Disabled = true
				case "client-policy":
					c.CodexAPI.Clients[0].ModelDenylist = []string{"gpt-fixture"}
				case "revoke":
					c.CodexAPI.Clients = c.CodexAPI.Clients[1:]
				case "alias":
					c.CodexAPI.Clients[0].ModelAliases["friendly"] = "gpt-new"
				case "account-epoch":
					c.LocalAccounts[0].CredentialEpoch++
				case "account-removal":
					c.LocalAccounts = c.LocalAccounts[1:]
				}
			})
			close(body.release)
			awaitDone(t, done)
			if calls.Load() != 0 || w.Code < 400 {
				t.Fatalf("changed authorization reached upstream: calls=%d status=%d body=%s", calls.Load(), w.Code, w.Body)
			}
		})
	}
}

func TestDisableDeniesNewRequestsAndDrainsAdmittedRequest(t *testing.T) {
	s, live := fixtureService(t)
	entered, release := make(chan struct{}), make(chan struct{})
	s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
		close(entered)
		<-release
		return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
	}
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.ServeHTTP(w, apiRequest(context.Background(), syntheticClientKey, strings.NewReader(`{"model":"gpt-fixture","input":"fixture"}`)))
	}()
	<-entered
	live.change(func(c *config.Config) { c.CodexAPI.Enabled = false })
	next := callAPI(s, syntheticClientKey, `{"model":"gpt-fixture","input":"fixture"}`)
	if next.Code != 503 {
		t.Fatalf("disabled service admitted new request: %d", next.Code)
	}
	stats := s.Snapshot()
	if len(stats) != 1 || stats[0].Inflight != 1 {
		t.Fatalf("admitted request was not tracked: %+v", stats)
	}
	close(release)
	awaitDone(t, done)
	if w.Code != 200 {
		t.Fatalf("admitted request did not drain: %d %s", w.Code, w.Body)
	}
	if stats = s.Snapshot(); stats[0].Inflight != 0 || stats[0].Succeeded != 1 {
		t.Fatalf("drain stats: %+v", stats)
	}
}

func TestConcurrencyCapReleasesAfterFailureAndCancellationAndErrorsAreSanitized(t *testing.T) {
	for _, finish := range []string{"failure", "cancel"} {
		t.Run(finish, func(t *testing.T) {
			s, _ := fixtureService(t)
			entered, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			s.Official = func(ctx context.Context, _ config.LocalAccount, _ []byte, _, _ bool, _ func() bool) (*http.Response, error) {
				if calls.Add(1) == 1 {
					close(entered)
					if finish == "cancel" {
						<-ctx.Done()
						return nil, errors.New("fixture private " + syntheticClientKey)
					}
					<-release
					return nil, errors.New("fixture private " + syntheticClientKey)
				}
				return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			w := httptest.NewRecorder()
			go func() {
				defer close(done)
				s.ServeHTTP(w, apiRequest(ctx, syntheticClientKey, strings.NewReader(`{"model":"gpt-fixture","input":"fixture"}`)))
			}()
			<-entered
			denied := callAPI(s, syntheticClientKey, `{"model":"gpt-fixture","input":"fixture"}`)
			if denied.Code != 429 || calls.Load() != 1 {
				t.Fatal("concurrency cap failed")
			}
			if finish == "cancel" {
				cancel()
			} else {
				close(release)
			}
			awaitDone(t, done)
			if w.Code != 502 || strings.Contains(w.Body.String(), syntheticClientKey) {
				t.Fatalf("error handling: %d %s", w.Code, w.Body)
			}
			next := callAPI(s, syntheticClientKey, `{"model":"gpt-fixture","input":"fixture"}`)
			if next.Code != 200 || calls.Load() != 2 {
				t.Fatalf("slot not released: %d %s", next.Code, next.Body)
			}
			raw, _ := json.Marshal(s.Snapshot())
			if strings.Contains(string(raw), syntheticClientKey) || strings.Contains(string(raw), "fixture private") {
				t.Fatal("secret in stats")
			}
			for _, stats := range s.Snapshot() {
				if stats.Inflight != 0 {
					t.Fatal("inflight leaked")
				}
			}
		})
	}
}

type closeTracked struct {
	io.ReadCloser
	closed atomic.Bool
}

func (c *closeTracked) Close() error { c.closed.Store(true); return c.ReadCloser.Close() }

func TestOfficialHTTPAndSSEBodiesCloseAndStreamProducerStops(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "sse"}[stream], func(t *testing.T) {
			s, _ := fixtureService(t)
			var tracked *closeTracked
			producerDone := make(chan struct{})
			s.Official = func(ctx context.Context, _ config.LocalAccount, _ []byte, gotStream, _ bool, _ func() bool) (*http.Response, error) {
				if gotStream != stream {
					t.Error("stream flag changed")
				}
				if !stream {
					tracked = &closeTracked{ReadCloser: io.NopCloser(strings.NewReader(fixtureResponse))}
					close(producerDone)
					return successfulResponse(tracked, false), nil
				}
				reader, writer := io.Pipe()
				tracked = &closeTracked{ReadCloser: reader}
				go func() {
					defer close(producerDone)
					defer writer.Close()
					_, _ = io.WriteString(writer, "event: response.completed\ndata: "+fixtureResponse[:len(fixtureResponse)-1]+`,"type":"response.completed"}`+"\n\n")
					select {
					case <-ctx.Done():
					default:
					}
				}()
				return successfulResponse(tracked, true), nil
			}
			body := `{"model":"gpt-fixture","input":"fixture"}`
			if stream {
				body = `{"model":"gpt-fixture","stream":true,"input":"fixture"}`
			}
			w := callAPI(s, syntheticClientKey, body)
			awaitDone(t, producerDone)
			if w.Code != 200 || tracked == nil || !tracked.closed.Load() {
				t.Fatalf("response body lifecycle: status=%d closed=%v body=%s", w.Code, tracked != nil && tracked.closed.Load(), w.Body)
			}
			if stream && !strings.Contains(w.Body.String(), "response.completed") {
				t.Fatalf("SSE terminal lost: %s", w.Body)
			}
		})
	}
}

func TestLocalAPIRejectsCrossSiteMissingKeyAndUnknownRoute(t *testing.T) {
	s, _ := fixtureService(t)
	var calls atomic.Int32
	s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("must not execute")
	}
	for _, test := range []struct {
		name   string
		mutate func(*http.Request)
		status int
	}{
		{"remote", func(r *http.Request) { r.RemoteAddr = "192.0.2.1:54321" }, 403},
		{"origin", func(r *http.Request) { r.Header.Set("Origin", "https://external.example.test") }, 403},
		{"missing-key", func(r *http.Request) { r.Header.Del("Authorization") }, 401},
		{"query", func(r *http.Request) { r.URL.RawQuery = "account=account-B" }, 404},
		{"route", func(r *http.Request) { r.URL.Path = "/codex-api/v1/not-supported" }, 404},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := apiRequest(context.Background(), syntheticClientKey, strings.NewReader(`{"model":"gpt-fixture"}`))
			test.mutate(r)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != test.status {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("rejected request executed")
	}
}

func TestDelayedOfficialWarmupChecksFinalClientAndAccountAuthorization(t *testing.T) {
	for _, change := range []string{"revoke", "alias", "credential-epoch", "credential-reference"} {
		t.Run(change, func(t *testing.T) {
			s, live := fixtureService(t)
			entered, release := make(chan struct{}), make(chan struct{})
			var upstream atomic.Int32
			s.Official = func(_ context.Context, _ config.LocalAccount, _ []byte, _, _ bool, valid func() bool) (*http.Response, error) {
				close(entered)
				<-release
				if valid == nil || !valid() {
					return nil, errors.New("authorization changed before upstream")
				}
				upstream.Add(1)
				return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
			}
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				s.ServeHTTP(w, apiRequest(context.Background(), syntheticClientKey, strings.NewReader(`{"model":"friendly","input":"fixture"}`)))
			}()
			<-entered
			live.change(func(c *config.Config) {
				switch change {
				case "revoke":
					c.CodexAPI.Clients = c.CodexAPI.Clients[1:]
				case "alias":
					c.CodexAPI.Clients[0].ModelAliases["friendly"] = "gpt-changed"
				case "credential-epoch":
					c.LocalAccounts[0].CredentialEpoch++
				case "credential-reference":
					c.LocalAccounts[0].CredentialRef = "credential-B"
				}
			})
			close(release)
			awaitDone(t, done)
			if upstream.Load() != 0 || w.Code < 400 {
				t.Fatalf("authorization changed during warmup still executed: calls=%d status=%d", upstream.Load(), w.Code)
			}
		})
	}
}
