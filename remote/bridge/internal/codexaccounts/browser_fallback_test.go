package codexaccounts

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func occupiedCallback(t *testing.T) (net.Listener, string) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener, "http://" + listener.Addr().String() + "/auth/callback"
}

func TestBrowserFallbackProductionAndFixtureIsolation(t *testing.T) {
	production, err := NewBrowserConnector(BrowserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if production.redirectURI != "http://localhost:1455/auth/callback" || production.fallbackRedirectURI != "http://localhost:1457/auth/callback" {
		t.Fatal("production callback candidates changed")
	}
	_, occupied := occupiedCallback(t)
	connector, err := NewBrowserConnector(BrowserOptions{FixtureEndpoints: &BrowserFixtureEndpoints{
		AuthURL: "http://127.0.0.1:1/authorize", TokenURL: "http://127.0.0.1:1/token", RedirectURI: occupied,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if connector.fallbackRedirectURI != "" {
		t.Fatal("fixture inherited a real callback port")
	}
	if _, listener, err := connector.listen(context.Background()); err == nil {
		listener.Close()
		t.Fatal("occupied fixture escaped to a production callback port")
	}
	for _, fallback := range []string{"http://0.0.0.0:1/auth/callback", "https://localhost:1/auth/callback", "http://localhost:1/wrong", "http://localhost:1/auth/callback?state=other"} {
		_, err := NewBrowserConnector(BrowserOptions{FixtureEndpoints: &BrowserFixtureEndpoints{
			AuthURL: "http://127.0.0.1:1/authorize", TokenURL: "http://127.0.0.1:1/token", RedirectURI: occupied, FallbackRedirectURI: fallback,
		}})
		if err != ErrLoginFailed {
			t.Fatalf("unsafe fallback accepted: %s", fallback)
		}
	}
}

func TestBrowserFallbackPinsListenerManualCallbackAndExchange(t *testing.T) {
	primary, occupied := occupiedCallback(t)
	exchanges := make(chan url.Values, 2)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" || r.Method != http.MethodPost || r.ParseForm() != nil {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		exchanges <- r.PostForm
		_, _ = io.WriteString(w, `{"access_token":"synthetic-fallback-access","refresh_token":"synthetic-fallback-refresh","expires_in":3600}`)
	}))
	defer provider.Close()
	connector, err := NewBrowserConnector(BrowserOptions{Timeout: 3 * time.Second, FixtureEndpoints: &BrowserFixtureEndpoints{
		AuthURL: provider.URL + "/authorize", TokenURL: provider.URL + "/token", RedirectURI: occupied,
		FallbackRedirectURI: "http://localhost:0/auth/callback", Client: provider.Client(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	service, _, _ := newTestService(t, nil, connector)
	login, err := service.StartLogin(context.Background(), LoginRequest{})
	if err != nil {
		t.Fatal(err)
	}
	pending := waitBrowserProgress(t, service, login.ID)
	auth, _ := url.Parse(pending.AuthorizationURL)
	redirect, err := url.Parse(auth.Query().Get("redirect_uri"))
	if err != nil || redirect.Hostname() != "localhost" || redirect.Port() == "0" || redirect.String() == occupied {
		t.Fatalf("fallback was not published: %q", pending.AuthorizationURL)
	}
	address := net.JoinHostPort("127.0.0.1", redirect.Port())
	if stolen, err := net.Listen("tcp4", address); err == nil {
		stolen.Close()
		t.Fatal("fallback listener was released before callback")
	}
	query := url.Values{"state": {auth.Query().Get("state")}, "code": {"synthetic-fallback-code"}}.Encode()
	if err := service.SubmitLoginCallback(login.ID, occupied+"?"+query); err != ErrLoginFailed {
		t.Fatal("callback from unselected primary accepted", err)
	}
	if err := service.SubmitLoginCallback(login.ID, redirect.String()+"?"+query); err != nil {
		t.Fatal(err)
	}
	finished := waitLogin(t, service, login.ID)
	if finished.Status != "complete" || finished.ErrorCode != "" || finished.AuthorizationURL != "" {
		t.Fatalf("fallback login did not complete: %+v", finished)
	}
	form := <-exchanges
	if form.Get("redirect_uri") != redirect.String() || form.Get("code") != "synthetic-fallback-code" {
		t.Fatal("exchange used a different callback", form)
	}
	if err := service.SubmitLoginCallback(login.ID, redirect.String()+"?"+query); err != ErrConflict {
		t.Fatal("fallback callback was reusable", err)
	}
	if len(exchanges) != 0 {
		t.Fatal("fallback exchanged twice")
	}
	reopened, err := net.Listen("tcp4", address)
	if err != nil {
		t.Fatal("completed flow kept its callback port", err)
	}
	reopened.Close()
	if stolen, err := net.Listen("tcp4", primary.Addr().String()); err == nil {
		stolen.Close()
		t.Fatal("fallback closed another process's primary listener")
	}
}

func TestBrowserFallbackBothPortsUnavailableIsSafeAndActionable(t *testing.T) {
	_, primary := occupiedCallback(t)
	_, fallback := occupiedCallback(t)
	var opens atomic.Int32
	connector, err := NewBrowserConnector(BrowserOptions{OpenBrowser: func(string) error { opens.Add(1); return nil }, FixtureEndpoints: &BrowserFixtureEndpoints{
		AuthURL: "http://127.0.0.1:1/authorize", TokenURL: "http://127.0.0.1:1/token", RedirectURI: primary, FallbackRedirectURI: fallback,
	}})
	if err != nil {
		t.Fatal(err)
	}
	service, _, _ := newTestService(t, nil, connector)
	login, err := service.StartLogin(context.Background(), LoginRequest{})
	if err != nil {
		t.Fatal(err)
	}
	finished := waitLogin(t, service, login.ID)
	if finished.Status != "failed" || finished.ErrorCode != "callback_unavailable" || finished.Error != ErrLoginFailed.Error() || opens.Load() != 0 || len(service.List()) != 0 {
		t.Fatalf("unavailable callback misreported: %+v opens=%d", finished, opens.Load())
	}
	raw, _ := json.Marshal(finished)
	if strings.Contains(string(raw), "127.0.0.1") || strings.Contains(string(raw), "bind") || finished.AuthorizationURL != "" {
		t.Fatal("local bind diagnostics escaped to login status", string(raw))
	}
}

func TestBrowserQueuedLoginExpiresWithoutOpeningBrowser(t *testing.T) {
	var opens atomic.Int32
	connector, err := NewBrowserConnector(BrowserOptions{Timeout: 30 * time.Millisecond, OpenBrowser: func(string) error { opens.Add(1); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate another window retaining the listener. The new session's own
	// timeout must bound queueing, without ever binding real production ports.
	connector.gate <- struct{}{}
	defer func() { <-connector.gate }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := connector.Login(ctx); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("queued login did not expire", err)
		}
	case <-time.After(time.Second):
		cancel()
		<-done
		t.Fatal("queued login ignored its timeout")
	}
	if opens.Load() != 0 {
		t.Fatal("queued login opened a browser")
	}
}

func TestBrowserChallengeUsesParentDeadlineAndCancelledGateStaysClosed(t *testing.T) {
	var opens atomic.Int32
	connector, err := NewBrowserConnector(BrowserOptions{Timeout: time.Minute, OpenBrowser: func(string) error { opens.Add(1); return nil }, FixtureEndpoints: &BrowserFixtureEndpoints{
		AuthURL: "http://127.0.0.1:1/authorize", TokenURL: "http://127.0.0.1:1/token", RedirectURI: "http://127.0.0.1:0/auth/callback",
	}})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	progress := make(chan LoginProgress, 1)
	done := make(chan error, 1)
	go func() {
		_, err := connector.LoginWithProgress(ctx, func(p LoginProgress) { progress <- p })
		done <- err
	}()
	p := <-progress
	if p.ExpiresAt == nil || !p.ExpiresAt.Equal(deadline) {
		t.Errorf("challenge outlives its actual deadline: %v want %v", p.ExpiresAt, deadline)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	before := opens.Load()
	for i := 0; i < 20; i++ {
		if _, err := connector.Login(ctx); !errors.Is(err, context.Canceled) {
			t.Fatal("cancelled context admitted login", err)
		}
	}
	if opens.Load() != before {
		t.Fatal("already-cancelled retry opened a browser")
	}
}

func TestLoginExpiredAndCallbackFailureCodesDoNotExposeRawErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code string
	}{
		{name: "expired", err: context.DeadlineExceeded, code: "expired"},
		{name: "callback", err: errLoginCallbackUnavailable, code: "callback_unavailable"},
		{name: "untrusted provider text", err: errors.New("expired callback_unavailable synthetic-token <script>")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, _, _ := newTestService(t, nil, LoginFunc(func(context.Context) (*coreauth.Auth, error) { return nil, tc.err }))
			login, err := service.StartLogin(context.Background(), LoginRequest{})
			if err != nil {
				t.Fatal(err)
			}
			finished := waitLogin(t, service, login.ID)
			if finished.Status != "failed" || finished.ErrorCode != tc.code || finished.Error != ErrLoginFailed.Error() {
				t.Fatalf("unsafe failure classification: %+v", finished)
			}
		})
	}
}

func TestLoginParentDeadlineRejectsLateCredentialAsExpired(t *testing.T) {
	entered := make(chan struct{})
	service, _, _ := newTestService(t, nil, LoginFunc(func(ctx context.Context) (*coreauth.Auth, error) {
		close(entered)
		<-ctx.Done()
		return sdkFixture("synthetic-expired-access", "synthetic-expired-refresh"), nil
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	login, err := service.StartLogin(ctx, LoginRequest{})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	finished := waitLogin(t, service, login.ID)
	if finished.Status != "failed" || finished.ErrorCode != "expired" || finished.Account != nil || len(service.List()) != 0 {
		t.Fatalf("deadline misreported or late credential saved: %+v", finished)
	}
}
