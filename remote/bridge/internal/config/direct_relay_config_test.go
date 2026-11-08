package config

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestDirectRelaySettingsPersistIndependentlyOfHubPairing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	hubPhoneHash := store.Get().PhoneHash
	if err := store.Update(func(c *Config) error {
		c.DirectRelayEnabled = true
		c.DirectRelayHosts = []string{"192.168.1.25", "100.100.2.3"}
		c.DirectPhoneID = "direct-phone"
		c.DirectPhoneHash = "direct-binding-hash"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got := reopened.Get()
	if !got.DirectRelayEnabled || !reflect.DeepEqual(got.DirectRelayHosts, []string{"192.168.1.25", "100.100.2.3"}) || got.DirectPhoneID != "direct-phone" || got.DirectPhoneHash != "direct-binding-hash" {
		t.Fatalf("direct relay configuration did not survive reopen: %+v", got)
	}
	if got.PhoneHash != hubPhoneHash {
		t.Fatalf("direct pairing changed Hub phone credential: before=%q after=%q", hubPhoneHash, got.PhoneHash)
	}
	got.DirectRelayHosts[0] = "changed"
	if reopened.Get().DirectRelayHosts[0] != "192.168.1.25" {
		t.Fatal("Get returned the backing DirectRelayHosts slice")
	}
}

