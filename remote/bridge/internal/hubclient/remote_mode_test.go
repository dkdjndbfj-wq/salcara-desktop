package hubclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func TestInactiveRemoteModeBlocksHubRequestsUploadsAndStandby(t *testing.T) {
	var calls atomic.Int32
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503) }))
	defer u.Close()
	c, store := newTestClient(t, u.URL, "synthetic-key", nil)
	old := store.Get()
	if !c.canUpload(old) {
		t.Fatal("legacy Hub fixture not enabled")
	}
	for _, mode := range []string{"direct", "none"} {
		if err := store.Update(func(cfg *config.Config) error { cfg.ActivateRemoteMode(mode); return nil }); err != nil {
			t.Fatal(err)
		}
		if c.canUpload(store.Get()) {
			t.Fatal("inactive Hub can upload")
		}
		if _, err := c.requestFor(context.Background(), old, http.MethodGet, "/bridge/stream", nil, false); err == nil {
			t.Fatal("old Hub snapshot remained usable")
		}
		if err := c.pollStandby(context.Background(), old, config.RemoteConnection{HubURL: u.URL, DeviceID: "other", DeviceSecret: "synthetic"}); err == nil {
			t.Fatal("inactive mode polled standby")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("inactive Hub contacted a server")
	}
}

func TestDirectModeStartupDoesNotRegisterHub(t *testing.T) {
	var calls atomic.Int32
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503) }))
	defer u.Close()
	c, store := newTestClient(t, u.URL, "synthetic-key", nil)
	if err := store.Update(func(cfg *config.Config) error { cfg.ActivateRemoteMode("direct"); return nil }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()
	select {
	case <-time.After(100 * time.Millisecond):
	case <-done:
		t.Fatal("client stopped unexpectedly")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("client did not stop")
	}
	if calls.Load() != 0 || c.Status().State != StateNotLoggedIn {
		t.Fatal("direct startup also started Hub registration")
	}
}

func TestBotModeNeverPollsHubStandbyOrAcceptsOldHubRequest(t *testing.T) {
	c, store := newTestClient(t, "https://hub.example.test", "synthetic-key", nil)
	old := store.Get()
	if err := store.Update(func(cfg *config.Config) error { cfg.ActivateRemoteMode("bot"); return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := c.requestFor(context.Background(), old, http.MethodGet, "/bridge/stream", nil, false); err == nil {
		t.Fatal("Hub snapshot crossed into Bot mode")
	}
	if err := c.pollStandby(context.Background(), old, config.RemoteConnection{HubURL: "https://standby.example.test"}); err == nil {
		t.Fatal("Bot mode polled a Hub")
	}
}

func TestInactiveRemoteModeIgnoresOldPairingSnapshot(t *testing.T) {
	c, store := newTestClient(t, "https://hub.example.test", "synthetic-key", nil)
	if err := store.Update(func(cfg *config.Config) error {
		cfg.PhoneBindingID, cfg.PhoneHash = strings.Repeat("a", 64), strings.Repeat("b", 64)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	station := store.Get()
	wire, _ := json.Marshal(map[string]any{"deviceId": station.DeviceID, "paired": false, "bindingId": station.PhoneBindingID, "phoneHash": station.PhoneHash, "revision": 1})
	if err := store.Update(func(cfg *config.Config) error { cfg.ActivateRemoteMode("direct"); return nil }); err != nil {
		t.Fatal(err)
	}
	before := store.Get()
	c.acceptPairState(station, string(wire))
	if !reflect.DeepEqual(before, store.Get()) {
		t.Fatal("inactive Hub snapshot removed a preserved binding")
	}
}

func TestRemoteModeRestoredOutboxResumesAfterActivation(t *testing.T) {
	requests := make(chan []byte, 2)
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		requests <- payload
		w.WriteHeader(http.StatusOK)
	}))
	defer u.Close()
	c, store := newTestClient(t, u.URL, "synthetic-key", nil)
	c.Push(protocol.Event{Type: "notice", ID: "saved-before-pause", Text: "unsent progress"})
	c.qmu.Lock()
	err := c.persistOutboxLocked()
	c.qmu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(cfg *config.Config) error { cfg.ActivateRemoteMode("direct"); return nil }); err != nil {
		t.Fatal(err)
	}
	restarted := New(Options{Store: store, FlushInterval: 10 * time.Millisecond})
	if restarted.Status().Queued != 1 {
		t.Fatal("restart discarded the paused Hub queue")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); restarted.flushLoop(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-requests:
		t.Fatal("paused Hub uploaded while direct mode was active")
	case <-time.After(50 * time.Millisecond):
	}
	if err := store.Update(func(cfg *config.Config) error { cfg.ActivateRemoteMode("hub"); return nil }); err != nil {
		t.Fatal(err)
	}
	select {
	case payload := <-requests:
		if !strings.Contains(string(payload), "saved-before-pause") {
			t.Fatal("resumed outbox did not retain unsent progress")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("resumed Hub outbox remained stuck in the persisted inactive mode")
	}
}
