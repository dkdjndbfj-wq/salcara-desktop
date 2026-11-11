package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/directrelay"
)

func remoteModeFixture(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	s, h := newTestServer(t)
	s.directListenAddr = "127.0.0.1:0"
	s.directHostProvider = func() []string { return []string{"192.168.1.20"} }
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.ConnectRemoteWithBotBinding("https://bot.example.test", "https://bot.example.test/salcara-hub", "saved-group", "synthetic-invite", "synthetic-signature")
		c.ConnectRemote("https://hub.example.test", "https://hub.example.test/salcara-hub")
		c.DirectRelayHosts = []string{"192.168.1.20"}
		c.DirectPhoneID, c.DirectPhoneHash = "saved-direct-phone", directrelay.HashPhoneSecret("synthetic-direct-secret")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.closeDirectRelay(ctx)
	})
	return s, h
}

func TestRemoteModeSwitchPreservesBindingsAndStatusReadsArePassive(t *testing.T) {
	s, h := remoteModeFixture(t)
	before := s.d.Store.Get()
	for _, path := range []string{"/api/state", "/api/local/direct/status"} {
		if w := localRequest(s, h, http.MethodGet, path, nil); w.Code != 200 {
			t.Fatal("state read failed")
		}
	}
	if !reflect.DeepEqual(before, s.d.Store.Get()) || s.directHTTP != nil {
		t.Fatal("viewing direct status activated it")
	}
	for _, mode := range []string{"direct", "bot", "hub", "none", "direct"} {
		w := localRequest(s, h, http.MethodPost, "/api/remote/mode", map[string]any{"mode": mode})
		var reply struct {
			OK   bool   `json:"ok"`
			Mode string `json:"activeMode"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &reply) != nil || !reply.OK || reply.Mode != mode {
			t.Fatalf("switch %s: %d %s", mode, w.Code, w.Body)
		}
		c := s.d.Store.Get()
		if c.EffectiveRemoteMode() != mode || c.DirectRelayEnabled != (mode == "direct") || (s.directHTTP != nil) != (mode == "direct") {
			t.Fatal("saved mode and live listener diverged")
		}
		if !reflect.DeepEqual(c.RemoteConnections, before.RemoteConnections) || c.DirectPhoneHash != before.DirectPhoneHash || c.DirectPhoneID != before.DirectPhoneID {
			t.Fatal("switch erased an inactive binding")
		}
		var status struct {
			Paired  bool `json:"paired"`
			Enabled bool `json:"enabled"`
		}
		w = localRequest(s, h, http.MethodGet, "/api/local/direct/status", nil)
		if json.Unmarshal(w.Body.Bytes(), &status) != nil || !status.Paired || status.Enabled != (mode == "direct") {
			t.Fatal("dormant direct pairing was misreported")
		}
	}
}

func TestRemoteModeLateLoginCannotReplaceNewerSelection(t *testing.T) {
	s, h := remoteModeFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"remaining":100}`))
	}))
	t.Cleanup(provider.Close)
	t.Cleanup(func() { close(release) })
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- localRequest(s, h, http.MethodPost, "/api/login", map[string]any{"relayRoot": provider.URL, "accountKey": "synthetic-key"})
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("login did not reach validation")
	}
	if w := localRequest(s, h, http.MethodPost, "/api/remote/mode", map[string]any{"mode": "bot"}); w.Code != 200 {
		t.Fatal(w.Body)
	}
	before := s.d.Store.Get()
	release <- struct{}{}
	select {
	case w := <-done:
		if w.Code != http.StatusConflict || !reflect.DeepEqual(before, s.d.Store.Get()) {
			t.Fatal("late login replaced the newer Bot selection")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("login did not finish")
	}
}

func TestRemoteModeSaveFailurePreservesOldRuntime(t *testing.T) {
	for _, oldMode := range []string{"hub", "direct"} {
		t.Run(oldMode, func(t *testing.T) {
			s, h := remoteModeFixture(t)
			if oldMode == "direct" {
				if w := localRequest(s, h, http.MethodPost, "/api/remote/mode", map[string]any{"mode": "direct"}); w.Code != 200 {
					t.Fatal(w.Body)
				}
			}
			before, listener := s.d.Store.Get(), s.directHTTP
			if err := os.Remove(s.d.Store.Path()); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(s.d.Store.Path(), 0700); err != nil {
				t.Fatal(err)
			}
			next := "direct"
			if oldMode == "direct" {
				next = "bot"
			}
			w := localRequest(s, h, http.MethodPost, "/api/remote/mode", map[string]any{"mode": next})
			if w.Code < 400 || !reflect.DeepEqual(before, s.d.Store.Get()) || listener != s.directHTTP {
				t.Fatal("failed save interrupted the old effective mode")
			}
		})
	}
}

func TestRemoteModeValidationAndDirectPreparationCannotStopHub(t *testing.T) {
	s, h := remoteModeFixture(t)
	before := s.d.Store.Get()
	for _, request := range []map[string]any{{"mode": "unknown"}, {"mode": "bot", "hubUrl": "https://missing.example.test"}, {"mode": "direct", "hubUrl": "https://hub.example.test"}} {
		w := localRequest(s, h, http.MethodPost, "/api/remote/mode", request)
		if w.Code < 400 || !reflect.DeepEqual(before, s.d.Store.Get()) {
			t.Fatal("invalid switch changed the active mode")
		}
	}
	// No fresh QR can replace a saved pairing or silently activate its mode.
	if w := localRequest(s, h, http.MethodPost, "/api/local/direct/qr", map[string]any{}); w.Code != http.StatusConflict || !reflect.DeepEqual(before, s.d.Store.Get()) {
		t.Fatal("rejected QR switched mode")
	}
	s.directHostProvider = func() []string { return nil }
	if w := localRequest(s, h, http.MethodPost, "/api/remote/mode", map[string]any{"mode": "direct"}); w.Code < 400 || !reflect.DeepEqual(before, s.d.Store.Get()) || s.directHTTP != nil {
		t.Fatal("failed direct bind stopped the Hub")
	}
}

func TestRemoteModeConcurrentSwitchesLeaveExactlyOneRuntime(t *testing.T) {
	s, h := remoteModeFixture(t)
	start, done := make(chan struct{}), make(chan int, 3)
	for _, mode := range []string{"direct", "bot", "hub"} {
		go func() {
			<-start
			done <- localRequest(s, h, http.MethodPost, "/api/remote/mode", map[string]any{"mode": mode}).Code
		}()
	}
	close(start)
	for range 3 {
		if code := <-done; code != 200 {
			t.Fatalf("concurrent switch: %d", code)
		}
	}
	c := s.d.Store.Get()
	if (s.directHTTP != nil) != c.DirectRemoteEnabled() || c.HubRemoteEnabled() == c.DirectRemoteEnabled() {
		t.Fatal("concurrent switches left conflicting transports")
	}
}

func TestRemoteModeSelectsExactSameServerBinding(t *testing.T) {
	s, h := newTestServer(t)
	const root, hub = "https://shared.example.test", "https://shared.example.test/salcara-hub"
	var hubID, botID string
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.ConnectRemote(root, hub)
		hubID = c.DeviceID
		c.ConnectRemoteWithBotBinding(root, hub, "group", "invite", "signature")
		botID = c.DeviceID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := s.d.Store.Get()
	for _, mode := range []string{"hub", "bot", "hub"} {
		w := localRequest(s, h, http.MethodPost, "/api/remote/mode", map[string]any{"mode": mode, "hubUrl": hub})
		if w.Code != http.StatusOK {
			t.Fatal(w.Body)
		}
		wantID := hubID
		if mode == "bot" {
			wantID = botID
		}
		current := s.d.Store.Get()
		if current.DeviceID != wantID || current.EffectiveRemoteMode() != mode || !reflect.DeepEqual(before.RemoteConnections, current.RemoteConnections) {
			t.Fatal("same-server mode selected wrong credentials or replaced a binding")
		}
	}
}

func TestRemoteModeSelectsExactBotGroupAtSameURL(t *testing.T) {
	s, h := newTestServer(t)
	const root, hub = "https://shared.example.test", "https://shared.example.test/salcara-hub"
	ids := map[string]string{}
	if err := s.d.Store.Update(func(c *config.Config) error {
		for _, group := range []string{"group-a", "group-b"} {
			c.ConnectRemoteWithBotBinding(root, hub, group, "invite-"+group, "signature-"+group)
			ids[group] = c.DeviceID
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := s.d.Store.Get()
	if w := localRequest(s, h, "POST", "/api/remote/mode", map[string]any{"mode": "bot", "hubUrl": hub}); w.Code != 409 {
		t.Fatal("ambiguous Bot group was guessed")
	}
	for _, group := range []string{"group-a", "group-b", "group-a"} {
		w := localRequest(s, h, "POST", "/api/remote/mode", map[string]any{"mode": "bot", "hubUrl": hub, "groupId": group})
		cfg := s.d.Store.Get()
		active, _, _ := cfg.BotEnrollment()
		if w.Code != 200 || cfg.DeviceID != ids[group] || active != group || !reflect.DeepEqual(before.RemoteConnections, cfg.RemoteConnections) {
			t.Fatal("Bot activation crossed group identity or rewrote another binding")
		}
	}
}
