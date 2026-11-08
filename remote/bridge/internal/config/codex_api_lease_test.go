package config

import (
	"path/filepath"
	"testing"
)

func TestCodexAPIActiveRequiresCurrentProcessLease(t *testing.T) {
	c := Config{GatewayRuntimeKey: "lease-one", CodexAPI: CodexAPIService{Enabled: true}}
	if c.CodexAPIActive() {
		t.Fatal("persisted enablement became active without an armed lease")
	}
	c.CodexAPI.RuntimeLease = "lease-one"
	if !c.CodexAPIActive() {
		t.Fatal("current process lease did not activate API service")
	}
	c.GatewayRuntimeKey = "lease-two"
	if c.CodexAPIActive() {
		t.Fatal("old process lease remained active after rotation")
	}
	c.CodexAPI.Enabled = false
	c.CodexAPI.RuntimeLease = "lease-two"
	if c.CodexAPIActive() {
		t.Fatal("disabled API service reported active")
	}
}

func TestSetRuntimeGatewayKeyClearsCodexAPILease(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.cfg.CodexAPI.Enabled = true
	s.cfg.CodexAPI.RuntimeLease = "old-lease"
	s.mu.Unlock()
	s.SetRuntimeGatewayKey("new-lease")
	c := s.Get()
	if c.GatewayRuntimeKey != "new-lease" || c.CodexAPI.RuntimeLease != "" || !c.CodexAPI.Enabled {
		t.Fatalf("runtime lease rotation did not fence API service: %+v", c)
	}
}
