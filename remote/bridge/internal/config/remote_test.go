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

func TestSavedRemoteConnectionNormalizesPublicHubSuffixOnly(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "config.json"))
	_ = s.Update(func(c *Config) error {
		c.ConnectRemote("https://a.example", "https://a.example/salcara-hub")
		c.ConnectRemote("https://b.example", "https://b.example/salcara-hub")
		return nil
	})
	cfg := s.Get()
	if _, ok := cfg.SavedRemoteConnection("https://b.example/salcara-hub/v1/", cfg.RemoteConnections[1].DeviceID); !ok {
		t.Fatal("/v1 spelling did not resolve the saved station")
	}
	if _, ok := cfg.SavedRemoteConnection("https://b.example/other", cfg.RemoteConnections[1].DeviceID); ok {
		t.Fatal("unrelated path resolved as the saved station")
	}
}

func TestConnectRemoteDoesNotDuplicateV1Spelling(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "config.json"))
	_ = s.Update(func(c *Config) error {
		c.ConnectRemote("https://same.example", "https://same.example/salcara-hub")
		first := c.RemoteConnections[0]
		c.ConnectRemote("https://same.example/", "https://same.example/salcara-hub/v1/")
		if len(c.RemoteConnections) != 1 || c.DeviceID != first.DeviceID || c.DeviceSecret != first.DeviceSecret {
			t.Fatalf("same station spelling created a new identity: %+v", c.RemoteConnections)
		}
		if c.HubURL != "https://same.example/salcara-hub" || c.EffectiveHubURL() != c.HubURL {
			t.Fatalf("non-canonical Hub URL persisted: %q", c.HubURL)
		}
		return nil
	})
}
