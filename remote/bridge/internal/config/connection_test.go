package config

import "testing"

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
