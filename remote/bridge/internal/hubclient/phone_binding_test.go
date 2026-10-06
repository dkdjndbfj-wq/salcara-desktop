package hubclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func TestPhysicalPhoneBindingAcrossStationsAndRevocation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/pair/start") {
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "ABCD2345", "ticket": strings.Repeat("a", 64), "expires_at": 9999999999999})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))
	defer server.Close()
	store, err := config.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Update(func(cfg *config.Config) error { cfg.ConnectRemote(server.URL, server.URL+"/salcara-hub"); return nil })
	c := New(Options{Store: store, HTTP: server.Client()})
	c.status.State = StateConnected
	pair, err := c.StartPair(context.Background())
	if err != nil || pair.ComputerID == "" {
		t.Fatal(pair, err)
	}
	first := store.Get()
	phone := strings.Repeat("b", 64)
	snapshot := func(cfg config.Config, paired bool, hash string) {
		b, _ := json.Marshal(map[string]any{"deviceId": cfg.DeviceID, "paired": paired, "bindingId": cfg.PhoneBindingID, "phoneHash": hash})
		c.handleSSEFor(context.Background(), "pair.status", string(b), cfg)
	}
	snapshot(first, true, phone)
	bound := store.Get()
	if !phoneMatches(bound, bound.PhoneBindingID, phone) || !c.canUpload(bound) || !c.Status().Pairing.Paired {
		t.Fatal("QR did not bind phone locally")
	}
	_ = store.Update(func(cfg *config.Config) error {
		cfg.ConnectRemote(server.URL+"/two", server.URL+"/two/salcara-hub")
		return nil
	})
	second := store.Get()
	c.Kick()
	c.status.State = StateConnected
	if second.ComputerID != first.ComputerID || second.PhoneHash != phone || second.DeviceSecret == first.DeviceSecret || c.canUpload(second) {
		t.Fatal("physical binding or station isolation lost")
	}
	snapshot(second, false, "")
	if store.Get().PhoneHash != phone {
		t.Fatal("unused station revoked physical phone")
	}
	_, err = c.StartPair(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	snapshot(store.Get(), true, strings.Repeat("c", 64))
	if c.canUpload(store.Get()) || store.Get().PhoneHash != phone {
		t.Fatal("different phone bypassed physical binding")
	}
	snapshot(store.Get(), true, phone)
	if !c.canUpload(store.Get()) {
		t.Fatal("same phone cannot use another station")
	}
	snapshot(store.Get(), false, phone)
	revoked := store.Get()
	if revoked.PhoneHash != "" || revoked.PhoneBindingID == first.PhoneBindingID || c.canUpload(revoked) {
		t.Fatal("phone revoke did not close all station authorization")
	}
	// Replay from the old station and process restart cannot reopen authorization.
	_ = store.Update(func(cfg *config.Config) error { cfg.ConnectRemote(server.URL, server.URL+"/salcara-hub"); return nil })
	snapshot(first, true, phone)
	reopened, err := config.Open(store.Path())
	if err != nil || reopened.Get().PhoneHash != "" || phoneMatches(reopened.Get(), bound.PhoneBindingID, phone) {
		t.Fatal("revoked binding restored by old station or restart")
	}
}

func TestDeviceOnlyHasNoImplicitPhoneAndStatusContainsNoMatchingHashes(t *testing.T) {
	store, _ := config.Open(filepath.Join(t.TempDir(), "config.json"))
	_ = store.Update(func(cfg *config.Config) error {
		cfg.ConnectRemote("https://fixture.example", "https://fixture.example/salcara-hub")
		return nil
	})
	c := New(Options{Store: store})
	if phoneMatches(store.Get(), "", "") || c.canUpload(store.Get()) {
		t.Fatal("missing phone identity authorized")
	}
	c.Push(protocol.Event{Type: "message", Text: "never upload before pairing"})
	if len(c.queue) != 0 {
		t.Fatal("unpaired station received private events")
	}
	c.publishPair(PairStatus{DeviceID: store.Get().DeviceID, Paired: true}, "")
	b, _ := json.Marshal(c.Status())
	if strings.Contains(string(b), "phoneHash") || strings.Contains(string(b), "bindingId") {
		t.Fatal("private matching metadata exposed to console")
	}
}

func TestLocalRevokePersistsBeforeOfflineStationRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	store, _ := config.Open(filepath.Join(t.TempDir(), "config.json"))
	_ = store.Update(func(cfg *config.Config) error {
		cfg.ConnectRemote(server.URL, server.URL+"/salcara-hub")
		cfg.PhoneBindingID, cfg.PhoneHash = strings.Repeat("a", 64), strings.Repeat("b", 64)
		return nil
	})
	c := New(Options{Store: store, HTTP: server.Client()})
	before := store.Get()
	c.publishPair(PairStatus{Paired: true}, eventIdentity(before))
	if err := c.RevokePair(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := store.Get()
	if after.PhoneHash != "" || phoneMatches(after, before.PhoneBindingID, before.PhoneHash) || c.canUpload(after) {
		t.Fatal("offline Hub retained local access")
	}
}

func TestMismatchedUnpairedSnapshotDoesNotReleaseHandoverQueue(t *testing.T) {
	store, _ := config.Open(filepath.Join(t.TempDir(), "config.json"))
	_ = store.Update(func(cfg *config.Config) error {
		cfg.ConnectRemote("https://fixture.example", "https://fixture.example/salcara-hub")
		cfg.ComputerID = "physical-pc"
		cfg.PhoneBindingID, cfg.PhoneHash = strings.Repeat("a", 64), strings.Repeat("b", 64)
		return nil
	})
	c := New(Options{Store: store})
	c.handoverAwaitingAuth.Store(true)
	c.pairRevisionIdentity = config.RemoteIdentity(store.Get())
	c.lastPairRevision = 10
	// This is an unpaired snapshot for a different phone. It must not be
	// treated as proof that the newly selected station revoked our binding.
	stale, _ := json.Marshal(map[string]any{"deviceId": store.Get().DeviceID, "paired": false, "revision": 11,
		"bindingId": strings.Repeat("c", 64), "phoneHash": strings.Repeat("d", 64)})
	c.handleSSEFor(context.Background(), "pair.status", string(stale), store.Get())
	if !c.handoverAwaitingAuth.Load() {
		t.Fatal("mismatched unpaired snapshot released the handover queue")
	}
	// A matching unpaired snapshot is an explicit revoke and must still clear
	// local authorization and release the retained queue safely.
	revoked, _ := json.Marshal(map[string]any{"deviceId": store.Get().DeviceID, "paired": false, "revision": 12,
		"bindingId": strings.Repeat("a", 64), "phoneHash": strings.Repeat("b", 64)})
	c.handleSSEFor(context.Background(), "pair.status", string(revoked), store.Get())
	if c.handoverAwaitingAuth.Load() || store.Get().PhoneHash != "" {
		t.Fatal("matching revoke did not clear local authorization")
	}
}
