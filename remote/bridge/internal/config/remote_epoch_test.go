package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func remoteEpochStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *Config) error {
		c.RelayRoot, c.HubURL = "https://a.fixture.test", "https://a.fixture.test/salcara-hub"
		c.DeviceID, c.DeviceSecret = "fixture-device", "synthetic-device-secret"
		c.RemoteDeviceOnly = true
		c.ActivateRemoteMode("hub")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestRemoteConnectionEpochFreshPrivateAndNotPersisted(t *testing.T) {
	store := remoteEpochStore(t)
	epoch := store.Get().RemoteConnectionEpoch
	if epoch == "" {
		t.Fatal("Open did not create a remote connection epoch")
	}
	for _, marshal := range []func() ([]byte, error){
		func() ([]byte, error) { return json.Marshal(store.Get()) },
		func() ([]byte, error) { return os.ReadFile(store.Path()) },
	} {
		raw, err := marshal()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), epoch) || strings.Contains(strings.ToLower(string(raw)), "remoteconnectionepoch") {
			t.Fatal("process-local connection epoch leaked into JSON or disk")
		}
	}
	reopened, err := Open(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Get().RemoteConnectionEpoch; got == "" || got == epoch {
		t.Fatal("a reopened Store reused an earlier connection epoch")
	}
}

func TestRemoteConnectionEpochPublishedBeforeDelayedABANotification(t *testing.T) {
	store := remoteEpochStore(t)
	initial := store.Get()
	notified, released := make(chan Config, 2), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(released) }) })
	store.OnChange(func(old, next Config) {
		if RemoteIdentity(old) != RemoteIdentity(next) {
			notified <- next
			<-released
		}
	})
	updates := make(chan error, 2)
	previous := initial.RemoteConnectionEpoch
	for _, hub := range []string{"https://b.fixture.test/salcara-hub", initial.HubURL} {
		go func(hub string) {
			updates <- store.Update(func(c *Config) error {
				c.HubURL = hub
				// Even a caller holding the first snapshot cannot restore it.
				c.RemoteConnectionEpoch = initial.RemoteConnectionEpoch
				return nil
			})
		}(hub)
		select {
		case committed := <-notified:
			current := store.Get()
			if current.HubURL != hub || current.RemoteConnectionEpoch != committed.RemoteConnectionEpoch {
				t.Fatal("notification did not refer to the already-published connection")
			}
			if current.RemoteConnectionEpoch == "" || current.RemoteConnectionEpoch == previous || current.RemoteConnectionEpoch == initial.RemoteConnectionEpoch {
				t.Fatal("a committed identity transition retained an earlier epoch")
			}
			previous = current.RemoteConnectionEpoch
		case <-time.After(3 * time.Second):
			t.Fatal("configuration notification did not reach the synthetic barrier")
		}
	}
	if RemoteIdentity(initial) != RemoteIdentity(store.Get()) {
		t.Fatal("fixture did not restore its original remote identity")
	}
	releaseOnce.Do(func() { close(released) })
	for range 2 {
		select {
		case err := <-updates:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("configuration update did not finish")
		}
	}
}

func TestRemoteConnectionEpochTracksEffectiveTransportWithoutIdentityChange(t *testing.T) {
	store := remoteEpochStore(t)
	for _, mode := range []string{"none", "bot", "direct", "hub"} {
		before := store.Get()
		if err := store.Update(func(c *Config) error { c.ActivateRemoteMode(mode); return nil }); err != nil {
			t.Fatal(err)
		}
		after := store.Get()
		if RemoteIdentity(before) != RemoteIdentity(after) {
			t.Fatal("transport-only fixture unexpectedly changed the station identity")
		}
		if after.EffectiveRemoteMode() != mode || after.RemoteConnectionEpoch == before.RemoteConnectionEpoch {
			t.Fatalf("transport change to %q retained an earlier connection epoch", mode)
		}
	}
}

func TestRemoteConnectionEpochPreservesUnchangedConnectionAndRejectsCallerEpoch(t *testing.T) {
	store := remoteEpochStore(t)
	initial := store.Get().RemoteConnectionEpoch
	for _, change := range []struct {
		name string
		fn   func(*Config)
	}{
		{"unchanged", func(*Config) {}},
		{"invented-epoch", func(c *Config) { c.RemoteConnectionEpoch = "synthetic-caller-epoch" }},
		{"cleared-epoch", func(c *Config) { c.RemoteConnectionEpoch = "" }},
		{"display", func(c *Config) { c.DeviceName = "renamed computer" }},
		{"ordinary-api", func(c *Config) {
			c.AccountKey, c.CodexKey, c.ClaudeKey = "synthetic-account", "synthetic-codex", "synthetic-claude"
			c.LocalAccounts = []LocalAccount{{ID: "local-api", Name: "API B", BaseURL: "https://api.fixture.test", Key: "synthetic-model-key"}}
			c.CodexModel, c.ClaudeModel = "codex-fixture", "claude-fixture"
		}},
		{"inactive-station", func(c *Config) {
			c.RemoteConnections = append(c.RemoteConnections, RemoteConnection{HubURL: "https://inactive.fixture.test/salcara-hub", DeviceID: "inactive-device", DeviceSecret: "synthetic-inactive-secret"})
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			if err := store.Update(func(c *Config) error { change.fn(c); return nil }); err != nil {
				t.Fatal(err)
			}
			if got := store.Get().RemoteConnectionEpoch; got != initial {
				t.Fatal("an unrelated change or callback replaced the Store-owned epoch")
			}
		})
	}
}

func TestRemoteConnectionEpochFailedUpdatesDoNotPublish(t *testing.T) {
	store := remoteEpochStore(t)
	before := store.Get()
	var notifications int
	store.OnChange(func(old, next Config) { notifications++ })
	wantErr := errors.New("synthetic callback failure")
	err := store.Update(func(c *Config) error {
		c.HubURL, c.RemoteConnectionEpoch = "https://callback-failure.fixture.test/salcara-hub", "synthetic-callback-epoch"
		return wantErr
	})
	if !errors.Is(err, wantErr) || notifications != 0 || !reflect.DeepEqual(before, store.Get()) {
		t.Fatal("failed callback published a connection or epoch change")
	}
	oldPath := store.path
	store.path = t.TempDir() // Atomic replacement of a directory must fail.
	_, err = store.UpdateWithSnapshot(func(c *Config) error {
		c.HubURL = "https://save-failure.fixture.test/salcara-hub"
		c.ActivateRemoteMode("none")
		return nil
	})
	store.path = oldPath
	if err == nil || notifications != 0 || !reflect.DeepEqual(before, store.Get()) {
		t.Fatal("failed persistence published a connection or epoch change")
	}
}

func TestRemoteConnectionEpochRuntimeGatewayKeyChanges(t *testing.T) {
	store := remoteEpochStore(t)
	before := store.Get().RemoteConnectionEpoch
	for _, key := range []string{"synthetic-runtime-A", "synthetic-runtime-B", ""} {
		store.SetRuntimeGatewayKey(key)
		current := store.Get().RemoteConnectionEpoch
		if current == "" || current == before {
			t.Fatal("a runtime gateway lease change retained the earlier connection epoch")
		}
		store.SetRuntimeGatewayKey(" " + key + " ")
		if store.Get().RemoteConnectionEpoch != current {
			t.Fatal("the same normalized runtime lease invalidated the connection epoch")
		}
		before = current
	}
}

func TestRemoteConnectionEpochUpdateWithSnapshotReturnsCommittedEpoch(t *testing.T) {
	store := remoteEpochStore(t)
	before := store.Get()
	committed, err := store.UpdateWithSnapshot(func(c *Config) error {
		c.DeviceSecret = "synthetic-new-device-secret"
		c.RemoteConnectionEpoch = before.RemoteConnectionEpoch
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if committed.RemoteConnectionEpoch == "" || committed.RemoteConnectionEpoch == before.RemoteConnectionEpoch || committed.RemoteConnectionEpoch != store.Get().RemoteConnectionEpoch {
		t.Fatal("committed snapshot did not carry the new Store-owned epoch")
	}
}
