package agents

import (
	"os"
	"reflect"
	"testing"

	"salcara/bridge/internal/config"
)

func TestAppliedLocalAPIIsSeparateFromHubLoginAndSelection(t *testing.T) {
	cx := config.LocalAccount{ID: "cx", Kind: "codex", Key: "local-codex", BaseURL: "https://cx.test", Model: "cx-model"}
	cl := config.LocalAccount{ID: "cl", Kind: "claude", Key: "local-claude", BaseURL: "https://cl.test", Model: "cl-model", AuthMode: "api-key"}
	c := config.Config{RelayRoot: "https://hub-login.test", AccountKey: "hub-login-key", LocalAccounts: []config.LocalAccount{cx, cl}, LocalToolPaths: map[string]string{"codex": os.Args[0], "claude": os.Args[0]}}
	legacy := SettingsFromConfig(c)
	if legacy.UseOriginalCodex || legacy.CodexKey != c.AccountKey {
		t.Fatal("changed legacy remote configuration")
	}
	c.RecordAppliedAPI("codex-desktop", cx, "original")
	c.RecordAppliedAPI("claude", cl, "")
	s := SettingsFromConfig(c)
	if !s.UseOriginalCodex || s.codexProvider() != "original" || s.CodexKey != cx.Key || s.ClaudeKey != cl.Key || s.ClaudeAuthMode != "api-key" || s.codexRoot() != cx.BaseURL || s.claudeRoot() != cl.BaseURL || s.RelayRoot != c.RelayRoot {
		t.Fatal("Hub and tool APIs mixed")
	}
	if !reflect.DeepEqual(codexArgs("codex.exe", s), []string{"app-server"}) {
		t.Fatal("changed original provider or history visibility")
	}
	c.ToolAPISelections["codex"] = ""
	if SettingsFromConfig(c).CodexKey != cx.Key {
		t.Fatal("deselection overwrote applied API")
	}
	c.LocalAccounts[0].Key = "changed"
	c.LocalAccounts[1].Key = "changed-claude-key"
	s = SettingsFromConfig(c)
	if s.CodexKey != "" || s.ClaudeKey != "" || s.codexRoot() != "" {
		t.Fatal("invalid binding used edited key or fell back to Hub key")
	}
	if c.AccountKey != "hub-login-key" {
		t.Fatal("mutated Hub login")
	}
}

func TestRuntimeCLIPathsFollowWorkbench(t *testing.T) {
	s := Settings{CodexPath: os.Args[0], ClaudePath: os.Args[0]}
	m := NewManagerWithOptions(nil, func() Settings { return s }, Options{StateDir: t.TempDir(), ClaudeHome: t.TempDir()})
	defer m.Close()
	mm := m.(*manager)
	if p, ok := mm.codex.exe(); !ok || p != os.Args[0] {
		t.Fatal("Codex ignored workbench path")
	}
	if p, ok := mm.claude.exe(); !ok || p != os.Args[0] {
		t.Fatal("Claude ignored workbench path")
	}
	s.CodexPath, s.ClaudePath = "missing-custom-tool", "missing-custom-tool"
	if _, ok := mm.codex.exe(); ok {
		t.Fatal("invalid custom Codex path fell back silently")
	}
	if _, ok := mm.claude.exe(); ok {
		t.Fatal("invalid custom Claude path fell back silently")
	}
}
