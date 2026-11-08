package config

import (
	"salcara/bridge/internal/protocol"
	"testing"
)

func TestRemoteCredentialBindsActualSourceAndPhone(t *testing.T) {
	a := LocalAccount{ID: "a", Kind: "api", SourceType: "api_key", BaseURL: "https://a.test", Key: "fixture-a"}
	b := a
	b.ID, b.Key = "b", "fixture-b"
	c := Config{LocalAccounts: []LocalAccount{a, b}, GatewayRuntimeKey: "process-fixture", RemoteExecutionArmed: true,
		GatewayKey: "persistent-fixture", RemoteAPI: map[string]RemoteAPI{"codex": {AccountID: a.ID}, "claude": {AccountID: a.ID}},
		PhoneBindingID: "binding", PhoneHash: "phone"}
	c.RemoteStationAuthorization = &StationAuthorization{Identity: RemoteIdentity(c), Revision: "r1", Station: protocol.StationPolicy{Version: 1, APIBases: []string{"https://a.test"}}}
	initial := c.RemoteGatewayCredential("remote-codex")
	if initial == "" || initial == c.GatewayRuntimeKey || initial == c.RemoteGatewayCredential("remote-claude") {
		t.Fatal("remote credential is not source/route scoped")
	}
	for _, edit := range []func(*Config){
		func(c *Config) { c.RemoteAPI["codex"] = RemoteAPI{AccountID: b.ID} },
		func(c *Config) { c.LocalAccounts[0].Key = "changed-key" },
		func(c *Config) { c.ToolProtocols = map[string]string{"codex": "chat"} },
		func(c *Config) { c.PhoneHash = "other-phone" },
		func(c *Config) { c.DeviceID = "other-station" },
		func(c *Config) { c.GatewayRuntimeKey = "other-process" },
	} {
		next := c.Clone()
		edit(&next)
		if got := next.RemoteGatewayCredential("remote-codex"); got == initial {
			t.Fatal("source mutation retained previous lease")
		}
		if got := next.RemoteGatewayCredential("codex"); got != c.GatewayKey {
			t.Fatal("remote mutation invalidated local tool")
		}
	}
	c.RemoteAPI = nil
	if got := c.RemoteGatewayCredential("remote-codex"); got != "" {
		t.Fatal("unmanaged route got a usable credential")
	}
}

func TestPoolNeedsGatewayEvenForNativeProtocol(t *testing.T) {
	a := LocalAccount{ID: "native-pool", Kind: "codex", Protocol: "responses", PoolEnabled: true, PoolGroup: "coding", BaseURL: "https://provider.test", Key: "fixture"}
	c := Config{GatewayKey: "local-fixture"}
	connection := c.ToolConnection(a, "codex", 12345)
	if connection.BaseURL != "http://127.0.0.1:12345/gateway/codex" || connection.Key != c.GatewayKey {
		t.Fatal("native pool bypassed scheduler")
	}
	before := AccountFingerprint(a)
	a.PoolEnabled = false
	if before == AccountFingerprint(a) {
		t.Fatal("membership change did not invalidate applied route")
	}
}

func TestRuntimeGatewayLeaseAppliesToEveryGatewayConnection(t *testing.T) {
	a := LocalAccount{ID: "native-pool", Kind: "codex", Protocol: "responses", PoolEnabled: true, PoolGroup: "coding", BaseURL: "https://provider.test", Key: "fixture"}
	c := Config{LocalAccounts: []LocalAccount{a}, GatewayKey: "persistent-local-key", GatewayRuntimeKey: "fresh-process-lease"}
	for _, target := range []string{"codex", "codex-desktop", "claude", "claude-desktop", "remote-codex", "remote-claude"} {
		t.Run(target, func(t *testing.T) {
			effective := c.APIForTool(a, target)
			c.RecordAppliedAPI(target, effective, "fixture")
			effective, _ = c.AuthorizedAppliedToolAccount(target)
			expected := c.LocalGatewayCredential(target)
			if target == "remote-codex" || target == "remote-claude" {
				expected = c.RemoteGatewayCredential(target)
			}
			connection := c.ToolConnection(effective, target, 12345)
			if connection.Key != expected || connection.AuthMode != "bearer" || connection.BaseURL != "http://127.0.0.1:12345/gateway/"+ToolFamily(target) {
				t.Fatalf("wrong gateway connection for %s: %+v", target, connection)
			}
		})
	}
	if got := c.Clone().GatewayRuntimeKey; got != "fresh-process-lease" {
		t.Fatalf("runtime lease was not kept in memory: %q", got)
	}
	previous := c.LocalGatewayCredential("codex")
	c.GatewayRuntimeKey = "next-process-lease"
	applied, _ := c.AuthorizedAppliedToolAccount("codex")
	if got := c.ToolConnection(applied, "codex", 12345).Key; got == "" || got == previous || got == c.GatewayRuntimeKey {
		t.Fatalf("local gateway did not derive a new scoped credential: %q", got)
	}
	if got := c.ToolConnection(a, "remote-codex", 12345).Key; got != "next-process-lease" {
		t.Fatalf("remote tool retained a previous process lease: %q", got)
	}
}

func TestUniversalKeyPerToolModelProtocolAndAppliedSnapshot(t *testing.T) {
	a := LocalAccount{ID: "one", Kind: "api", Name: "shared", BaseURL: "https://relay.test", Key: "upstream-secret", AuthMode: "bearer"}
	c := Config{LocalAccounts: []LocalAccount{a}, ToolAPISelections: map[string]string{"codex": "one", "claude": "one"}, ToolModels: map[string]string{"codex": "grok-id", "claude": "claude-id"}, ToolProtocols: map[string]string{"codex": "chat", "claude": "anthropic"}, GatewayKey: "loopback-secret"}
	cx, _ := c.SelectedToolAPI("codex")
	cl, _ := c.SelectedToolAPI("claude")
	if cx.Kind != "codex" || cx.Model != "grok-id" || cl.Kind != "claude" || cl.Model != "claude-id" || c.LocalAccounts[0].Kind != "api" || c.LocalAccounts[0].Model != "" {
		t.Fatal("key was assigned to a brand/tool")
	}
	c.RecordAppliedAPI("codex", cx, "openai")
	c.RecordAppliedAPI("claude", cl, "")
	c.ToolModels["codex"], c.ToolProtocols["codex"] = "other-model", "responses"
	old, valid := c.AppliedToolAccount("codex")
	if !valid || old.Model != "grok-id" || old.Protocol != "chat" {
		t.Fatal("pending card edits changed the live connection")
	}
	conn := c.ToolConnection(old, "codex", 12345)
	if conn.Key != c.GatewayKey || conn.BaseURL != "http://127.0.0.1:12345/gateway/codex" || old.Key != "upstream-secret" {
		t.Fatal("wrong credential boundary")
	}
	native := c.ToolConnection(cl, "claude", 12345)
	if native.Key != a.Key || native.BaseURL != a.BaseURL {
		t.Fatal("native connection unnecessarily proxied")
	}
	clone := c.Clone()
	clone.ToolModels["claude"] = "x"
	clone.ToolProtocols["claude"] = "chat"
	if c.ToolModels["claude"] != "claude-id" || c.ToolProtocols["claude"] != "anthropic" {
		t.Fatal("aliased maps")
	}
	c.LocalAccounts[0].Key = "edited"
	if _, ok := c.AppliedToolAccount("codex"); ok {
		t.Fatal("an edited upstream key silently became live")
	}
}

func TestNativeSnapshotRemainsNativeWhenPendingProtocolChanges(t *testing.T) {
	a := LocalAccount{ID: "key", Kind: "codex", Key: "secret", Model: "model", BaseURL: "https://relay.test"}
	c := Config{LocalAccounts: []LocalAccount{a}, ActiveCodexAccount: a.ID}
	c.RecordAppliedAPI("codex", a, "openai")
	c.ToolProtocols = map[string]string{"codex": "anthropic"}
	actual, ok := c.AppliedToolAccount("codex")
	if !ok || actual.Protocol != "responses" || NeedsAdapter(actual) {
		t.Fatal("native live connection changed because of a pending selection")
	}
}
