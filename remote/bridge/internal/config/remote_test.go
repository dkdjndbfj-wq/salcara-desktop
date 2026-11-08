package config

import (
	"path/filepath"
	"reflect"
	"strings"
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

func TestCanonicalRemoteHubBindingsRequireSavedStationIdentity(t *testing.T) {
	c := Config{RemoteConnections: []RemoteConnection{
		{HubURL: "https://one.example/salcara-hub", DeviceID: "device-one", DeviceSecret: "secret-one"},
		{HubURL: "https://two.example/salcara-hub", DeviceID: "device-two", DeviceSecret: "secret-two"},
		{HubURL: "https://incomplete.example/salcara-hub", DeviceID: "device-incomplete"},
	}}

	got, err := CanonicalRemoteHubBindings(c, []string{"https://one.example/salcara-hub/v1/", "https://two.example/salcara-hub"})
	if err != nil || !reflect.DeepEqual(got, []string{"https://one.example/salcara-hub", "https://two.example/salcara-hub"}) {
		t.Fatalf("canonical bindings = %v, %v", got, err)
	}
	for _, urls := range [][]string{
		{"https://attacker.example/salcara-hub"},
		{"https://one.example/salcara-hub", "https://one.example/salcara-hub/v1"},
		{"https://incomplete.example/salcara-hub"},
	} {
		if _, err := CanonicalRemoteHubBindings(c, urls); err == nil {
			t.Errorf("accepted unregistered or duplicate bindings %v", urls)
		}
	}
	tooMany := make([]string, MaxRemoteHubBindings+1)
	for i := range tooMany {
		tooMany[i] = "https://one.example/salcara-hub"
	}
	if _, err := CanonicalRemoteHubBindings(c, tooMany); err == nil {
		t.Fatal("accepted an oversized site list")
	}
}

func TestRemoteHubAccountBindingIsExplicitAndStationScoped(t *testing.T) {
	c := Config{}
	legacy := LocalAccount{}
	if !c.RemoteHubAllowsAccount("https://one.example/salcara-hub", legacy) {
		t.Fatal("legacy account lost its prior remote behavior")
	}
	account := LocalAccount{RemoteHubBindingsSet: true, RemoteHubURLs: []string{"https://one.example/salcara-hub"}}
	if !c.RemoteHubAllowsAccount("https://one.example/salcara-hub/v1/", account) {
		t.Fatal("equivalent saved Hub spelling was rejected")
	}
	if c.RemoteHubAllowsAccount("https://two.example/salcara-hub", account) || c.RemoteHubAllowsAccount("", account) {
		t.Fatal("explicit station restriction allowed an unselected site")
	}
	account.RemoteHubURLs = nil
	if c.RemoteHubAllowsAccount("https://one.example/salcara-hub", account) {
		t.Fatal("empty explicit selection must disable remote use")
	}
}

func TestDeviceEnrollmentClaimIsScopedToStationAndClearedByExactMatch(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "config.json"))
	claimOne := strings.Repeat("a", 64)
	if err := s.Update(func(c *Config) error {
		c.ConnectRemoteWithEnrollmentClaim("https://one.test", "https://one.test/salcara-hub", claimOne)
		c.ConnectRemote("https://two.test", "https://two.test/salcara-hub")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := s.Get().DeviceEnrollmentClaim(); got != "" {
		t.Fatal("station one enrollment claim leaked into station two")
	}
	if err := s.Update(func(c *Config) error {
		c.ConnectRemote("https://one.test", "https://one.test/salcara-hub")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := s.Get().DeviceEnrollmentClaim(); got != claimOne {
		t.Fatal("claim was not retained for retry on its own station")
	}
	if err := s.Update(func(c *Config) error {
		if c.ClearDeviceEnrollmentClaim("https://one.test/salcara-hub/v1", c.DeviceID, strings.Repeat("b", 64)) {
			t.Fatal("a different claim cleared the stored claim")
		}
		if !c.ClearDeviceEnrollmentClaim("https://one.test/salcara-hub", c.DeviceID, claimOne) {
			t.Fatal("matching claim was not cleared")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reopen, err := Open(s.Path())
	if err != nil || reopen.Get().DeviceEnrollmentClaim() != "" {
		t.Fatal("cleared enrollment claim was not persisted")
	}
}
