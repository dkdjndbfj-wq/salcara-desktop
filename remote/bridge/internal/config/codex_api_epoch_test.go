package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCodexAPIEpochsAreOwnedBySuccessfulStoreCommits(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *Config) error {
		c.CodexAPI.Clients = []NamedAPIClient{{ID: "fixture-client", Name: "Fixture", AccountID: "fixture-account", KeyDigest: APIClientDigest("synthetic-fixture-key")}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	original := store.Get()
	if original.CodexAPI.AuthEpoch == "" || original.CodexAPI.Clients[0].AuthEpoch == "" {
		t.Fatal("Store did not assign process-local authorization epochs")
	}
	if err := store.Update(func(c *Config) error {
		c.CodexAPI.AuthEpoch = "callback-supplied-epoch"
		c.CodexAPI.Clients[0].AuthEpoch = "callback-supplied-epoch"
		return nil
	}); err != nil || !reflect.DeepEqual(original.CodexAPI, store.Get().CodexAPI) {
		t.Fatal("a callback replaced Store-owned epochs on unchanged settings")
	}
	rejected := errors.New("fixture callback failure")
	if err := store.Update(func(c *Config) error { c.CodexAPI.Clients[0].Disabled = true; return rejected }); !errors.Is(err, rejected) || !reflect.DeepEqual(original.CodexAPI, store.Get().CodexAPI) {
		t.Fatal("rejected callback changed live authorization")
	}
	oldPath := store.path
	store.path = t.TempDir() // An existing directory cannot be an atomic file target.
	if err := store.Update(func(c *Config) error { c.CodexAPI.Clients[0].Disabled = true; return nil }); err == nil || !reflect.DeepEqual(original.CodexAPI, store.Get().CodexAPI) {
		t.Fatal("failed persistence changed live authorization")
	}
	store.path = oldPath
	raw, err := os.ReadFile(store.Path())
	if err != nil || strings.Contains(string(raw), original.CodexAPI.AuthEpoch) || strings.Contains(string(raw), original.CodexAPI.Clients[0].AuthEpoch) || strings.Contains(string(raw), "AuthEpoch") {
		t.Fatal("process-local epochs were persisted")
	}
}
