package config

import "testing"

func TestToolProtocolDoesNotLeakAcrossAccounts(t *testing.T) {
	a := LocalAccount{ID: "a", Kind: "api", Model: "claude-sonnet-fixture"}
	b := LocalAccount{ID: "b", Kind: "api", Model: "deepseek-chat"}
	c := Config{LocalAccounts: []LocalAccount{a, b}, ToolAPISelections: map[string]string{"codex": "a"}, ToolProtocols: map[string]string{"codex": "anthropic"}}
	got := c.APIForTool(b, "codex")
	if got.Protocol != "chat" || !NeedsGateway(got) {
		t.Fatal("another account inherited the selected account's protocol")
	}
	for _, model := range []string{"claude-sonnet-fixture", "grok-fixture", "deepseek-chat"} {
		b.Model = model
		got = (Config{}).APIForTool(b, "codex")
		if got.Protocol == "responses" || !NeedsGateway(got) {
			t.Fatalf("cross-family model %s bypassed translator", model)
		}
	}
}

func TestClaudeCatalogDenyAppliesToRoutesAndDirectIDs(t *testing.T) {
	a := LocalAccount{Models: []string{"deepseek-chat", "claude-sonnet-4-5"}, ModelDenylist: []string{"deepseek-chat"}}
	routes, _ := ClaudeDesktopCatalog(a)
	if len(routes) != 1 {
		t.Fatal("denied model in desktop catalog")
	}
	for _, name := range []string{"deepseek-chat", ClaudeDesktopModelRoute("deepseek-chat")} {
		if _, ok := ResolveClaudeDesktopModel(a, name); ok {
			t.Fatal("direct or compatibility route bypassed deny")
		}
	}
}
