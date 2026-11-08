package config

import (
	"os"
	"path/filepath"
	"reflect"
	"salcara/bridge/internal/protocol"
	"strings"
	"testing"
)

func gatewayAuthorizationStore(t *testing.T, pooled bool) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	store.SetRuntimeGatewayKey("synthetic-gateway-runtime")
	if err := store.Update(func(c *Config) error {
		c.LocalAccounts = []LocalAccount{
			{ID: "account-A", Name: "A", Kind: "api", SourceType: "api_key", BaseURL: "https://api.fixture.test", Key: "synthetic-account-A", Model: "gpt-fixture", Wire: "responses", PoolEnabled: pooled, PoolGroup: "shared"},
			{ID: "account-B", Name: "B", Kind: "api", SourceType: "api_key", BaseURL: "https://api.fixture.test", Key: "synthetic-account-B", Model: "gpt-fixture", Wire: "responses", PoolEnabled: true, PoolGroup: "shared"},
			{ID: "account-C", Name: "C", Kind: "api", SourceType: "api_key", BaseURL: "https://api.fixture.test", Key: "synthetic-account-C", Model: "gpt-fixture", Wire: "responses", PoolEnabled: true, PoolGroup: "other"},
		}
		c.RecordAppliedAPI("codex", c.APIForTool(c.LocalAccounts[0], "codex"), "openai")
		c.RemoteAPI = map[string]RemoteAPI{"codex": {AccountID: "account-A", Model: "gpt-fixture"}}
		c.HubURL, c.DeviceID, c.DeviceSecret = "https://hub.fixture.test", "fixture-device", "synthetic-device-secret"
		c.PhoneBindingID, c.PhoneHash = "fixture-binding", "fixture-phone"
		c.RemoteStationAuthorization = &StationAuthorization{Identity: RemoteIdentity(*c), Revision: "r1", Station: protocol.StationPolicy{Version: 1, APIBases: []string{"https://api.fixture.test"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

func updateGatewayAuthorization(t *testing.T, store *Store, fn func(*Config)) {
	t.Helper()
	if err := store.Update(func(c *Config) error { fn(c); return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayPoolMemberAuthorizationChangesFenceOnlyItsRoute(t *testing.T) {
	for _, policy := range []string{"key", "model-policy", "membership", "concurrency", "priority", "weight"} {
		t.Run(policy, func(t *testing.T) {
			store := gatewayAuthorizationStore(t, true)
			before := store.Get()
			updateGatewayAuthorization(t, store, func(c *Config) {
				member := &c.LocalAccounts[1]
				switch policy {
				case "key":
					member.Key = "synthetic-rotated-member-key"
				case "model-policy":
					member.ModelDenylist = []string{"gpt-fixture"}
				case "membership":
					member.PoolEnabled = false
				case "concurrency":
					member.MaxConcurrency = 1
				case "priority":
					member.Priority = 1
				case "weight":
					member.Weight = 2
				}
			})
			changed := store.Get()
			if changed.GatewayAuthorizationRevision("codex") == before.GatewayAuthorizationRevision("codex") {
				t.Fatal("related pool policy did not fence queued local work")
			}
			if changed.GatewayAuthorizationRevision("remote-codex") != before.GatewayAuthorizationRevision("remote-codex") {
				t.Fatal("fixed-account remote worker inherited a local pool policy change")
			}
			updateGatewayAuthorization(t, store, func(c *Config) { c.LocalAccounts[1] = before.LocalAccounts[1] })
			if restored := store.Get().GatewayAuthorizationRevision("codex"); restored == before.GatewayAuthorizationRevision("codex") || restored == changed.GatewayAuthorizationRevision("codex") {
				t.Fatal("restoring a pool policy revived an earlier authorization epoch")
			}
		})
	}
}

func TestGatewayUnrelatedAccountsAndCatalogMetadataDoNotRevokeRoute(t *testing.T) {
	for _, pooled := range []bool{false, true} {
		t.Run(map[bool]string{false: "manual", true: "pool"}[pooled], func(t *testing.T) {
			store := gatewayAuthorizationStore(t, pooled)
			before := store.Get()
			updateGatewayAuthorization(t, store, func(c *Config) {
				unrelated := 1
				if pooled {
					unrelated = 2 // A different pool group cannot serve this route.
				}
				c.LocalAccounts[unrelated].Key = "synthetic-unrelated-key"
				c.LocalAccounts[unrelated].ModelDenylist = []string{"gpt-fixture"}
				for i := range c.LocalAccounts {
					c.LocalAccounts[i].Name += " renamed"
					c.LocalAccounts[i].ProviderName = "Changed display label"
					c.LocalAccounts[i].Models = []string{"gpt-fixture", "new-catalog-hint"}
					c.LocalAccounts[i].LastUsedAt++
				}
				c.DeviceName = "Changed display name"
			})
			for _, route := range []string{"codex", "remote-codex"} {
				if store.Get().GatewayAuthorizationRevision(route) != before.GatewayAuthorizationRevision(route) {
					t.Fatalf("unrelated configuration revoked %s", route)
				}
			}
		})
	}
}

func TestGatewayRemoteBoundAccountPolicyRestorationCannotReviveEpoch(t *testing.T) {
	store := gatewayAuthorizationStore(t, false)
	before := store.Get()
	updateGatewayAuthorization(t, store, func(c *Config) { c.LocalAccounts[0].ModelDenylist = []string{"gpt-fixture"} })
	changed := store.Get()
	if changed.GatewayAuthorizationRevision("remote-codex") == before.GatewayAuthorizationRevision("remote-codex") {
		t.Fatal("remote account policy changed without fencing queued work")
	}
	updateGatewayAuthorization(t, store, func(c *Config) { c.LocalAccounts[0] = before.LocalAccounts[0] })
	restored := store.Get().GatewayAuthorizationRevision("remote-codex")
	if restored == before.GatewayAuthorizationRevision("remote-codex") || restored == changed.GatewayAuthorizationRevision("remote-codex") {
		t.Fatal("restored remote policy revived old work")
	}
}

func TestGatewayUnarmedRemoteIdentityStillFencesQueuedWork(t *testing.T) {
	for _, field := range []string{"station", "phone-binding", "phone-identity"} {
		t.Run(field, func(t *testing.T) {
			store := gatewayAuthorizationStore(t, false)
			updateGatewayAuthorization(t, store, func(c *Config) { c.RemoteExecutionArmed = false })
			before := store.Get()
			updateGatewayAuthorization(t, store, func(c *Config) {
				switch field {
				case "station":
					c.HubURL = "https://different-hub.fixture.test"
				case "phone-binding":
					c.PhoneBindingID = "different-fixture-binding"
				case "phone-identity":
					c.PhoneHash = "different-fixture-phone"
				}
			})
			changed := store.Get()
			if changed.GatewayAuthorizationRevision("remote-codex") == before.GatewayAuthorizationRevision("remote-codex") {
				t.Fatal("unarmed remote identity change did not fence old work")
			}
			if changed.GatewayAuthorizationRevision("codex") != before.GatewayAuthorizationRevision("codex") {
				t.Fatal("remote identity change revoked a local tool route")
			}
			updateGatewayAuthorization(t, store, func(c *Config) {
				c.HubURL, c.PhoneBindingID, c.PhoneHash = before.HubURL, before.PhoneBindingID, before.PhoneHash
			})
			if store.Get().GatewayAuthorizationRevision("remote-codex") == before.GatewayAuthorizationRevision("remote-codex") {
				t.Fatal("restored remote identity revived an earlier epoch")
			}
		})
	}
}

func TestGatewayEpochsAreStoreOwnedPrivateAndCloneIsolated(t *testing.T) {
	store := gatewayAuthorizationStore(t, false)
	before := store.Get()
	clone := before.Clone()
	clone.GatewayAuthEpochs["codex"] = "synthetic-mutated-clone"
	if !reflect.DeepEqual(before.GatewayAuthEpochs, store.Get().GatewayAuthEpochs) || clone.GatewayAuthEpochs["codex"] == before.GatewayAuthEpochs["codex"] {
		t.Fatal("gateway epoch map aliases a caller snapshot")
	}
	updateGatewayAuthorization(t, store, func(c *Config) { c.GatewayAuthEpochs = map[string]string{"codex": "callback-supplied-epoch"} })
	if !reflect.DeepEqual(before.GatewayAuthEpochs, store.Get().GatewayAuthEpochs) {
		t.Fatal("callback replaced Store-owned gateway epochs")
	}
	oldPath := store.path
	store.path = t.TempDir()
	err := store.Update(func(c *Config) error { c.LocalAccounts[0].ModelDenylist = []string{"gpt-fixture"}; return nil })
	store.path = oldPath
	if err == nil || !reflect.DeepEqual(before, store.Get()) {
		t.Fatal("failed persistence changed live gateway authorization")
	}
	raw, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "GatewayAuthEpochs") || strings.Contains(string(raw), "gatewayAuthEpochs") {
		t.Fatal("private gateway epoch field was persisted")
	}
	for _, epoch := range before.GatewayAuthEpochs {
		if epoch == "" || strings.Contains(string(raw), epoch) {
			t.Fatal("gateway epoch is empty or persisted")
		}
	}
	reopened, err := Open(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range gatewayAuthorizationTargets {
		if reopened.Get().GatewayAuthorizationRevision(route) == before.GatewayAuthorizationRevision(route) {
			t.Fatal("reopened Store inherited an old process epoch")
		}
	}
}
