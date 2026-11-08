package config

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestRemoteModeLegacyInferenceAndRestartAreExclusive(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Update(func(c *Config) error {
		c.ConnectRemote("https://hub.example.test", "https://hub.example.test/salcara-hub")
		c.ActiveRemoteMode = "" // simulate the old format with both channels enabled
		c.DirectRelayEnabled = true
		c.DirectRelayHosts = []string{"192.168.1.20"}
		c.DirectPhoneID, c.DirectPhoneHash = "direct-phone", "synthetic-hash"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	c := reopened.Get()
	if c.EffectiveRemoteMode() != "direct" || !c.DirectRemoteEnabled() || c.HubRemoteEnabled() {
		t.Fatal("old dual configuration started both transports")
	}
	before := c.Clone()
	c.ActivateRemoteMode("hub")
	if c.DirectRemoteEnabled() || !c.HubRemoteEnabled() || c.EffectiveRemoteMode() != "hub" {
		t.Fatal("hub selection did not disable direct execution")
	}
	if !reflect.DeepEqual(c.RemoteConnections, before.RemoteConnections) || c.DirectPhoneID != before.DirectPhoneID || c.DirectPhoneHash != before.DirectPhoneHash || !reflect.DeepEqual(c.DirectRelayHosts, before.DirectRelayHosts) {
		t.Fatal("mode switch erased dormant pairing")
	}
	c.ActivateRemoteMode("none")
	if c.HubRemoteEnabled() || c.DirectRemoteEnabled() || !c.LoggedIn() {
		t.Fatal("disable erased saved credentials or left an active channel")
	}
}

func TestRemoteModeExplicitBindingSelectsOnlyItsTransport(t *testing.T) {
	c := Config{DirectRelayEnabled: true, DirectPhoneID: "saved-phone", DirectPhoneHash: "saved-hash"}
	c.ConnectRemote("https://relay.example.test", "https://relay.example.test/salcara-hub")
	if c.EffectiveRemoteMode() != "hub" || c.DirectRelayEnabled {
		t.Fatal("Hub binding kept direct active")
	}
	c.ConnectRemoteWithBotBinding("https://bot.example.test", "https://bot.example.test/salcara-hub", "group", "invite", "signature")
	if c.EffectiveRemoteMode() != "bot" || c.DirectRelayEnabled || len(c.RemoteConnections) != 2 {
		t.Fatal("Bot binding did not preserve Hub binding exclusively")
	}
	c.ActivateRemoteMode("direct")
	if c.RemoteGatewayCredential("remote-codex") != "" || c.DirectPhoneID != "saved-phone" {
		t.Fatal("inactive Hub retained gateway execution or direct pairing was erased")
	}
}

func TestRemoteModeSameServerKeepsIndependentHubAndBotCredentials(t *testing.T) {
	const root, hub = "https://shared.example.test", "https://shared.example.test/salcara-hub"
	for _, botFirst := range []bool{false, true} {
		c := Config{}
		if botFirst {
			c.ConnectRemoteWithBotBinding(root, hub, "group", "invite", "signature")
		}
		c.ConnectRemote(root, hub)
		hubID, hubSecret := c.DeviceID, c.DeviceSecret
		c.ConnectRemoteWithBotBinding(root, hub+"/v1/", "group", "invite", "signature")
		botID, botSecret := c.DeviceID, c.DeviceSecret
		if len(c.RemoteConnections) != 2 || hubID == botID || hubSecret == botSecret || c.EffectiveRemoteMode() != "bot" {
			t.Fatal("same-server Bot replaced Hub credentials")
		}
		saved := c.Clone()
		c.ConnectRemote(root, hub+"/")
		if c.EffectiveRemoteMode() != "hub" || c.DeviceID != hubID || c.DeviceSecret != hubSecret {
			t.Fatal("Hub discovery selected the same-server Bot identity")
		}
		if err := c.RestoreRemoteConnection(root, hub, botID); err != nil {
			t.Fatal(err)
		}
		if c.EffectiveRemoteMode() != "bot" || c.DeviceID != botID || c.DeviceSecret != botSecret || !reflect.DeepEqual(c.RemoteConnections, saved.RemoteConnections) {
			t.Fatal("resuming Bot rewrote a saved connection")
		}
	}
}
