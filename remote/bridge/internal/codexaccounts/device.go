package codexaccounts

// Independent legacy device-flow integration following the public MIT
// CLIProxyAPI/v7 v7.2.157 sdk/auth/codex_device.go protocol. Production uses the
// same pinned legacy Codex CLI client identity as the browser connector.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type DeviceOptions struct {
	ClientID        string
	Client          *http.Client
	UserCodeURL     string
	PollURL         string
	TokenURL        string
	VerificationURL string
	RedirectURI     string
	Timeout         time.Duration
	Now             func() time.Time
}
type DeviceConnector struct {
	opts   DeviceOptions
	client *http.Client
}

// NewLegacyDeviceConnector registers the pinned CLI device flow without
// accepting endpoint or client-identity overrides from the console.
func NewLegacyDeviceConnector() (*DeviceConnector, error) {
	return NewDeviceConnector(DeviceOptions{ClientID: legacyCodexClientID})
}

func NewDeviceConnector(opts DeviceOptions) (*DeviceConnector, error) {
	if strings.TrimSpace(opts.ClientID) == "" || strings.TrimSpace(opts.ClientID) != opts.ClientID || len(opts.ClientID) > 256 || strings.ContainsAny(opts.ClientID, "\r\n\x00") {
		return nil, ErrLoginFailed
	}
	if opts.UserCodeURL == "" {
		opts.UserCodeURL = "https://auth.openai.com/api/accounts/deviceauth/usercode"
	}
	if opts.PollURL == "" {
		opts.PollURL = "https://auth.openai.com/api/accounts/deviceauth/token"
	}
	if opts.TokenURL == "" {
		opts.TokenURL = "https://auth.openai.com/oauth/token"
	}
	if opts.VerificationURL == "" {
		opts.VerificationURL = "https://auth.openai.com/codex/device"
	}
	if opts.RedirectURI == "" {
		opts.RedirectURI = "https://auth.openai.com/deviceauth/callback"
	}
	for _, endpoint := range []string{opts.UserCodeURL, opts.PollURL, opts.TokenURL, opts.VerificationURL, opts.RedirectURI} {
		if !validDeviceURL(endpoint) {
			return nil, ErrLoginFailed
		}
	}
	if opts.Timeout <= 0 || opts.Timeout > 15*time.Minute {
		opts.Timeout = 15 * time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	client := newOAuthHTTPClient()
	if opts.Client != nil {
		client = *opts.Client
		if client.Timeout <= 0 || client.Timeout > 30*time.Second {
			client.Timeout = 30 * time.Second
		}
	}
	if client.Transport == nil {
		client.Transport = newOAuthHTTPClient().Transport
	}
	client.Jar = nil
	// Device/auth form values must never follow an HTTP redirect to another host.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &DeviceConnector{opts: opts, client: &client}, nil
}

func validDeviceURL(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return u.Scheme == "http" && (strings.EqualFold(u.Hostname(), "localhost") || ip != nil && ip.IsLoopback())
}

func (c *DeviceConnector) Login(ctx context.Context) (*coreauth.Auth, error) {
	return c.LoginWithProgress(ctx, nil)
}
func (c *DeviceConnector) LoginWithProgress(ctx context.Context, notify func(LoginProgress)) (*coreauth.Auth, error) {
	if c == nil {
		return nil, ErrLoginFailed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()
	status, response, err := c.jsonPost(ctx, c.opts.UserCodeURL, map[string]string{"client_id": c.opts.ClientID})
	if err != nil || status < 200 || status >= 300 {
		return nil, loginError(ctx)
	}
	deviceID := deviceValue(response, "device_auth_id")
	code := deviceValue(response, "user_code")
	if code == "" {
		code = deviceValue(response, "usercode")
	}
	if !validDeviceValue(deviceID, 4096) || !validDeviceValue(code, 128) {
		return nil, ErrLoginFailed
	}
	interval := 5 * time.Second
	if value, ok := response["interval"].(json.Number); ok {
		if secs, err := strconv.ParseInt(value.String(), 10, 64); err == nil && secs > 0 && secs <= 60 {
			interval = time.Duration(secs) * time.Second
		}
	}
	if value, ok := response["interval"].(string); ok {
		if secs, err := strconv.ParseInt(value, 10, 64); err == nil && secs > 0 && secs <= 60 {
			interval = time.Duration(secs) * time.Second
		}
	}
	expires := c.opts.Now().Add(c.opts.Timeout).UTC()
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(expires) {
		expires = deadline.UTC()
	}
	if notify != nil {
		notify(LoginProgress{VerificationURL: c.opts.VerificationURL, UserCode: code, ExpiresAt: &expires})
	}
	for {
		status, response, err = c.jsonPost(ctx, c.opts.PollURL, map[string]string{"device_auth_id": deviceID, "user_code": code})
		if err != nil {
			return nil, loginError(ctx)
		}
		if status >= 200 && status < 300 {
			authorizationCode := deviceValue(response, "authorization_code")
			verifier := deviceValue(response, "code_verifier")
			challenge := deviceValue(response, "code_challenge")
			if !validDeviceValue(authorizationCode, 8192) || !validDeviceValue(verifier, 1024) || !validDeviceValue(challenge, 1024) {
				return nil, ErrLoginFailed
			}
			return c.exchange(ctx, url.Values{"grant_type": {"authorization_code"}, "client_id": {c.opts.ClientID}, "code": {authorizationCode}, "redirect_uri": {c.opts.RedirectURI}, "code_verifier": {verifier}})
		}
		if status != http.StatusForbidden && status != http.StatusNotFound {
			return nil, ErrLoginFailed
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func validDeviceValue(value string, limit int) bool {
	return value != "" && len(value) <= limit && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\r\n\x00")
}

func deviceValue(response map[string]any, field string) string {
	value, _ := response[field].(string)
	return value
}

// Refresh is a cancellable transport alternative for explicitly configured
// legacy device clients. Service still owns coalescing and atomic persistence.
func (c *DeviceConnector) Refresh(ctx context.Context, record *coreauth.Auth) (*coreauth.Auth, error) {
	if record == nil || record.Provider != "codex" {
		return nil, ErrRefreshFailed
	}
	refresh, _ := record.Metadata["refresh_token"].(string)
	if refresh == "" {
		return nil, ErrNeedsLogin
	}
	return c.exchange(ctx, url.Values{"grant_type": {"refresh_token"}, "client_id": {c.opts.ClientID}, "refresh_token": {refresh}, "scope": {"openid profile email"}})
}

func (c *DeviceConnector) exchange(ctx context.Context, form url.Values) (*coreauth.Auth, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.opts.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, ErrLoginFailed
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	status, m, err := c.request(req)
	if cancelled := ctx.Err(); cancelled != nil {
		return nil, cancelled
	}
	if err != nil {
		return nil, loginError(ctx)
	}
	if status != http.StatusOK {
		if definitiveRefreshRejection(status, m) {
			return nil, ErrNeedsLogin
		}
		return nil, ErrLoginFailed
	}
	p, err := parseCredential(m)
	if err != nil || p.AccessToken == "" {
		return nil, ErrLoginFailed
	}
	if rawDuration, exists := m["expires_in"]; exists {
		duration, ok := rawDuration.(json.Number)
		if !ok {
			return nil, ErrLoginFailed
		}
		secs, err := strconv.ParseInt(duration.String(), 10, 64)
		if err != nil || secs <= 0 || secs > 365*24*3600 {
			return nil, ErrLoginFailed
		}
		p.ExpiresAt = c.opts.Now().Add(time.Duration(secs) * time.Second).UTC()
	}
	if p.AccountID == "" {
		if auth, ok := displayClaims(p.IDToken)["https://api.openai.com/auth"].(map[string]any); ok {
			p.AccountID = stringValue(auth, "chatgpt_account_id")
		}
	}
	return p.sdk(), nil
}
func (c *DeviceConnector) jsonPost(ctx context.Context, endpoint string, body map[string]string) (int, map[string]any, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, ErrLoginFailed
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, ErrLoginFailed
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return c.request(req)
}
func (c *DeviceConnector) request(req *http.Request) (int, map[string]any, error) {
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, nil, ErrLoginFailed
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxImportBytes+1))
	if err != nil || len(raw) > MaxImportBytes {
		return 0, nil, ErrLoginFailed
	}
	if len(raw) == 0 && resp.StatusCode != http.StatusOK {
		return resp.StatusCode, map[string]any{}, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// A destructive refresh verdict needs an explicitly JSON response,
		// not a gateway/HTML diagnostic that happens to contain JSON text.
		// Headerless or ambiguous errors remain retryable. Successful legacy
		// replies keep their existing parsing compatibility; pending device
		// polls below still use only the HTTP status.
		contentTypes := resp.Header.Values("Content-Type")
		if len(contentTypes) != 1 || len(raw) > maxRefreshErrorBytes {
			return resp.StatusCode, map[string]any{}, nil
		}
		mediaType, _, parseErr := mime.ParseMediaType(contentTypes[0])
		if parseErr != nil || (mediaType != "application/json" && !(strings.HasPrefix(mediaType, "application/") && strings.HasSuffix(mediaType, "+json"))) {
			return resp.StatusCode, map[string]any{}, nil
		}
	}
	value, err := strictJSON(raw)
	if err != nil {
		// Pending device polls may use an empty or non-JSON 403/404 body.
		// Only status drives that branch; never return provider HTML/text.
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return resp.StatusCode, map[string]any{}, nil
		}
		return 0, nil, ErrLoginFailed
	}
	m, ok := value.(map[string]any)
	if !ok {
		return 0, nil, ErrLoginFailed
	}
	return resp.StatusCode, m, nil
}
func loginError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrLoginFailed
}
