package codexaccounts

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestStructuredDeviceLoginAndRefreshWithHTTPFixture(t *testing.T) {
	var polls atomic.Int32
	var exchanges atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/usercode":
			var body map[string]string
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["client_id"] != "synthetic-authorized-client" {
				t.Error("client ID not supplied")
			}
			io.WriteString(w, `{"device_auth_id":"synthetic-device-id","user_code":"TEST-CODE","interval":1}`)
		case "/poll":
			polls.Add(1)
			var body map[string]string
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["device_auth_id"] != "synthetic-device-id" || body["user_code"] != "TEST-CODE" {
				t.Error("bad polling fixture")
			}
			io.WriteString(w, `{"authorization_code":"synthetic-code","code_verifier":"synthetic-verifier","code_challenge":"synthetic-challenge"}`)
		case "/exchange":
			exchanges.Add(1)
			if r.ParseForm() != nil || r.Form.Get("client_id") != "synthetic-authorized-client" {
				t.Error("missing form client ID")
			}
			if r.Form.Get("grant_type") == "authorization_code" && (r.Form.Get("code") != "synthetic-code" || r.Form.Get("code_verifier") != "synthetic-verifier") {
				t.Error("missing PKCE fixture")
			}
			io.WriteString(w, `{"access_token":"synthetic-device-access","refresh_token":"synthetic-device-refresh","expires_in":3600,"account_id":"explicit-server-account"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	c, err := NewDeviceConnector(DeviceOptions{ClientID: "synthetic-authorized-client", Client: server.Client(), UserCodeURL: server.URL + "/usercode", PollURL: server.URL + "/poll", TokenURL: server.URL + "/exchange", VerificationURL: server.URL + "/verify", RedirectURI: server.URL + "/callback"})
	if err != nil {
		t.Fatal(err)
	}
	var progress LoginProgress
	record, err := c.LoginWithProgress(context.Background(), func(p LoginProgress) { progress = p })
	if err != nil {
		t.Fatal(err)
	}
	if progress.UserCode != "TEST-CODE" || progress.VerificationURL != server.URL+"/verify" || progress.ExpiresAt == nil || polls.Load() != 1 {
		t.Fatalf("challenge: %+v", progress)
	}
	if record.Metadata["access_token"] != "synthetic-device-access" || record.Metadata["account_id"] != "explicit-server-account" {
		t.Fatal("wrong credentials")
	}
	if _, err = c.Refresh(context.Background(), record); err != nil || exchanges.Load() != 2 {
		t.Fatal("device client refresh", err)
	}
	raw, _ := json.Marshal(progress)
	if strings.Contains(string(raw), "synthetic-device") {
		t.Fatal("progress leaked device credential")
	}
}

func TestDeviceCancellationAndSafeSessionProgress(t *testing.T) {
	pollEntered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/usercode" {
			io.WriteString(w, `{"device_auth_id":"synthetic-device-id","user_code":"TEST-CODE","interval":1}`)
			return
		}
		close(pollEntered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	c, err := NewDeviceConnector(DeviceOptions{ClientID: "synthetic-client", Client: server.Client(), UserCodeURL: server.URL + "/usercode", PollURL: server.URL + "/poll"})
	if err != nil {
		t.Fatal(err)
	}
	s, _, _ := newTestService(t, nil, c)
	l, err := s.StartLogin(context.Background(), LoginRequest{})
	if err != nil {
		t.Fatal(err)
	}
	<-pollEntered
	l, err = s.LoginStatus(l.ID)
	if err != nil || l.UserCode != "TEST-CODE" || l.VerificationURL != "https://auth.openai.com/codex/device" {
		t.Fatalf("progress absent: %+v %v", l, err)
	}
	if err = s.CancelLogin(l.ID); err != nil {
		t.Fatal(err)
	}
	finished := waitLogin(t, s, l.ID)
	if finished.Status != "cancelled" || len(s.List()) != 0 {
		t.Fatal("cancelled device login saved credential")
	}
}

func TestDeviceConnectorRequiresConfiguredClientAndRejectsUnsafeURLs(t *testing.T) {
	if _, err := NewDeviceConnector(DeviceOptions{}); err != ErrLoginFailed {
		t.Fatal("client ID defaulted")
	}
	if _, err := NewDeviceConnector(DeviceOptions{ClientID: "synthetic-client", TokenURL: "http://external.example.test/token"}); err != ErrLoginFailed {
		t.Fatal("plaintext external token endpoint")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"device_auth_id":"first","device_auth_id":"second","user_code":"TEST-CODE"}`)
	}))
	defer server.Close()
	c, err := NewDeviceConnector(DeviceOptions{ClientID: "synthetic-client", Client: server.Client(), UserCodeURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Login(context.Background()); err != ErrLoginFailed {
		t.Fatal("duplicate OAuth JSON accepted")
	}
}

func TestPlatformProtectionSyntheticRoundTrip(t *testing.T) {
	p := platformProtector()
	synthetic := []byte("synthetic-platform-protection-fixture")
	protected, err := p.Protect(synthetic)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := p.Unprotect(protected)
	if err != nil || string(plain) != string(synthetic) {
		t.Fatal("platform protector round trip", err)
	}
}
