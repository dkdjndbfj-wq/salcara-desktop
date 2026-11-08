package console

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/localusage"
)

func TestAuditUsageOverviewDelayedNotificationsCannotReviveABARead(t *testing.T) {
	localUsageOnce.Do(func() {})
	localUsage = &localusage.Scanner{CodexHome: t.TempDir(), ClaudeHome: t.TempDir()}
	entered, release := make(chan struct{}), make(chan struct{})
	notified, notificationsReleased := make(chan struct{}, 2), make(chan struct{})
	var releaseOnce, notificationOnce sync.Once
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_, _ = w.Write([]byte(`{"remaining":13}`))
	}))
	t.Cleanup(provider.Close)
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		notificationOnce.Do(func() { close(notificationsReleased) })
	})
	store, err := config.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Update(func(c *config.Config) error {
		c.LocalAccounts = []config.LocalAccount{{ID: "usage-card", Name: "fixture", Kind: "api", BaseURL: provider.URL, Key: "synthetic-A"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var hold atomic.Bool
	// Registered before the console observer: successful commits have already
	// been published while this observer delays delivery of all notifications.
	store.OnChange(func(old, next config.Config) {
		if hold.Load() && old.LocalAccounts[0].Key != next.LocalAccounts[0].Key {
			notified <- struct{}{}
			<-notificationsReleased
		}
	})
	s := New(Deps{Store: store, Version: "test", Port: 47831})
	t.Cleanup(s.CloseUsageLedger)
	h := s.Handler()
	request := func() *httptest.ResponseRecorder {
		return do(h, "GET", "/api/usage/overview?days=7", "127.0.0.1:47831", map[string]string{"Cookie": CookieName + "=" + s.Token()}, "")
	}
	oldDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { oldDone <- request() }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("synthetic usage query did not start")
	}
	hold.Store(true)
	updates := make(chan error, 2)
	for _, key := range []string{"synthetic-B", "synthetic-A"} {
		go func(key string) {
			updates <- store.Update(func(c *config.Config) error { c.LocalAccounts[0].Key = key; return nil })
		}(key)
		select {
		case <-notified:
			if store.Get().LocalAccounts[0].Key != key {
				t.Fatal("fixture commit was not published before notification")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("synthetic notification did not reach barrier")
		}
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case old := <-oldDone:
		if old.Code != http.StatusConflict {
			t.Errorf("delayed notifications revived retired credential read: %d %s", old.Code, old.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("synthetic usage query did not finish")
	}
	notificationOnce.Do(func() { close(notificationsReleased) })
	for range 2 {
		select {
		case err := <-updates:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("synthetic configuration update did not finish")
		}
	}
}

type usageBoundaryTransport func(*http.Request) (*http.Response, error)

func (fn usageBoundaryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func TestAuditUsageOverviewNeverQueriesOfficialOrOAuthCredentials(t *testing.T) {
	localUsageOnce.Do(func() {})
	localUsage = &localusage.Scanner{CodexHome: t.TempDir(), ClaudeHome: t.TempDir()}
	// Catch every HTTP destination, including the validated fixed official
	// endpoint. No real upstream or credential is used by this test.
	var mu sync.Mutex
	var calls []string
	oldTransport := http.DefaultTransport
	http.DefaultTransport = usageBoundaryTransport(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		calls = append(calls, r.URL.Host+"|"+r.Header.Get("Authorization"))
		mu.Unlock()
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"remaining":17}`)), Request: r}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	s, h := newTestServer(t)
	official := config.LocalAccount{ID: "official-fixture", Name: "Official", Kind: "api", SourceType: "official_account", ProviderID: "openai", CredentialRef: "credential-fixture", CredentialEpoch: 1, BaseURL: "https://chatgpt.com/backend-api/codex", Key: "protected-credential-fixture", Wire: "responses"}
	if err := config.ValidateLocalAccount(&official); err != nil {
		t.Fatal("official fixture must match the actual attach contract", err)
	}
	oauth := config.LocalAccount{ID: "oauth-fixture", Name: "OAuth", Kind: "api", SourceType: "oauth", BaseURL: "https://oauth.fixture.test", Key: "synthetic-oauth-key"}
	if err := config.ValidateLocalAccount(&oauth); err != nil {
		t.Fatal(err)
	}
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.RelayRoot, c.AccountKey = "https://relay.fixture.test", "synthetic-relay-key"
		c.LocalAccounts = []config.LocalAccount{
			official, oauth,
			{ID: "ref-fixture", Name: "Protected ref", Kind: "api", SourceType: "api_key", CredentialRef: "synthetic-ref", BaseURL: "https://ref.fixture.test", Key: "protected-ref"},
			{ID: "api-fixture", Name: "API", Kind: "api", SourceType: "api_key", BaseURL: "https://api.fixture.test", Key: "synthetic-api-key"},
			{ID: "proxy-fixture", Name: "Reverse proxy", Kind: "api", SourceType: "reverse_proxy", BaseURL: "https://proxy.fixture.test", Key: "synthetic-proxy-key"},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w := do(h, "GET", "/api/usage/overview?days=7", "127.0.0.1:47831", map[string]string{"Cookie": CookieName + "=" + s.Token()}, "")
	if w.Code != http.StatusOK {
		t.Fatalf("overview failed: %d %s", w.Code, w.Body.String())
	}
	mu.Lock()
	observed := append([]string(nil), calls...)
	mu.Unlock()
	for _, call := range observed {
		if strings.Contains(call, "protected-") || strings.Contains(call, "oauth") || strings.Contains(call, "chatgpt.com") {
			t.Errorf("unsupported/protected credential attempted a dashboard usage request: %s", call)
		}
	}
	var dto struct {
		Sources []usageSource `json:"sources"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &dto); err != nil {
		t.Fatal(err)
	}
	if len(observed) != 3 || len(dto.Sources) != 3 {
		t.Fatalf("expected only relay/API-key/reverse-proxy usage, got calls=%v sources=%+v", observed, dto.Sources)
	}
	for _, src := range dto.Sources {
		if !src.OK || src.Usage["remaining"] != float64(17) {
			t.Fatal("supported synthetic API usage regressed", src)
		}
	}
}
