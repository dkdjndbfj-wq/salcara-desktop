package config

import "testing"

func TestNormalizeAgentFamilyOnlyAliasesBuiltinDesktopCards(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"codex-desktop", "codex"}, {"claude-desktop", "claude"},
		{" codex-desktop ", "codex"}, {"codex", "codex"}, {"claude", "claude"},
		{"hermes-desktop", "hermes-desktop"}, {"hermes-desktop-desktop", "hermes-desktop-desktop"},
		{"other-desktop", "other-desktop"}, {"Codex-desktop", "Codex-desktop"},
		{"fixture.agent-desktop", "fixture.agent-desktop"}, {"", ""},
	} {
		t.Run(tc.input, func(t *testing.T) {
			if got := NormalizeAgentFamily(tc.input); got != tc.want {
				t.Fatalf("family %q became %q; want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestExtensionDesktopSuffixRemoteSelectionsRemainIndependent(t *testing.T) {
	c := Config{LocalAccounts: []LocalAccount{{ID: "fixture-a", Kind: "api"}, {ID: "fixture-b", Kind: "api"}}}
	if err := c.SetRemoteAPIForAgent("hermes", "fixture-a", "model-a"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetRemoteAPIForAgent("hermes-desktop", "fixture-b", "model-b"); err != nil {
		t.Fatal(err)
	}
	if c.RemoteAPI["hermes"] != (RemoteAPI{AccountID: "fixture-a", Model: "model-a"}) ||
		c.RemoteAPI["hermes-desktop"] != (RemoteAPI{AccountID: "fixture-b", Model: "model-b"}) {
		t.Fatal("an extension suffix silently shared another family's account/model")
	}
	if err := c.SetRemoteAPIForAgent("hermes-desktop", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, present := c.RemoteAPI["hermes-desktop"]; present || c.RemoteAPI["hermes"].AccountID != "fixture-a" {
		t.Fatal("clearing an extension changed its sibling selection")
	}
}
