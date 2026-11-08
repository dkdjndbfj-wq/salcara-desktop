package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func usageRevisionStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *Config) error {
		c.LocalAccounts = []LocalAccount{{ID: "usage-card", Name: "fixture account", Kind: "api", SourceType: "api_key", BaseURL: "https://api.fixture.test", Key: "synthetic-A"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestUsageSourceRevisionPublishedBeforeDelayedABANotification(t *testing.T) {
	store := usageRevisionStore(t)
	initial := store.Get()
	notified, released := make(chan Config, 2), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(released) }) })
	store.OnChange(func(old, next Config) {
		if old.LocalAccounts[0].Key != next.LocalAccounts[0].Key {
			notified <- next
			<-released
		}
	})
	updates := make(chan error, 2)
	previous := initial.UsageSourceRevision
	for _, key := range []string{"synthetic-B", "synthetic-A"} {
		go func(key string) {
			updates <- store.Update(func(c *Config) error { c.LocalAccounts[0].Key = key; return nil })
		}(key)
		select {
		case committed := <-notified:
			current := store.Get()
			if current.LocalAccounts[0].Key != key || committed.UsageSourceRevision != current.UsageSourceRevision {
				t.Fatal("notification did not refer to the already-published commit")
			}
			if current.UsageSourceRevision == "" || current.UsageSourceRevision == previous || current.UsageSourceRevision == initial.UsageSourceRevision {
				t.Fatal("delayed notification exposed a changed source with an old revision")
			}
			previous = current.UsageSourceRevision
		case <-time.After(3 * time.Second):
			t.Fatal("synthetic notification did not reach barrier")
		}
	}
	if initial.UsageSourceFingerprint() != store.Get().UsageSourceFingerprint() {
		t.Fatal("fixture did not restore the original source projection")
	}
	releaseOnce.Do(func() { close(released) })
	for range 2 {
		select {
		case err := <-updates:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("synthetic configuration update did not finish")
		}
	}
}

func TestUsageSourceRevisionTracksQueriesAndDisplayOnly(t *testing.T) {
	store := usageRevisionStore(t)
	before := store.Get()
	if err := store.Update(func(c *Config) error {
		c.DeviceName, c.CodexModel = "unrelated device", "another selected model"
		c.LocalAccounts[0].Models = []string{"new catalog hint"}
		c.LocalAccounts[0].Model = "another selected model"
		c.LocalAccounts[0].ModelDenylist = []string{"model policy"}
		c.LocalAccounts[0].PoolEnabled, c.LocalAccounts[0].PoolGroup = true, "new routing group"
		c.LocalAccounts[0].ProviderName, c.LocalAccounts[0].LastUsedAt = "unrendered provider label", 123
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if before.UsageSourceRevision != store.Get().UsageSourceRevision {
		t.Fatal("execution policy or catalog invalidated dashboard usage")
	}
	for _, change := range []struct {
		name string
		fn   func(*Config)
	}{
		{"official-choice", func(c *Config) {
			c.LocalAccounts = append(c.LocalAccounts, LocalAccount{ID: "official", SourceType: "official_account", CredentialRef: "protected-ref", CredentialEpoch: 1, Key: "protected-marker", BaseURL: "https://chatgpt.com/backend-api/codex", Name: "official metadata"})
		}},
		{"display-name", func(c *Config) { c.LocalAccounts[0].Name = "renamed" }},
		{"display-id", func(c *Config) { c.LocalAccounts[0].ID = "different-card" }},
		{"provider-root", func(c *Config) { c.LocalAccounts[0].BaseURL = "https://different.fixture.test" }},
		{"provider-key", func(c *Config) { c.LocalAccounts[0].Key = "synthetic-B" }},
		{"unsupported-source", func(c *Config) { c.LocalAccounts[0].SourceType = "oauth" }},
		{"relay-login", func(c *Config) { c.RelayRoot, c.AccountKey = "https://relay.fixture.test", "synthetic-relay" }},
		{"relay-key", func(c *Config) { c.AccountKey = "synthetic-relay-B" }},
		{"remote-only", func(c *Config) { c.RemoteDeviceOnly = true }},
	} {
		t.Run(change.name, func(t *testing.T) {
			before := store.Get().UsageSourceRevision
			if err := store.Update(func(c *Config) error { change.fn(c); return nil }); err != nil {
				t.Fatal(err)
			}
			if before == store.Get().UsageSourceRevision {
				t.Fatal("query source or visible source metadata changed without a revision")
			}
		})
	}
}

func TestUsageSourceRevisionPrivateStoreOwnedAndFailedSaveDoesNotPublish(t *testing.T) {
	store := usageRevisionStore(t)
	before := store.Get()
	clone := before.Clone()
	clone.UsageSourceRevision = "synthetic-caller-revision"
	if store.Get().UsageSourceRevision != before.UsageSourceRevision {
		t.Fatal("caller clone changed the live revision")
	}
	if err := store.Update(func(c *Config) error { c.UsageSourceRevision = "synthetic-caller-revision"; return nil }); err != nil {
		t.Fatal(err)
	}
	if store.Get().UsageSourceRevision != before.UsageSourceRevision {
		t.Fatal("callback replaced the Store-owned revision")
	}
	var notifications int
	store.OnChange(func(old, next Config) { notifications++ })
	oldPath := store.path
	store.path = t.TempDir() // Atomic replacement of a directory must fail.
	err := store.Update(func(c *Config) error { c.LocalAccounts[0].Key = "synthetic-failed-key"; return nil })
	store.path = oldPath
	if err == nil || notifications != 0 || !reflect.DeepEqual(before, store.Get()) {
		t.Fatal("failed persistence changed the published source/revision")
	}
	for _, marshal := range []func() ([]byte, error){
		func() ([]byte, error) { return json.Marshal(store.Get()) },
		func() ([]byte, error) { return os.ReadFile(store.Path()) },
		func() ([]byte, error) { return json.Marshal(store.Get().UsageSourceTargets()) },
	} {
		raw, err := marshal()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), before.UsageSourceRevision) || strings.Contains(string(raw), "UsageSourceRevision") || strings.Contains(string(raw), "usageSourceRevision") {
			t.Fatal("process revision leaked into JSON/persistence")
		}
	}
	targetJSON, err := json.Marshal(store.Get().UsageSourceTargets())
	if err != nil || strings.Contains(string(targetJSON), "synthetic-A") || strings.Contains(string(targetJSON), "https://") {
		t.Fatal("execution credential or root leaked through target JSON")
	}
	reopened, err := Open(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Get().UsageSourceRevision == "" || reopened.Get().UsageSourceRevision == before.UsageSourceRevision {
		t.Fatal("process restart reused a private revision")
	}
}

func TestUsageSourceProjectionPreservesLegacyAPIsAndSkipsProtectedSources(t *testing.T) {
	c := Config{LocalAccounts: []LocalAccount{
		{ID: "legacy", Name: " Legacy ", BaseURL: "https://api.fixture.test/v1/", Key: " synthetic-A "},
		{ID: "duplicate", Name: "duplicate", SourceType: "api_key", BaseURL: "https://api.fixture.test", Key: "synthetic-A"},
		{ID: "proxy", Name: "Proxy", SourceType: "reverse_proxy", BaseURL: "https://proxy.fixture.test", Key: "synthetic-proxy"},
		{ID: "official", Name: "Official", SourceType: "official_account", BaseURL: "https://official.fixture.test", Key: "protected-marker"},
		{ID: "oauth", Name: "OAuth", SourceType: "oauth", BaseURL: "https://oauth.fixture.test", Key: "synthetic-oauth"},
		{ID: "protected-ref", Name: "Protected ref", SourceType: "api_key", CredentialRef: " ", BaseURL: "https://protected.fixture.test", Key: "protected-marker"},
		{ID: "invalid-source", Name: "Unknown", SourceType: "API_KEY", BaseURL: "https://unknown.fixture.test", Key: "synthetic-unknown"},
	}}
	targets := c.UsageSourceTargets()
	if len(targets) != 2 || targets[0].ID != "legacy" || targets[0].Name != "Legacy" || targets[0].Root != "https://api.fixture.test" || targets[1].ID != "proxy" {
		t.Fatal("usage projection did not preserve supported normalized/deduplicated sources", targets)
	}
}
