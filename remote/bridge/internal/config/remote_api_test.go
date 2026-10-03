package config

import "testing"

func TestRemoteToolAccountUsesNativeProtocolUnlessSelectedOnCard(t *testing.T) {
	a := LocalAccount{ID: "a1", Name: "A", Kind: "api", Key: "k", BaseURL: "https://x.test", Model: "m0"}
	b := LocalAccount{ID: "b1", Name: "B", Kind: "api", Key: "k2", BaseURL: "https://y.test"}
	c := Config{LocalAccounts: []LocalAccount{a, b}, ToolProtocols: map[string]string{"codex": "chat"}, ToolAPISelections: map[string]string{"codex": "b1"}}
	if err := c.SetRemoteAPI("codex", "a1", "m1"); err != nil {
		t.Fatal(err)
	}
	got, ok := c.RemoteToolAccount("codex")
	if !ok || got.Model != "m1" || got.Protocol != "responses" {
		t.Fatalf("remote account: %+v %v", got, ok)
	}
	c.ToolAPISelections = map[string]string{"codex": "a1"}
	if got, _ := c.RemoteToolAccount("codex"); got.Protocol != "chat" || !NeedsAdapter(got) {
		t.Fatalf("selected card protocol ignored: %+v", got)
	}
	if err := c.SetRemoteAPI("codex", "missing", ""); err == nil {
		t.Fatal("missing account accepted")
	}
	if err := c.SetRemoteAPI("other", "a1", ""); err == nil {
		t.Fatal("unknown family accepted")
	}
	c.LocalAccounts = nil
	if _, ok := c.RemoteToolAccount("codex"); ok {
		t.Fatal("deleted account still used")
	}
}

func TestInferProtocolLetsAnyToolUseAnyModelFamily(t *testing.T) {
	cases := map[[2]string]string{
		{"claude-sonnet-4-5", ""}: "anthropic", {"anthropic/claude-opus", "auto"}: "anthropic",
		{"gpt-5-codex", ""}: "responses", {"o3-mini", ""}: "responses",
		{"grok-code-fast-1", ""}: "chat", {"deepseek-chat", ""}: "chat", {"qwen3-coder", "auto"}: "chat",
		{"claude-sonnet-4-5", "chat"}: "chat", {"", ""}: "",
	}
	for in, want := range cases {
		if got := InferProtocol(in[0], in[1]); got != want {
			t.Fatalf("%v: %s != %s", in, got, want)
		}
	}
}

func TestEmptyRemoteModelNeverInheritsVaultOrToolModel(t *testing.T) {
	c := Config{
		LocalAccounts:     []LocalAccount{{ID: "new", Kind: "api", Model: "vault-default", Models: []string{"new-only"}}},
		ToolAPISelections: map[string]string{"codex": "new"},
		ToolModels:        map[string]string{"codex": "old-key-model"},
		RemoteAPI:         map[string]RemoteAPI{"codex": {AccountID: "new"}},
	}
	a, ok := c.RemoteToolAccount("codex")
	if !ok || a.Model != "" {
		t.Fatalf("empty phone model inherited another default: %+v", a)
	}
}
