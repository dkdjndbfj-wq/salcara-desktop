package codexaccounts

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type browserLoginOutcome struct {
	record *coreauth.Auth
	err    error
}

type progressFixtureConnector struct {
	result *coreauth.Auth
	err    error
}

func (c progressFixtureConnector) Login(ctx context.Context) (*coreauth.Auth, error) {
	return c.LoginWithProgress(ctx, nil)
}

func (c progressFixtureConnector) LoginWithProgress(_ context.Context, notify func(LoginProgress)) (*coreauth.Auth, error) {
	expires := time.Now().Add(time.Minute)
	if notify != nil {
		notify(LoginProgress{AuthorizationURL: "https://auth.openai.com/oauth/authorize?state=synthetic-state", VerificationURL: "https://auth.openai.com/codex/device", UserCode: "SYNTHETIC-CODE", ExpiresAt: &expires})
	}
	return c.result, c.err
}

func browserIDToken(payload string) string {
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".synthetic-signature"
}

func waitBrowserProgress(t *testing.T, service *Service, id string) LoginSession {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		session, err := service.LoginStatus(id)
		if err != nil {
			t.Fatal(err)
		}
		if session.AuthorizationURL != "" || session.Status != "pending" {
			return session
		}
		if time.Now().After(deadline) {
			t.Fatalf("authorization progress was not published: %+v", session)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestBrowserConnectorLoopbackPKCEAndStrictCallback(t *testing.T) {
	var wantChallenge string
	var tokenCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		tokenCalls.Add(1)
		if r.Method != http.MethodPost || r.URL.RawQuery != "" || r.ParseForm() != nil {
			t.Error("invalid token exchange request")
		}
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("client_id") != legacyCodexClientID || r.Form.Get("code") != "synthetic-code" {
			t.Errorf("invalid token exchange form: %#v", r.Form)
		}
		if redirect, err := url.Parse(r.Form.Get("redirect_uri")); err != nil || redirect.Hostname() != "127.0.0.1" || redirect.Path != "/auth/callback" || redirect.Port() == "0" {
			t.Errorf("invalid redirect: %q", r.Form.Get("redirect_uri"))
		}
		verifier := r.Form.Get("code_verifier")
		hash := sha256.Sum256([]byte(verifier))
		if verifier == "" || base64.RawURLEncoding.EncodeToString(hash[:]) != wantChallenge {
			t.Error("PKCE verifier did not match authorization challenge")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"synthetic-browser-access","refresh_token":"synthetic-browser-refresh","id_token":"`+browserIDToken(`{"email":"browser@example.test","https://api.openai.com/auth":{"chatgpt_account_id":"provider-account","chatgpt_plan_type":"plus"}}`)+`","expires_in":3600}`)
	}))
	defer server.Close()

	opened := make(chan string, 1)
	connector, err := NewBrowserConnector(BrowserOptions{
		OpenBrowser: func(raw string) error { opened <- raw; return errors.New("synthetic shell failure") },
		Timeout:     2 * time.Second,
		FixtureEndpoints: &BrowserFixtureEndpoints{
			AuthURL: server.URL + "/authorize", TokenURL: server.URL + "/token", RedirectURI: "http://127.0.0.1:0/auth/callback", Client: server.Client(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	progresses := make(chan LoginProgress, 1)
	done := make(chan browserLoginOutcome, 1)
	go func() {
		record, err := connector.LoginWithProgress(context.Background(), func(progress LoginProgress) { progresses <- progress })
		done <- browserLoginOutcome{record: record, err: err}
	}()
	progress := <-progresses
	authorizationURL := <-opened
	if progress.AuthorizationURL != authorizationURL || progress.VerificationURL != "" || progress.ExpiresAt == nil {
		t.Fatalf("unsafe or incomplete progress: %+v", progress)
	}
	auth, err := url.Parse(authorizationURL)
	if err != nil || auth.Scheme != "http" || auth.Host != strings.TrimPrefix(server.URL, "http://") || auth.Path != "/authorize" {
		t.Fatalf("authorization URL: %q %v", authorizationURL, err)
	}
	query := auth.Query()
	wantChallenge = query.Get("code_challenge")
	redirect := query.Get("redirect_uri")
	state := query.Get("state")
	for key, want := range map[string]string{
		"client_id": legacyCodexClientID, "response_type": "code", "scope": legacyCodexScope, "code_challenge_method": "S256", "prompt": "login", "id_token_add_organizations": "true", "codex_cli_simplified_flow": "true",
	} {
		if query.Get(key) != want || len(query[key]) != 1 {
			t.Fatalf("authorization parameter %s = %#v", key, query[key])
		}
	}
	if state == "" || wantChallenge == "" || query.Get("originator") != "" || query.Get("codex_app_version") != "" {
		t.Fatalf("authorization flow mixed another client identity: %s", auth.RawQuery)
	}

	callbackClient := &http.Client{Timeout: time.Second}
	request, _ := http.NewRequest(http.MethodGet, redirect+"?code=bad&state="+url.QueryEscape(state), nil)
	request.Host = "example.test"
	if response, callErr := callbackClient.Do(request); callErr != nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign Host accepted: %v %v", response, callErr)
	} else {
		response.Body.Close()
	}
	request, _ = http.NewRequest(http.MethodPost, redirect+"?code=bad&state="+url.QueryEscape(state), nil)
	if response, callErr := callbackClient.Do(request); callErr != nil || response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("non-GET accepted: %v %v", response, callErr)
	} else {
		response.Body.Close()
	}
	for _, suffix := range []string{
		"?code=bad&state=" + url.QueryEscape(state) + "&state=" + url.QueryEscape(state),
		"?code=first&code=second&state=" + url.QueryEscape(state),
		"?code=bad&state=" + url.QueryEscape(state) + "&unknown=value",
		"?code=bad&state=wrong-state",
	} {
		response, callErr := callbackClient.Get(redirect + suffix)
		if callErr != nil || response.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid callback accepted (%s): %v %v", suffix, response, callErr)
		}
		response.Body.Close()
	}
	response, err := callbackClient.Get(redirect + "?code=synthetic-code&state=" + url.QueryEscape(state))
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("valid callback rejected: %v %v", response, err)
	}
	response.Body.Close()
	outcome := <-done
	if outcome.err != nil || outcome.record == nil || tokenCalls.Load() != 1 {
		t.Fatalf("login result: %+v calls=%d", outcome, tokenCalls.Load())
	}
	if outcome.record.Provider != "codex" || outcome.record.Metadata["access_token"] != "synthetic-browser-access" || outcome.record.Metadata["refresh_token"] != "synthetic-browser-refresh" || outcome.record.Metadata["account_id"] != "provider-account" || outcome.record.Label != "browser@example.test" {
		t.Fatalf("incorrect protected record: %#v", outcome.record)
	}
	encoded, _ := json.Marshal(progress)
	for _, secret := range []string{"synthetic-code", "synthetic-browser-access", "synthetic-browser-refresh", "provider-account"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("progress exposed credential %q: %s", secret, encoded)
		}
	}
}

func TestBrowserCallbackConsumesOnlyOneValidState(t *testing.T) {
	callback := &browserCallback{expectedHost: "127.0.0.1:1455", expectedState: "expected-state", result: make(chan browserCallbackResult, 1)}
	call := func(target string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:1455"+target, nil)
		request.Host = "127.0.0.1:1455"
		request.RemoteAddr = "127.0.0.1:12345"
		response := httptest.NewRecorder()
		callback.ServeHTTP(response, request)
		return response
	}
	if response := call("/auth/callback?code=wrong&state=other"); response.Code != http.StatusBadRequest || callback.consumed.Load() {
		t.Fatal("wrong state consumed callback")
	}
	if response := call("/auth/callback?error=access_denied&error_description=cancelled&state=expected-state"); response.Code != http.StatusBadRequest || !callback.consumed.Load() {
		t.Fatal("matching provider error did not consume callback")
	}
	if result := <-callback.result; !result.providerErr || result.code != "" {
		t.Fatalf("unexpected provider error result: %+v", result)
	}
	if response := call("/auth/callback?code=second&state=expected-state"); response.Code != http.StatusConflict {
		t.Fatal("second valid callback was not rejected")
	}
}

func TestBrowserConnectorCancellationAndTimeoutCloseListener(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		cancel  bool
		wantErr error
	}{
		{name: "cancel", timeout: 2 * time.Second, cancel: true, wantErr: context.Canceled},
		{name: "timeout", timeout: 40 * time.Millisecond, wantErr: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			connector, err := NewBrowserConnector(BrowserOptions{Timeout: tc.timeout, FixtureEndpoints: &BrowserFixtureEndpoints{AuthURL: server.URL + "/authorize", TokenURL: server.URL + "/token", RedirectURI: "http://127.0.0.1:0/auth/callback", Client: server.Client()}})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			progress := make(chan LoginProgress, 1)
			done := make(chan error, 1)
			go func() {
				_, loginErr := connector.LoginWithProgress(ctx, func(value LoginProgress) { progress <- value })
				done <- loginErr
			}()
			value := <-progress
			if tc.cancel {
				cancel()
			}
			if err := <-done; !errors.Is(err, tc.wantErr) {
				t.Fatalf("login error = %v, want %v", err, tc.wantErr)
			}
			auth, _ := url.Parse(value.AuthorizationURL)
			callback := auth.Query().Get("redirect_uri")
			client := http.Client{Timeout: 100 * time.Millisecond}
			if response, err := client.Get(callback + "?code=late&state=" + url.QueryEscape(auth.Query().Get("state"))); err == nil {
				response.Body.Close()
				t.Fatal("listener remained reachable after cancellation")
			}
		})
	}
}

func TestBrowserServiceCancellationClosesSessionListener(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	opened := make(chan string, 1)
	connector, err := NewBrowserConnector(BrowserOptions{OpenBrowser: func(raw string) error { opened <- raw; return nil }, Timeout: 2 * time.Second, FixtureEndpoints: &BrowserFixtureEndpoints{AuthURL: server.URL + "/authorize", TokenURL: server.URL + "/token", RedirectURI: "http://127.0.0.1:0/auth/callback", Client: server.Client()}})
	if err != nil {
		t.Fatal(err)
	}
	service, _, _ := newTestService(t, nil, connector)
	session, err := service.StartLogin(context.Background(), LoginRequest{})
	if err != nil {
		t.Fatal(err)
	}
	authorizationURL := <-opened
	for deadline := time.Now().Add(time.Second); ; {
		session, err = service.LoginStatus(session.ID)
		if err != nil {
			t.Fatal(err)
		}
		if session.AuthorizationURL == authorizationURL {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("authorization progress was not published: %+v", session)
		}
		time.Sleep(time.Millisecond)
	}
	if err = service.CancelLogin(session.ID); err != nil {
		t.Fatal(err)
	}
	finished := waitLogin(t, service, session.ID)
	if finished.Status != "cancelled" || finished.AuthorizationURL != "" || finished.VerificationURL != "" || finished.UserCode != "" || finished.ExpiresAt != nil || len(service.List()) != 0 {
		t.Fatalf("cancelled browser login persisted an account: %+v", finished)
	}
	auth, _ := url.Parse(authorizationURL)
	client := http.Client{Timeout: 100 * time.Millisecond}
	if response, err := client.Get(auth.Query().Get("redirect_uri") + "?code=late&state=" + url.QueryEscape(auth.Query().Get("state"))); err == nil {
		response.Body.Close()
		t.Fatal("cancelled Service login left callback listener reachable")
	}
}

func TestLoginTerminalStatesClearAuthorizationChallenges(t *testing.T) {
	for _, tc := range []struct {
		name       string
		connector  progressFixtureConnector
		wantStatus string
	}{
		{name: "complete", connector: progressFixtureConnector{result: sdkFixture("synthetic-terminal-access", "synthetic-terminal-refresh")}, wantStatus: "complete"},
		{name: "failed", connector: progressFixtureConnector{err: ErrLoginFailed}, wantStatus: "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, _, _ := newTestService(t, nil, tc.connector)
			session, err := service.StartLogin(context.Background(), LoginRequest{})
			if err != nil {
				t.Fatal(err)
			}
			finished := waitLogin(t, service, session.ID)
			if finished.Status != tc.wantStatus || finished.AuthorizationURL != "" || finished.VerificationURL != "" || finished.UserCode != "" || finished.ExpiresAt != nil {
				t.Fatalf("terminal login retained challenge: %+v", finished)
			}
		})
	}
}

func TestBrowserManualCallbackIsBoundToPendingServiceSession(t *testing.T) {
	var tokenCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		tokenCalls.Add(1)
		if r.ParseForm() != nil || r.Form.Get("code") != "manual-code" {
			t.Error("unexpected manual callback exchange")
		}
		io.WriteString(w, `{"access_token":"synthetic-manual-access","refresh_token":"synthetic-manual-refresh","id_token":"`+browserIDToken(`{"email":"manual@example.test","https://api.openai.com/auth":{"chatgpt_account_id":"manual-provider-account"}}`)+`","expires_in":3600}`)
	}))
	defer server.Close()
	connector, err := NewBrowserConnector(BrowserOptions{Timeout: 2 * time.Second, FixtureEndpoints: &BrowserFixtureEndpoints{AuthURL: server.URL + "/authorize", TokenURL: server.URL + "/token", RedirectURI: "http://127.0.0.1:0/auth/callback", Client: server.Client()}})
	if err != nil {
		t.Fatal(err)
	}
	service, _, _ := newTestService(t, nil, connector)
	login, err := service.StartLogin(context.Background(), LoginRequest{})
	if err != nil {
		t.Fatal(err)
	}
	pending := waitBrowserProgress(t, service, login.ID)
	auth, err := url.Parse(pending.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	redirect, _ := url.Parse(auth.Query().Get("redirect_uri"))
	state := auth.Query().Get("state")
	valid := *redirect
	valid.RawQuery = url.Values{"code": {"manual-code"}, "state": {state}}.Encode()
	if err = service.SubmitLoginCallback("missing-session", valid.String()); err != ErrNotFound {
		t.Fatalf("wrong session error = %v", err)
	}
	wrongState := valid
	wrongState.RawQuery = url.Values{"code": {"manual-code"}, "state": {"wrong-state"}}.Encode()
	wrongPort := valid
	wrongPort.Host = "127.0.0.1:1"
	wrongPath := valid
	wrongPath.Path = "/other"
	wrongUser := valid
	wrongUser.User = url.User("fixture")
	wrongFragment := valid
	wrongFragment.Fragment = "fragment"
	duplicate := valid
	duplicate.RawQuery += "&state=" + url.QueryEscape(state)
	for _, raw := range []string{wrongState.String(), wrongPort.String(), wrongPath.String(), wrongUser.String(), wrongFragment.String(), duplicate.String(), "manual-code"} {
		if err = service.SubmitLoginCallback(login.ID, raw); err != ErrLoginFailed {
			t.Fatalf("unsafe callback %q error = %v", raw, err)
		}
		if current, getErr := service.LoginStatus(login.ID); getErr != nil || current.Status != "pending" || tokenCalls.Load() != 0 {
			t.Fatalf("invalid callback changed session: %+v %v calls=%d", current, getErr, tokenCalls.Load())
		}
	}
	if err = service.SubmitLoginCallback(login.ID, valid.String()); err != nil {
		t.Fatal(err)
	}
	finished := waitLogin(t, service, login.ID)
	if finished.Status != "complete" || finished.AuthorizationURL != "" || finished.ExpiresAt != nil || tokenCalls.Load() != 1 || finished.Account == nil || finished.Account.Name != "manual@example.test" {
		t.Fatalf("manual callback did not complete safely: %+v calls=%d", finished, tokenCalls.Load())
	}
	if err = service.SubmitLoginCallback(login.ID, valid.String()); err != ErrConflict {
		t.Fatalf("consumed callback error = %v", err)
	}

	second, err := service.StartLogin(context.Background(), LoginRequest{})
	if err != nil {
		t.Fatal(err)
	}
	secondPending := waitBrowserProgress(t, service, second.ID)
	if err = service.SubmitLoginCallback(second.ID, valid.String()); err != ErrLoginFailed {
		t.Fatalf("old callback crossed into a new session: %v", err)
	}
	if err = service.CancelLogin(second.ID); err != nil {
		t.Fatal(err)
	}
	secondFinished := waitLogin(t, service, second.ID)
	if secondFinished.Status != "cancelled" || secondFinished.AuthorizationURL != "" {
		t.Fatalf("second session did not cancel cleanly: %+v", secondFinished)
	}
	secondAuth, _ := url.Parse(secondPending.AuthorizationURL)
	secondRedirect, _ := url.Parse(secondAuth.Query().Get("redirect_uri"))
	secondRedirect.RawQuery = url.Values{"code": {"late"}, "state": {secondAuth.Query().Get("state")}}.Encode()
	if err = service.SubmitLoginCallback(second.ID, secondRedirect.String()); err != ErrConflict {
		t.Fatalf("cancelled session accepted callback: %v", err)
	}
}

func TestBrowserConnectorTokenResponseBoundsAndRedirects(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.Handler
	}{
		{name: "redirect", handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/sink", http.StatusFound) })},
		{name: "oversize", handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, strings.Repeat("x", MaxImportBytes+1))
		})},
		{name: "duplicate-json", handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"access_token":"first","access_token":"second"}`)
		})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sink atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/sink" {
					sink.Add(1)
					return
				}
				tc.handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			opened := make(chan string, 1)
			connector, err := NewBrowserConnector(BrowserOptions{OpenBrowser: func(raw string) error { opened <- raw; return nil }, Timeout: time.Second, FixtureEndpoints: &BrowserFixtureEndpoints{AuthURL: server.URL + "/authorize", TokenURL: server.URL + "/token", RedirectURI: "http://127.0.0.1:0/auth/callback", Client: server.Client()}})
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, loginErr := connector.Login(context.Background()); done <- loginErr }()
			auth, _ := url.Parse(<-opened)
			query := auth.Query()
			response, err := http.Get(query.Get("redirect_uri") + "?code=synthetic-code&state=" + url.QueryEscape(query.Get("state")))
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if err = <-done; err != ErrLoginFailed {
				t.Fatalf("unsafe token response error = %v", err)
			}
			if sink.Load() != 0 {
				t.Fatal("token exchange followed a redirect")
			}
		})
	}
}

func TestBrowserFixtureEndpointsMustRemainLoopback(t *testing.T) {
	bad := []*BrowserFixtureEndpoints{
		{AuthURL: "http://example.test/authorize", TokenURL: "http://127.0.0.1/token", RedirectURI: "http://127.0.0.1:0/auth/callback"},
		{AuthURL: "http://127.0.0.1/authorize", TokenURL: "http://example.test/token", RedirectURI: "http://127.0.0.1:0/auth/callback"},
		{AuthURL: "http://127.0.0.1/authorize", TokenURL: "http://127.0.0.1/token", RedirectURI: "http://0.0.0.0:1455/auth/callback"},
		{AuthURL: "http://127.0.0.1/authorize", TokenURL: "http://127.0.0.1/token", RedirectURI: "http://127.0.0.1:1455/other"},
	}
	for _, fixture := range bad {
		if _, err := NewBrowserConnector(BrowserOptions{FixtureEndpoints: fixture}); err != ErrLoginFailed {
			t.Fatalf("unsafe fixture endpoints accepted: %+v", fixture)
		}
	}
}
