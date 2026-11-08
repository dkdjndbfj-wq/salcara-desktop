package config

import "testing"

func TestRemoteGatewayCredentialDoesNotReviveAfterStoreRestoration(t *testing.T) {
	for _, family := range []string{"codex", "claude"} {
		for _, change := range []string{"account-selection", "station-revocation", "account-policy"} {
			t.Run(family+"/"+change, func(t *testing.T) {
				store := gatewayAuthorizationStore(t, false)
				updateGatewayAuthorization(t, store, func(c *Config) {
					c.RemoteAPI[family] = RemoteAPI{AccountID: "account-A", Model: "gpt-fixture"}
				})
				target := RemoteGatewayTarget(family)
				before := store.Get()
				initialKey := before.RemoteGatewayCredential(target)
				initialEpoch := before.GatewayAuthorizationRevision(target)
				if initialKey == "" || initialEpoch == "" {
					t.Fatal("fixture has no remote authority")
				}
				updateGatewayAuthorization(t, store, func(c *Config) {
					switch change {
					case "account-selection":
						c.RemoteAPI[family] = RemoteAPI{AccountID: "account-B", Model: "gpt-fixture"}
					case "station-revocation":
						c.RemoteStationAuthorization = nil
					case "account-policy":
						c.LocalAccounts[0].ModelDenylist = []string{"gpt-fixture"}
					}
				})
				changed := store.Get()
				if changed.GatewayAuthorizationRevision(target) == initialEpoch || changed.RemoteGatewayCredential(target) == initialKey {
					t.Fatal("authorization change retained the old worker credential")
				}
				if change == "station-revocation" && changed.RemoteGatewayCredential(target) != "" {
					t.Fatal("revoked station still has a remote credential")
				}
				updateGatewayAuthorization(t, store, func(c *Config) {
					restored := before.Clone()
					c.RemoteAPI = restored.RemoteAPI
					c.LocalAccounts = restored.LocalAccounts
					c.RemoteStationAuthorization = restored.RemoteStationAuthorization
				})
				restored := store.Get()
				currentKey := restored.RemoteGatewayCredential(target)
				if currentKey == "" || currentKey == initialKey || currentKey == changed.RemoteGatewayCredential(target) {
					t.Fatal("restoration did not issue a fresh worker credential")
				}
				if before.RemoteGatewayCredential(target) != initialKey || before.GatewayAuthorizationRevision(target) != initialEpoch {
					t.Fatal("Store update mutated an earlier worker snapshot")
				}
				account, ok := restored.GatewaySourceAccount(target)
				if !ok || restored.ToolConnection(account, target, 12345).Key != currentKey {
					t.Fatal("new worker did not receive the committed credential")
				}
				for range 3 {
					updateGatewayAuthorization(t, store, func(*Config) {})
					current := store.Get()
					if current.RemoteGatewayCredential(target) != currentKey || current.GatewayAuthorizationRevision(target) != restored.GatewayAuthorizationRevision(target) {
						t.Fatal("no-op save rotated a committed credential")
					}
				}
			})
		}
	}
}

func TestRemoteGatewayCredentialIgnoresUnrelatedStoreUpdates(t *testing.T) {
	for _, change := range []string{"metadata", "unselected-account", "other-route", "callback-epochs"} {
		t.Run(change, func(t *testing.T) {
			store := gatewayAuthorizationStore(t, false)
			updateGatewayAuthorization(t, store, func(c *Config) {
				c.RemoteAPI["claude"] = RemoteAPI{AccountID: "account-B", Model: "gpt-fixture"}
			})
			before := store.Get()
			codexKey := before.RemoteGatewayCredential("remote-codex")
			claudeKey := before.RemoteGatewayCredential("remote-claude")
			if codexKey == "" || claudeKey == "" || codexKey == claudeKey {
				t.Fatal("fixture credentials are not route scoped")
			}
			updateGatewayAuthorization(t, store, func(c *Config) {
				switch change {
				case "metadata":
					for i := range c.LocalAccounts {
						c.LocalAccounts[i].Name += " renamed"
						c.LocalAccounts[i].ProviderName = "Changed display label"
						c.LocalAccounts[i].Models = []string{"gpt-fixture", "new-catalog-hint"}
						c.LocalAccounts[i].LastUsedAt++
					}
					c.DeviceName = "Changed display name"
				case "unselected-account":
					c.LocalAccounts[2].Key = "synthetic-unrelated-key"
					c.LocalAccounts[2].ModelDenylist = []string{"gpt-fixture"}
				case "other-route":
					c.RemoteAPI["claude"] = RemoteAPI{AccountID: "account-C", Model: "gpt-fixture"}
				case "callback-epochs":
					c.GatewayAuthEpochs = map[string]string{"remote-codex": "callback-supplied-epoch"}
				}
			})
			current := store.Get()
			if current.RemoteGatewayCredential("remote-codex") != codexKey || current.GatewayAuthorizationRevision("remote-codex") != before.GatewayAuthorizationRevision("remote-codex") {
				t.Fatal("unrelated change rotated the Codex worker credential")
			}
			if current.LocalGatewayCredential("codex") != before.LocalGatewayCredential("codex") || current.GatewayAuthorizationRevision("codex") != before.GatewayAuthorizationRevision("codex") {
				t.Fatal("unrelated change revoked the local route")
			}
			if rotated := current.RemoteGatewayCredential("remote-claude") != claudeKey; rotated != (change == "other-route") {
				t.Fatal("Claude credential did not track only its own authorization")
			}
		})
	}
}
