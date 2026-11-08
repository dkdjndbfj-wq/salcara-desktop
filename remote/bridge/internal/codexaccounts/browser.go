package codexaccounts

// This file independently implements the legacy Codex CLI authorization-code
// flow documented by the pinned MIT CLIProxyAPI/v7 v7.2.157 dependency. It
// deliberately does not claim Codex Desktop identity or add its hosted wrapper,
// originator, stable-id, or version parameters.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	legacyCodexClientID            = "app_EMoamEEZ73f0CkXaXp7hrann"
	legacyCodexAuthURL             = "https://auth.openai.com/oauth/authorize"
	legacyCodexTokenURL            = "https://auth.openai.com/oauth/token"
	legacyCodexRedirectURI         = "http://localhost:1455/auth/callback"
	legacyCodexFallbackRedirectURI = "http://localhost:1457/auth/callback"
	legacyCodexScope               = "openid email profile offline_access"
	maxOAuthQueryBytes             = 16 * 1024
	maxOAuthCodeBytes              = 8 * 1024
	maxOAuthErrorBytes             = 256
)

// Only this fixed classification leaves the connector. Bind errors can contain
// local addresses and OS diagnostics that do not belong in the browser DTO.
var errLoginCallbackUnavailable = errors.New("Codex login callback listener unavailable")

// BrowserFixtureEndpoints can only target loopback HTTP(S) servers. It exists
// for hermetic protocol tests; production callers omit it and use the pinned
// legacy Codex CLI endpoints above.
type BrowserFixtureEndpoints struct {
	AuthURL             string
	TokenURL            string
	RedirectURI         string
	FallbackRedirectURI string
	Client              *http.Client
}

type BrowserOptions struct {
	OpenBrowser      func(string) error
	Timeout          time.Duration
	FixtureEndpoints *BrowserFixtureEndpoints
}

type BrowserConnector struct {
	authURL             string
	tokenURL            string
	redirectURI         string
	fallbackRedirectURI string
	client              http.Client
	openBrowser         func(string) error
	timeout             time.Duration
	gate                chan struct{}
	activeMu            sync.Mutex
	active              map[string]*browserManualSession
}

type browserCallbackResult struct {
	code        string
	providerErr bool
}

type browserCallback struct {
	expectedHost  string
	expectedState string
	result        chan browserCallbackResult
	consumed      atomic.Bool
}

type browserManualSession struct {
	redirect string
	callback *browserCallback
}

func NewBrowserConnector(opts BrowserOptions) (*BrowserConnector, error) {
	authURL, tokenURL, redirectURI := legacyCodexAuthURL, legacyCodexTokenURL, legacyCodexRedirectURI
	fallbackRedirectURI := legacyCodexFallbackRedirectURI
	client := newOAuthHTTPClient()
	if fixture := opts.FixtureEndpoints; fixture != nil {
		if !validBrowserFixtureEndpoint(fixture.AuthURL) || !validBrowserFixtureEndpoint(fixture.TokenURL) || !validBrowserRedirect(fixture.RedirectURI, true) || fixture.FallbackRedirectURI != "" && !validBrowserRedirect(fixture.FallbackRedirectURI, true) {
			return nil, ErrLoginFailed
		}
		authURL, tokenURL, redirectURI = fixture.AuthURL, fixture.TokenURL, fixture.RedirectURI
		// A fixture never falls back to a production port implicitly.
		fallbackRedirectURI = fixture.FallbackRedirectURI
		if fixture.Client != nil {
			client = *fixture.Client
			if client.Timeout <= 0 || client.Timeout > 30*time.Second {
				client.Timeout = 30 * time.Second
			}
		}
	} else if !validBrowserRedirect(redirectURI, false) {
		return nil, ErrLoginFailed
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.Jar = nil
	if client.Transport == nil {
		client.Transport = newOAuthHTTPClient().Transport
	}
	timeout := opts.Timeout
	if timeout <= 0 || timeout > 15*time.Minute {
		timeout = 10 * time.Minute
	}
	return &BrowserConnector{
		authURL:             authURL,
		tokenURL:            tokenURL,
		redirectURI:         redirectURI,
		fallbackRedirectURI: fallbackRedirectURI,
		client:              client,
		openBrowser:         opts.OpenBrowser,
		timeout:             timeout,
		gate:                make(chan struct{}, 1),
		active:              map[string]*browserManualSession{},
	}, nil
}

func validBrowserFixtureEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.Host == "" || u.Path == "" {
		return false
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	return (u.Scheme == "http" || u.Scheme == "https") && (strings.EqualFold(host, "localhost") || ip != nil && ip.IsLoopback())
}

func validBrowserRedirect(raw string, allowZeroPort bool) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.Path != "/auth/callback" {
		return false
	}
	host := u.Hostname()
	if !strings.EqualFold(host, "localhost") && host != "127.0.0.1" {
		return false
	}
	port, err := strconv.Atoi(u.Port())
	return err == nil && (port > 0 && port <= 65535 || allowZeroPort && port == 0)
}

func (c *BrowserConnector) Login(ctx context.Context) (*coreauth.Auth, error) {
	return c.LoginWithProgress(ctx, nil)
}

func (c *BrowserConnector) LoginWithProgress(ctx context.Context, notify func(LoginProgress)) (*coreauth.Auth, error) {
	return c.loginWithSession(ctx, "", notify)
}

func (c *BrowserConnector) LoginWithSessionProgress(ctx context.Context, sessionID string, notify func(LoginProgress)) (*coreauth.Auth, error) {
	if !validLoginSessionID(sessionID) {
		return nil, ErrLoginFailed
	}
	return c.loginWithSession(ctx, sessionID, notify)
}

func (c *BrowserConnector) loginWithSession(ctx context.Context, sessionID string, notify func(LoginProgress)) (*coreauth.Auth, error) {
	if c == nil {
		return nil, ErrLoginFailed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// Include waiting for a previous listener in the same bounded login lifetime.
	flowCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	select {
	case c.gate <- struct{}{}:
		defer func() { <-c.gate }()
	case <-flowCtx.Done():
		return nil, flowCtx.Err()
	}
	if err := flowCtx.Err(); err != nil {
		return nil, err
	}

	state, err := randomOAuthValue(32)
	if err != nil {
		return nil, ErrLoginFailed
	}
	verifier, err := randomOAuthValue(64)
	if err != nil {
		return nil, ErrLoginFailed
	}
	challengeBytes := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeBytes[:])

	redirect, listener, err := c.listen(flowCtx)
	if err != nil {
		if flowCtx.Err() != nil {
			return nil, flowCtx.Err()
		}
		return nil, errLoginCallbackUnavailable
	}
	expected, _ := url.Parse(redirect)
	callback := &browserCallback{expectedHost: expected.Host, expectedState: state, result: make(chan browserCallbackResult, 1)}
	var manual *browserManualSession
	if sessionID != "" {
		manual = &browserManualSession{redirect: redirect, callback: callback}
		if !c.registerManualSession(sessionID, manual) {
			_ = listener.Close()
			return nil, ErrLoginFailed
		}
		defer c.unregisterManualSession(sessionID, manual)
	}
	server := &http.Server{
		Handler:           callback,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       5 * time.Second,
		MaxHeaderBytes:    8 * 1024,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	server.SetKeepAlivesEnabled(false)
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	defer func() {
		_ = server.Close()
		_ = listener.Close()
		<-serveDone
	}()

	authorizationURL, err := c.authorizationURL(redirect, state, challenge)
	if err != nil {
		return nil, ErrLoginFailed
	}
	if err := flowCtx.Err(); err != nil {
		return nil, err
	}
	expires, _ := flowCtx.Deadline()
	expires = expires.UTC()
	if notify != nil {
		notify(LoginProgress{AuthorizationURL: authorizationURL, ExpiresAt: &expires})
	}
	if c.openBrowser != nil {
		// Failure to open the OS browser is not fatal: the UI retains the same
		// bounded authorization URL so the user can reopen it explicitly.
		_ = c.openBrowser(authorizationURL)
	}

	select {
	case result := <-callback.result:
		// The handler may still be flushing the accepted response after sending
		// its result. Close would interrupt that connection and show a browser
		// network error even though the token exchange succeeds. Drain it first,
		// bounded by both the login deadline and a short shutdown timeout.
		shutdownCtx, shutdownCancel := context.WithTimeout(flowCtx, time.Second)
		_ = server.Shutdown(shutdownCtx)
		shutdownCancel()
		_ = server.Close()
		if result.providerErr {
			return nil, ErrLoginFailed
		}
		return c.exchange(flowCtx, redirect, result.code, verifier)
	case err = <-serveDone:
		// Put the terminal value back for the deferred join.
		serveDone <- err
		if errors.Is(err, http.ErrServerClosed) {
			return nil, loginError(flowCtx)
		}
		return nil, ErrLoginFailed
	case <-flowCtx.Done():
		return nil, flowCtx.Err()
	}
}

func validLoginSessionID(id string) bool {
	return id != "" && len(id) <= 128 && strings.TrimSpace(id) == id && !strings.ContainsAny(id, "\r\n\x00")
}

func (c *BrowserConnector) registerManualSession(id string, session *browserManualSession) bool {
	c.activeMu.Lock()
	defer c.activeMu.Unlock()
	if c.active == nil {
		c.active = map[string]*browserManualSession{}
	}
	if _, exists := c.active[id]; exists {
		return false
	}
	c.active[id] = session
	return true
}

func (c *BrowserConnector) unregisterManualSession(id string, session *browserManualSession) {
	c.activeMu.Lock()
	if c.active[id] == session {
		delete(c.active, id)
	}
	c.activeMu.Unlock()
}

// SubmitCallback parses but never requests callbackURL. The complete URL must
// exactly match the active session's loopback redirect; the shared callback
// consumer then applies the same state, duplicate-field, and one-shot checks as
// the listener.
func (c *BrowserConnector) SubmitCallback(sessionID, callbackURL string) error {
	if c == nil || !validLoginSessionID(sessionID) || callbackURL == "" || len(callbackURL) > maxOAuthQueryBytes+512 || strings.TrimSpace(callbackURL) != callbackURL {
		return ErrLoginFailed
	}
	c.activeMu.Lock()
	defer c.activeMu.Unlock()
	session := c.active[sessionID]
	if session == nil {
		return ErrNotFound
	}
	want, err := url.Parse(session.redirect)
	if err != nil {
		return ErrLoginFailed
	}
	got, err := url.Parse(callbackURL)
	if err != nil || !got.IsAbs() || got.Opaque != "" || got.User != nil || got.Fragment != "" || got.Scheme != want.Scheme || !sameCallbackHost(got.Host, want.Host) || got.Path != want.Path || got.EscapedPath() != want.EscapedPath() || got.RawQuery == "" {
		return ErrLoginFailed
	}
	result, accepted := session.callback.consume(got.RawQuery)
	if !accepted {
		return ErrLoginFailed
	}
	session.callback.result <- result
	return nil
}

func (c *BrowserConnector) listen(ctx context.Context) (string, net.Listener, error) {
	var listenErr error
	for _, raw := range []string{c.redirectURI, c.fallbackRedirectURI} {
		if err := ctx.Err(); err != nil {
			return "", nil, err
		}
		if raw == "" {
			continue
		}
		if !validBrowserRedirect(raw, true) {
			return "", nil, ErrLoginFailed
		}
		redirect, _ := url.Parse(raw)
		listenAddress := net.JoinHostPort("127.0.0.1", redirect.Port())
		listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp4", listenAddress)
		if err != nil {
			listenErr = err
			continue
		}
		// Retain this listener through the whole flow: probing, closing and
		// rebinding the port would let another process take the callback.
		addr, ok := listener.Addr().(*net.TCPAddr)
		if !ok || addr.IP == nil || !addr.IP.IsLoopback() {
			_ = listener.Close()
			return "", nil, ErrLoginFailed
		}
		if err := ctx.Err(); err != nil {
			_ = listener.Close()
			return "", nil, err
		}
		redirect.Host = net.JoinHostPort(redirect.Hostname(), strconv.Itoa(addr.Port))
		return redirect.String(), listener, nil
	}
	if listenErr != nil {
		return "", nil, listenErr
	}
	return "", nil, ErrLoginFailed
}

func (c *BrowserConnector) authorizationURL(redirect, state, challenge string) (string, error) {
	u, err := url.Parse(c.authURL)
	if err != nil {
		return "", err
	}
	u.RawQuery = url.Values{
		"client_id":                  {legacyCodexClientID},
		"response_type":              {"code"},
		"redirect_uri":               {redirect},
		"scope":                      {legacyCodexScope},
		"state":                      {state},
		"code_challenge":             {challenge},
		"code_challenge_method":      {"S256"},
		"prompt":                     {"login"},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
	}.Encode()
	return u.String(), nil
}

func randomOAuthValue(size int) (string, error) {
	if size < 32 || size > 96 {
		return "", ErrLoginFailed
	}
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func (c *BrowserConnector) exchange(ctx context.Context, redirect, code, verifier string) (*coreauth.Auth, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {legacyCodexClientID},
		"code":          {code},
		"redirect_uri":  {redirect},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, ErrLoginFailed
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, loginError(ctx)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxImportBytes+1))
	if cancelled := ctx.Err(); cancelled != nil {
		return nil, cancelled
	}
	if err != nil {
		return nil, loginError(ctx)
	}
	if len(raw) == 0 || len(raw) > MaxImportBytes || resp.StatusCode != http.StatusOK {
		return nil, ErrLoginFailed
	}
	value, err := strictJSON(raw)
	if err != nil {
		return nil, ErrLoginFailed
	}
	m, ok := value.(map[string]any)
	if !ok {
		return nil, ErrLoginFailed
	}
	credential, err := parseCredential(m)
	if err != nil || credential.AccessToken == "" {
		return nil, ErrLoginFailed
	}
	if rawExpiry, exists := m["expires_in"]; exists {
		number, ok := rawExpiry.(json.Number)
		if !ok {
			return nil, ErrLoginFailed
		}
		seconds, err := strconv.ParseInt(number.String(), 10, 64)
		if err != nil || seconds <= 0 || seconds > 365*24*60*60 {
			return nil, ErrLoginFailed
		}
		credential.ExpiresAt = time.Now().Add(time.Duration(seconds) * time.Second).UTC()
	}
	if credential.AccountID == "" {
		claims := displayClaims(credential.IDToken)
		if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
			credential.AccountID = stringValue(auth, "chatgpt_account_id")
		}
	}
	if cancelled := ctx.Err(); cancelled != nil {
		return nil, cancelled
	}
	return credential.sdk(), nil
}

func (c *browserCallback) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !loopbackRemote(r.RemoteAddr) || !sameCallbackHost(r.Host, c.expectedHost) {
		http.Error(w, "invalid callback origin", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path != "/auth/callback" || r.URL.EscapedPath() != "/auth/callback" || r.ContentLength > 0 || len(r.TransferEncoding) != 0 || len(r.URL.RawQuery) == 0 || len(r.URL.RawQuery) > maxOAuthQueryBytes {
		http.Error(w, "invalid callback", http.StatusBadRequest)
		return
	}
	if c.consumed.Load() {
		http.Error(w, "callback already consumed", http.StatusConflict)
		return
	}
	result, accepted := c.consume(r.URL.RawQuery)
	if !accepted {
		http.Error(w, "invalid callback", http.StatusBadRequest)
		return
	}
	if result.providerErr {
		http.Error(w, "authorization was not completed", http.StatusBadRequest)
		c.result <- result
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "Authorization received. You can close this window.")
	c.result <- result
}

func (c *browserCallback) consume(rawQuery string) (browserCallbackResult, bool) {
	if len(rawQuery) == 0 || len(rawQuery) > maxOAuthQueryBytes {
		return browserCallbackResult{}, false
	}
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return browserCallbackResult{}, false
	}
	for key, values := range query {
		if (key != "code" && key != "state" && key != "error" && key != "error_description") || len(values) != 1 {
			return browserCallbackResult{}, false
		}
	}
	state := singleQuery(query, "state", maxOAuthCodeBytes)
	if state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(c.expectedState)) != 1 {
		return browserCallbackResult{}, false
	}
	code := singleQuery(query, "code", maxOAuthCodeBytes)
	providerError := singleQuery(query, "error", maxOAuthErrorBytes)
	if description := query["error_description"]; len(description) > 0 && (providerError == "" || len(description[0]) > maxOAuthErrorBytes || strings.ContainsAny(description[0], "\r\n\x00")) {
		return browserCallbackResult{}, false
	}
	if code == "" == (providerError == "") {
		return browserCallbackResult{}, false
	}
	if !c.consumed.CompareAndSwap(false, true) {
		return browserCallbackResult{}, false
	}
	if providerError != "" {
		return browserCallbackResult{providerErr: true}, true
	}
	return browserCallbackResult{code: code}, true
}

func singleQuery(values url.Values, key string, max int) string {
	items := values[key]
	if len(items) != 1 {
		return ""
	}
	value := items[0]
	if value == "" || strings.TrimSpace(value) != value || len(value) > max || strings.ContainsAny(value, "\r\n\x00") {
		return ""
	}
	return value
}

func loopbackRemote(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	ip := net.ParseIP(host)
	return err == nil && ip != nil && ip.IsLoopback()
}

func sameCallbackHost(got, want string) bool {
	gotHost, gotPort, gotErr := net.SplitHostPort(got)
	wantHost, wantPort, wantErr := net.SplitHostPort(want)
	if gotErr != nil || wantErr != nil || gotPort != wantPort {
		return false
	}
	gotIP, wantIP := net.ParseIP(gotHost), net.ParseIP(wantHost)
	if gotIP != nil || wantIP != nil {
		return gotIP != nil && wantIP != nil && gotIP.Equal(wantIP)
	}
	return strings.EqualFold(gotHost, wantHost)
}
