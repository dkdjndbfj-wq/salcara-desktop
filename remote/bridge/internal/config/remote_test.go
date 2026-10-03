package config

import (
	"path/filepath"
	"testing"
)

func TestRemoteStationsKeepIndependentPersistentIdentitiesAndKeys(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "config.json"))
	_ = s.Update(func(c *Config) error {
		c.LocalAccounts = []LocalAccount{{ID: "api", Kind: "api", Key: "unchanged-model-key"}}
		c.ConnectRemote("https://one.test", "https://one.test/salcara-hub")
		return nil
	})
	one := s.Get()
	_ = s.Update(func(c *Config) error { c.ConnectRemote("https://two.test", "https://two.test/salcara-hub"); return nil })
	two := s.Get()
	if !two.LoggedIn() || two.AccountKey != "" || one.DeviceID == two.DeviceID || one.DeviceSecret == two.DeviceSecret || two.LocalAccounts[0].Key != "unchanged-model-key" {
		t.Fatal("remote station and model identities mixed")
	}
	copy := two.Clone()
	copy.RemoteConnections[0].DeviceSecret = "edited"
	if two.RemoteConnections[0].DeviceSecret == "edited" {
		t.Fatal("clone aliases secrets")
	}
	_ = s.Update(func(c *Config) error { c.ConnectRemote("https://one.test", "https://one.test/salcara-hub"); return nil })
	back := s.Get()
	if back.DeviceID != one.DeviceID || back.DeviceSecret != one.DeviceSecret {
		t.Fatal("returning to same station lost existing pairing")
	}
	reopen, err := Open(s.Path())
	if err != nil || RemoteIdentity(reopen.Get()) != RemoteIdentity(one) || len(reopen.Get().RemoteConnections) != 2 {
		t.Fatal("station identities not persisted")
	}
}
