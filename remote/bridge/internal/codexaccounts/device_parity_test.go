package codexaccounts

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProductionOAuthUsesPinnedDeviceIdentityAndPrivateTransport(t *testing.T) {
	device, err := NewLegacyDeviceConnector()
	if err != nil {
		t.Fatal(err)
	}
	if device.opts.ClientID != legacyCodexClientID || device.opts.UserCodeURL != "https://auth.openai.com/api/accounts/deviceauth/usercode" || device.opts.PollURL != "https://auth.openai.com/api/accounts/deviceauth/token" || device.opts.TokenURL != legacyCodexTokenURL || device.opts.RedirectURI != "https://auth.openai.com/deviceauth/callback" {
		t.Fatal("production device flow changed pinned identity or endpoints")
	}
	browser, err := NewBrowserConnector(BrowserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, client := range []*http.Client{device.client, &browser.client} {
		transport, ok := client.Transport.(*http.Transport)
		if !ok || transport == http.DefaultTransport || transport.Proxy != nil || client.Jar != nil || client.CheckRedirect == nil || client.Timeout != 30*time.Second {
			t.Fatal("OAuth inherited mutable defaults, proxy, cookies, redirects, or unbounded timeout")
		}
	}
}

func TestDeviceOptionsDoNotReuseCookiesOrUnboundedClient(t *testing.T) {
	jar, _ := cookiejar.New(nil)
	fixture := &http.Client{Jar: jar, Timeout: time.Hour}
	device, err := NewDeviceConnector(DeviceOptions{ClientID: "synthetic-client", Client: fixture})
	if err != nil {
		t.Fatal(err)
	}
	if device.client.Jar != nil || device.client.Timeout != 30*time.Second || device.client.Transport == nil {
		t.Fatal("unsafe fixture client inherited")
	}
	if fixture.Jar != jar || fixture.Timeout != time.Hour || fixture.Transport != nil {
		t.Fatal("caller HTTP client mutated")
	}
}

func deviceHTTPFixture(t *testing.T, exchange string, firstPending bool) (*DeviceConnector, *atomic.Int32) {
	t.Helper()
	var polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/usercode":
			io.WriteString(w, `{"device_auth_id":"synthetic-device","usercode":"SYNTHETIC-CODE","interval":"1"}`)
		case "/poll":
			if polls.Add(1) == 1 && firstPending {
				w.WriteHeader(http.StatusForbidden)
				io.WriteString(w, "authorization_pending")
				return
			}
			io.WriteString(w, `{"authorization_code":"synthetic-code","code_verifier":"synthetic-verifier","code_challenge":"synthetic-challenge"}`)
		case "/exchange":
			io.WriteString(w, exchange)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	device, err := NewDeviceConnector(DeviceOptions{ClientID: "synthetic-client", Client: server.Client(), UserCodeURL: server.URL + "/usercode", PollURL: server.URL + "/poll", TokenURL: server.URL + "/exchange", Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return device, &polls
}

func TestDevicePendingBodyAndIdentityFromProviderToken(t *testing.T) {
	token := browserIDToken(`{"https://api.openai.com/auth":{"chatgpt_account_id":"synthetic-provider-account","chatgpt_plan_type":"pro"}}`)
	device, polls := deviceHTTPFixture(t, `{"access_token":"synthetic-access","id_token":"`+token+`","expires_in":3600}`, true)
	var progress LoginProgress
	record, err := device.LoginWithProgress(context.Background(), func(p LoginProgress) { progress = p })
	if err != nil {
		t.Fatal(err)
	}
	if polls.Load() != 2 || progress.UserCode != "SYNTHETIC-CODE" || record.Metadata["account_id"] != "synthetic-provider-account" {
		t.Fatal("pending retry, alternate code field, or provider identity lost")
	}
}

func TestDeviceExchangeRejectsUnusableSuccessPayload(t *testing.T) {
	for _, response := range []string{
		`{"refresh_token":"synthetic-refresh"}`,
		`{"access_token":"synthetic-access","expires_in":"3600"}`,
		`{"access_token":"synthetic-access","expires_in":null}`,
		`{"access_token":"synthetic-access","expires_in":0}`,
		`{"access_token":"synthetic-access","expires_in":1.5}`,
	} {
		t.Run(response, func(t *testing.T) {
			device, _ := deviceHTTPFixture(t, response, false)
			if _, err := device.Login(context.Background()); err != ErrLoginFailed {
				t.Fatal("invalid success accepted", err)
			}
		})
	}
}

func TestDeviceDoesNotFollowCredentialRedirect(t *testing.T) {
	var escaped atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { escaped.Add(1); io.Copy(io.Discard, r.Body) }))
	defer target.Close()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer provider.Close()
	device, err := NewDeviceConnector(DeviceOptions{ClientID: "synthetic-client", UserCodeURL: provider.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := device.Login(context.Background()); err != ErrLoginFailed {
		t.Fatal("redirect accepted", err)
	}
	if escaped.Load() != 0 {
		t.Fatal("credential flow followed redirect")
	}
}

func TestDeviceRejectsMalformedChallengeWithoutPolling(t *testing.T) {
	for _, response := range []string{
		`{"device_auth_id":"synthetic-device\n","user_code":"SYNTHETIC-CODE"}`,
		`{"device_auth_id":"synthetic-device","user_code":" "}`,
		`{"device_auth_id":"synthetic-device","user_code":"FIRST","user_code":"SECOND"}`,
	} {
		t.Run(strings.ReplaceAll(response, "\\", "-"), func(t *testing.T) {
			var polls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/poll" {
					polls.Add(1)
				}
				io.WriteString(w, response)
			}))
			defer server.Close()
			device, err := NewDeviceConnector(DeviceOptions{ClientID: "synthetic-client", UserCodeURL: server.URL + "/code", PollURL: server.URL + "/poll"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := device.Login(context.Background()); err != ErrLoginFailed {
				t.Fatal("malformed challenge accepted", err)
			}
			if polls.Load() != 0 {
				t.Fatal("malformed challenge polled")
			}
		})
	}
}
