package config

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestReenrollmentRetainsLocalSourcesAndOtherComputerCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(c *Config) error {
		c.LocalAccounts = []LocalAccount{{ID: NewUUID(), Name: "A API", Kind: "api", BaseURL: "https://a.fixture/v1", Key: "synthetic-model-key"}}
		c.ConnectRemote("https://b.fixture", "https://b.fixture/salcara-hub")
		c.ConnectRemote("https://a.fixture", "https://a.fixture/tenant/salcara-hub")
		c.RemoteConnections[1].HubInstanceID = "synthetic-old-hub-id"
		c.RemoteConnections[1].IdentityPublicKey = "synthetic-old-pin"
		c.RemoteConnections[1].IngressURL = "https://edge.fixture/v1"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := s.Get()
	if err := s.Update(func(c *Config) error {
		c.ReenrollRemote("https://a.fixture", "https://a.fixture/tenant/salcara-hub", "synthetic-new-claim")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	after := s.Get()
	if !reflect.DeepEqual(before.LocalAccounts, after.LocalAccounts) || !reflect.DeepEqual(before.Projects, after.Projects) || !reflect.DeepEqual(before.RemoteConnections[0], after.RemoteConnections[0]) {
		t.Fatal("reenrollment changed local data or another station")
	}
	if before.DeviceID == after.DeviceID || before.DeviceSecret == after.DeviceSecret || len(after.RemoteConnections) != 2 || after.DeviceEnrollmentClaim() != "synthetic-new-claim" {
		t.Fatal("retired enrollment was reused or duplicated")
	}
	if id, key, endpoint := after.RemoteIngress(); id != "" || key != "" || endpoint != "" {
		t.Fatal("replacement enrollment retained the retired pin")
	}
	if err := s.Update(func(c *Config) error {
		c.RemoteConnections[1].HubInstanceID = "new-id"
		c.RemoteConnections[1].IdentityPublicKey = "new-pin"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if id, key, _ := restarted.Get().RemoteIngress(); id != "new-id" || key != "new-pin" {
		t.Fatal("restart forgot persistent identity pin")
	}
}
