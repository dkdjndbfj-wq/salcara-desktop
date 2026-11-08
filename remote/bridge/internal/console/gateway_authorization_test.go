package console

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"salcara/bridge/internal/config"
)

type gatewayDelayedBody struct {
	entered, release chan struct{}
	reader           *strings.Reader
	once             sync.Once
}

func TestProductionWebSocketCannotResumeAfterAPISwitchBack(t *testing.T) {
	s, _ := newTestServer(t)
	s.d.Store.SetRuntimeGatewayKey("synthetic-socket-runtime")
	update := func(index int) {
		t.Helper()
		if err := s.d.Store.Update(func(c *config.Config) error {
			if len(c.LocalAccounts) == 0 {
				for _, id := range []string{"fixture-A", "fixture-B"} {
					c.LocalAccounts = append(c.LocalAccounts, config.LocalAccount{ID: id, Name: id, Kind: "api", SourceType: "api_key", BaseURL: "https://synthetic.invalid", Key: "synthetic-key-" + id, Model: "gpt-fixture"})
				}
			}
			c.RecordAppliedAPI("codex", c.APIForTool(c.LocalAccounts[index], "codex"), "fixture")
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	update(0)
	h := s.Handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { r.Host = "127.0.0.1:47831"; h.ServeHTTP(w, r) }))
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/gateway/codex/v1/responses", http.Header{"Authorization": {"Bearer " + s.d.Store.Get().LocalGatewayCredential("codex")}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	warmup := map[string]any{"type": "response.create", "model": "gpt-fixture", "generate": false, "input": "fixture context"}
	read := func() map[string]any {
		t.Helper()
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var event map[string]any
		if err := conn.ReadJSON(&event); err != nil {
			t.Fatal(err)
		}
		return event
	}
	if err := conn.WriteJSON(warmup); err != nil {
		t.Fatal(err)
	}
	if event := read(); event["type"] != "response.completed" {
		t.Fatalf("socket warmup failed: %v", event)
	}
	update(1)
	update(0)
	if err := conn.WriteJSON(warmup); err != nil {
		t.Fatal(err)
	}
	event := read()
	if event["type"] != "error" || event["error"].(map[string]any)["code"] != "configuration_changed" {
		t.Fatalf("restored API revived the old socket: %v", event)
	}
}

func (b *gatewayDelayedBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered); <-b.release })
	return b.reader.Read(p)
}
func (*gatewayDelayedBody) Close() error { return nil }

func gatewayAwait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("gateway request did not progress")
	}
}

func TestProductionGatewayFencesAuthorizationBeforeSlowBodyAndRestoration(t *testing.T) {
	for _, pooled := range []bool{false, true} {
		for _, change := range []string{"switch", "switch-back", "account-policy-back"} {
			t.Run(change+map[bool]string{true: "/pool", false: "/manual"}[pooled], func(t *testing.T) {
				s, _ := newTestServer(t)
				s.d.Store.SetRuntimeGatewayKey("synthetic-gateway-runtime")
				update := func(fn func(*config.Config)) {
					t.Helper()
					if err := s.d.Store.Update(func(c *config.Config) error { fn(c); return nil }); err != nil {
						t.Fatal(err)
					}
				}
				update(func(c *config.Config) {
					for _, id := range []string{"fixture-A", "fixture-B"} {
						c.LocalAccounts = append(c.LocalAccounts, config.LocalAccount{ID: id, Name: id, Kind: "api", SourceType: "official_account", CredentialRef: id, CredentialEpoch: 1, BaseURL: "https://chatgpt.com/backend-api/codex", Model: "gpt-fixture", PoolEnabled: pooled, PoolGroup: "fixture-pool"})
					}
					c.RecordAppliedAPI("codex", c.APIForTool(c.LocalAccounts[0], "codex"), "fixture")
				})
				var calls atomic.Int32
				s.d.OfficialGateway = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
					calls.Add(1)
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"fixture-response","object":"response","model":"gpt-fixture","output":[]}`))}, nil
				}
				h := s.Handler()
				body := &gatewayDelayedBody{entered: make(chan struct{}), release: make(chan struct{}), reader: strings.NewReader(`{"model":"gpt-fixture","input":"fixture"}`)}
				r := httptest.NewRequest(http.MethodPost, "/gateway/codex/v1/responses", body)
				r.Host, r.RemoteAddr = "127.0.0.1:47831", "127.0.0.1:12345"
				r.Header.Set("Authorization", "Bearer "+s.d.Store.Get().LocalGatewayCredential("codex"))
				r.Header.Set("Content-Type", "application/json")
				w, done := httptest.NewRecorder(), make(chan struct{})
				go func() { defer close(done); h.ServeHTTP(w, r) }()
				gatewayAwait(t, body.entered)
				update(func(c *config.Config) {
					if change == "account-policy-back" {
						c.LocalAccounts[0].ModelDenylist = []string{"gpt-fixture"}
					} else {
						c.RecordAppliedAPI("codex", c.APIForTool(c.LocalAccounts[1], "codex"), "fixture")
					}
				})
				if change != "switch" {
					update(func(c *config.Config) {
						c.LocalAccounts[0].ModelDenylist = nil
						c.RecordAppliedAPI("codex", c.APIForTool(c.LocalAccounts[0], "codex"), "fixture")
					})
				}
				close(body.release)
				gatewayAwait(t, done)
				if calls.Load() != 0 || w.Code != 409 {
					t.Fatalf("slow request crossed authorization change: calls=%d status=%d body=%s", calls.Load(), w.Code, w.Body)
				}
				freshRequest := r.Clone(context.Background())
				freshRequest.Body = io.NopCloser(strings.NewReader(`{"model":"gpt-fixture","input":"new fixture"}`))
				freshRequest.Header.Set("Authorization", "Bearer "+s.d.Store.Get().LocalGatewayCredential("codex"))
				fresh := httptest.NewRecorder()
				h.ServeHTTP(fresh, freshRequest)
				if fresh.Code != 200 || calls.Load() != 1 {
					t.Fatalf("current authorization rejected fresh work: calls=%d status=%d body=%s", calls.Load(), fresh.Code, fresh.Body)
				}
			})
		}
	}
}
