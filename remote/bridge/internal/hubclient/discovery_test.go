package hubclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func TestDiscoveryDoesNotSendCredentialsAndChecksCapabilities(t *testing.T) {
	mode := "ok"
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Salcara-Device-Secret") != "" || r.Header.Get("Cookie") != "" {
			t.Error("discovery leaked credentials")
		}
		if r.URL.Path != "/salcara-hub/v1/ping" {
			t.Error("wrong discovery endpoint")
		}
		switch mode {
		case "redirect":
			w.Header().Set("Location", "https://different.test/v1/ping")
			w.WriteHeader(302)
		case "legacy":
			_, _ = w.Write([]byte(`{"service":"salcara-hub","version":"1.1.0"}`))
		case "missing":
			w.WriteHeader(404)
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"service": "salcara-hub", "protocol": "salcara-remote", "protocolVersion": 1, "version": "1.3.0", "authModes": []string{"device-pairing"}, "capabilities": []string{"device.identity.v1", "pair.qr.v1", "session.remote.v1", "pair.revoke.v1"}})
		}
	}))
	defer u.Close()
	info, err := Discover(context.Background(), u.URL+"/v1", "")
	if err != nil || info.HubURL != u.URL+"/salcara-hub" {
		t.Fatalf("%+v %v", info, err)
	}
	for _, m := range []string{"redirect", "legacy", "missing"} {
		mode = m
		if _, err := Discover(context.Background(), u.URL, ""); err == nil {
			t.Fatalf("accepted %s", mode)
		}
	}
	if _, err := Discover(context.Background(), u.URL, "https://different.test/salcara-hub"); err == nil {
		t.Fatal("accepted cross-origin endpoint")
	}
	for _, url := range []string{"http://relay.test", "https://user:pass@relay.test", "https://relay.test?key=secret", "https://relay.test#secret"} {
		if _, err := RemoteURL(url); err == nil {
			t.Fatalf("unsafe remote address %s", url)
		}
	}
}

func TestDiscoverForDeviceScopesCredentialsAndNeverFollowsRedirects(t *testing.T) {
	for _, tc := range []struct {
		name, id, secret, claim string
	}{
		{"selected device", "selected-device", "selected-secret", ""},
		{"explicit invitation", "", "", "selected-claim"},
		{"invitation takes precedence", "old-device", "old-secret", "selected-claim"},
		{"incomplete identity stays public", "selected-device", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wantID, wantSecret := tc.id, tc.secret
				if tc.claim != "" || tc.secret == "" {
					wantID, wantSecret = "", ""
				}
				if r.Header.Get("X-Salcara-Device-Id") != wantID || r.Header.Get("X-Salcara-Device-Secret") != wantSecret || r.Header.Get("X-Salcara-Enrollment-Claim") != tc.claim || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
					t.Error("discovery mixed device, invitation or upstream credentials")
				}
				_, _ = w.Write([]byte(`{"service":"salcara-hub","protocol":"salcara-remote","protocolVersion":1,"authModes":["device-pairing"],"capabilities":["device.identity.v1","pair.qr.v1","session.remote.v1","pair.revoke.v1"]}`))
			}))
			defer u.Close()
			if _, err := DiscoverForDevice(context.Background(), u.URL, "", tc.id, tc.secret, tc.claim); err != nil {
				t.Fatal(err)
			}
		})
	}
	var targetRequests, originRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetRequests.Add(1) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originRequests.Add(1)
		http.Redirect(w, r, target.URL+"/salcara-hub/v1/ping", http.StatusFound)
	}))
	defer origin.Close()
	for _, override := range []string{"", target.URL + "/salcara-hub"} {
		if _, err := DiscoverForDevice(context.Background(), origin.URL, override, "selected-device", "selected-secret", ""); err == nil {
			t.Fatal("accepted a credential redirect or cross-origin override")
		}
	}
	if targetRequests.Load() != 0 || originRequests.Load() != 1 {
		t.Fatal("discovery credentials left the selected origin")
	}
}

func TestDeviceOnlyRequestsNeverUseModelKeyAndQueueDoesNotCrossStations(t *testing.T) {
	st, _ := config.Open(filepath.Join(t.TempDir(), "config.json"))
	_ = st.Update(func(c *config.Config) error {
		c.AccountKey = "private-legacy-model-key"
		c.ConnectRemote("https://one.test", "https://one.test/salcara-hub")
		c.PhoneBindingID, c.PhoneHash = strings.Repeat("b", 64), strings.Repeat("a", 64)
		return nil
	})
	c := New(Options{Store: st})
	old := st.Get()
	c.publishPair(PairStatus{DeviceID: old.DeviceID, Paired: true}, eventIdentity(old))
	r, err := c.newRequest(context.Background(), "GET", "/bridge/stream", nil)
	if err != nil || r.Header.Get("Authorization") != "" || r.Header.Get("X-Salcara-Device-Secret") != old.DeviceSecret || r.Header.Get("X-Salcara-Device-Id") != old.DeviceID {
		t.Fatal("device identity mixed with model key")
	}
	c.Push(protocol.Event{Type: "notice", Text: "old-station-only"})
	_ = st.Update(func(c *config.Config) error {
		c.ConnectRemote("https://two.test", "https://two.test/salcara-hub")
		return nil
	})
	c.publishPair(PairStatus{DeviceID: st.Get().DeviceID, Paired: true}, eventIdentity(st.Get()))
	c.Push(protocol.Event{Type: "notice", Text: "new-station-only"})
	if len(c.queue) != 1 || c.queue[0].Text != "new-station-only" {
		t.Fatal("old station events leaked into new station queue")
	}
	r, err = c.requestFor(context.Background(), old, "POST", "/bridge/reply", strings.NewReader(`{}`), true)
	if err != nil || r.URL.Host != "one.test" || r.Header.Get("X-Salcara-Device-Id") != old.DeviceID {
		t.Fatal("an old command reply moved to the new station")
	}
}
