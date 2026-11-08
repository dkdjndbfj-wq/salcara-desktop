package config

import (
	"path/filepath"
	"testing"
)

func TestRetireCodexAPIServiceRuntimeFencesNewerStart(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	store.SetRuntimeGatewayKey("fixture-runtime-lease")
	if err := store.Update(func(c *Config) error {
		c.CodexAPI.Enabled = true
		c.CodexAPI.RuntimeLease = c.GatewayRuntimeKey
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	captured := store.Get().CodexAPI.AuthEpoch
	retired, ok := store.RetireCodexAPIServiceRuntime(captured)
	if !ok || retired == "" || retired == captured {
		t.Fatal("service runtime was not retired with a new authorization epoch")
	}
	if current := store.Get(); current.CodexAPIActive() || current.CodexAPI.Enabled || current.CodexAPI.RuntimeLease != "" {
		t.Fatal("retired service remained active")
	}
	if err := store.Update(func(c *Config) error {
		c.CodexAPI.Enabled = true
		c.CodexAPI.RuntimeLease = c.GatewayRuntimeKey
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.RetireCodexAPIServiceRuntime(retired); ok {
		t.Fatal("stale retirement stopped a newer service start")
	}
	if !store.Get().CodexAPIActive() {
		t.Fatal("newer service start was not retained")
	}
}

func TestRetireCodexAPIClientRuntimeFencesNewerRotation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	account := LocalAccount{ID: "fixture-account", Name: "Fixture", Kind: "api", BaseURL: "https://provider.example", Key: "synthetic-upstream", Model: "gpt-fixture", AuthMode: "bearer"}
	if err := store.Update(func(c *Config) error {
		c.LocalAccounts = append(c.LocalAccounts, account)
		c.CodexAPI.Clients = []NamedAPIClient{{ID: "fixture-client", Name: "Fixture", AccountID: account.ID, KeyDigest: APIClientDigest("synthetic-client-old")}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	captured, _ := store.Get().APIClient("fixture-client")
	retired, ok := store.RetireCodexAPIClientRuntime(captured)
	if !ok || !retired.Disabled || retired.AuthEpoch == "" || retired.AuthEpoch == captured.AuthEpoch {
		t.Fatal("client runtime was not retired with a new authorization epoch")
	}
	newDigest := APIClientDigest("synthetic-client-new")
	if err := store.Update(func(c *Config) error {
		for i := range c.CodexAPI.Clients {
			if c.CodexAPI.Clients[i].ID == captured.ID {
				c.CodexAPI.Clients[i].Disabled = false
				c.CodexAPI.Clients[i].KeyDigest = newDigest
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.RetireCodexAPIClientRuntime(retired); ok {
		t.Fatal("stale retirement disabled a newer client rotation")
	}
	current, ok := store.Get().APIClient(captured.ID)
	if !ok || current.Disabled || current.KeyDigest != newDigest {
		t.Fatal("newer client rotation was not retained")
	}
}
