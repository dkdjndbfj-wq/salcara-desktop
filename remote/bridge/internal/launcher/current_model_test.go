package launcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentWithoutModelPickerDoesNotRequireCatalogForNativeLaunch(t *testing.T) {
	for _, target := range []string{"codex", "codex-desktop", "claude"} {
		t.Run(target, func(t *testing.T) {
			kind := strings.Split(target, "-")[0]
			s, root, _ := switchFixture(t, kind, target)
			name, content := "config.toml", "# fresh fixture\n[features]\nplugins = true\n"
			if kind == "claude" {
				name, content = "settings.json", `{"permissions":{"allow":["Read"]}}`
			}
			fixtureFile(t, root, name, content)
			a := account(kind, "shared-fixture", "fixture-key")
			a.Model, a.Models = "", nil
			plan, err := s.PrepareSwitch(context.Background(), a, target, t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, arg := range plan.Args {
				if arg == "--model" {
					t.Fatal("empty model forced on native tool")
				}
			}
			if envValue(plan, "ANTHROPIC_MODEL") != "" {
				t.Fatal("empty native model overridden by environment")
			}
			if _, err := s.Switch(context.Background(), a, target, t.TempDir(), nil); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(filepath.Join(root, name))
			if err != nil || strings.Contains(string(b), `model = ""`) || strings.Contains(string(b), `"model": ""`) {
				t.Fatal("API-only switch wrote a blank model")
			}
		})
	}
}

func TestAgentWithoutModelPickerPreservesOriginalSelection(t *testing.T) {
	for _, target := range []string{"codex", "codex-desktop", "claude"} {
		t.Run(target, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("CODEX_HOME", root)
			t.Setenv("CLAUDE_CONFIG_DIR", root)
			name, content, selected, protocol := "config.toml", "model = 'grok-current-fixture'\n[features]\nplugins = true\n", "grok-current-fixture", "chat"
			if target == "claude" {
				name, content, selected, protocol = "settings.json", `{"model":"claude-current-fixture","env":{"CUSTOM_UNRELATED":{"keep":true}},"permissions":{"allow":["Read"]}}`, "claude-current-fixture", "anthropic"
			}
			path := filepath.Join(root, name)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			a := account("codex", "shared-fixture", "fixture-key")
			a.Wire = "auto"
			got, err := PreserveCurrentToolModel(a, target)
			if err != nil || got.Model != selected || got.Protocol != protocol {
				t.Fatalf("selection not retained: model=%q protocol=%q err=%v", got.Model, got.Protocol, err)
			}
			b, err := os.ReadFile(path)
			if err != nil || string(b) != content {
				t.Fatal("read-only inspection changed original configuration")
			}
		})
	}
}

func TestAgentWithoutModelPickerFreshInstallUsesCatalogBootstrap(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	a := account("codex", "shared-fixture", "fixture-key")
	got, err := PreserveCurrentToolModel(a, "codex-desktop")
	if err != nil || got.Model != a.Model {
		t.Fatal("fresh install lost internal catalog bootstrap")
	}
}

func TestAgentWithoutModelPickerUnknownAliasUsesNativeProtocolNotBootstrap(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	fixtureFile(t, root, "config.toml", "model = 'custom-code-fixture'\n")
	a := account("codex", "shared-fixture", "fixture-key")
	a.Wire, a.Protocol, a.Model = "auto", "chat", "grok-bootstrap-fixture"
	got, err := PreserveCurrentToolModel(a, "codex-desktop")
	if err != nil || got.Model != "custom-code-fixture" || got.Protocol != "responses" {
		t.Fatal("unknown native alias inherited an unrelated bootstrap protocol")
	}
	a.Wire = "chat"
	got, err = PreserveCurrentToolModel(a, "codex-desktop")
	if err != nil || got.Protocol != "chat" {
		t.Fatal("explicit provider wire type ignored")
	}
}

func TestAgentWithoutModelPickerRejectsUnreadableOrInvalidSettings(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	path := filepath.Join(root, "settings.json")
	for _, content := range []string{`{broken`, `{"model":"bad\nmodel"}`, `{"env":{"ANTHROPIC_MODEL":42}}`} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := PreserveCurrentToolModel(account("claude", "shared-fixture", "fixture-key"), "claude"); err == nil {
			t.Fatal("invalid original selection guessed silently")
		}
	}
}
