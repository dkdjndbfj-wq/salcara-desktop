package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/toolcfg"
)

func TestNativeClaudeSwitchAndResumePinSelectedAPI(t *testing.T) {
	for _, mode := range []string{"api-key", "bearer"} {
		for _, action := range []string{"switch", "resume"} {
			t.Run(mode+"/"+action, func(t *testing.T) {
				s, profile, _ := switchFixture(t, "claude", "claude")
				cwd := t.TempDir()
				retained := map[string]string{}
				for _, path := range []string{fixtureFile(t, profile, ".credentials.json", "existing-login"), fixtureFile(t, cwd, ".claude/settings.json", `{"env":{"CLAUDE_CODE_USE_VERTEX":"1","ANTHROPIC_BASE_URL":"https://old.invalid"},"hooks":{"Stop":[]}}`), fixtureFile(t, cwd, ".claude/settings.local.json", `{"env":{"CLAUDE_CODE_USE_BEDROCK":"1"}}`)} {
					data, _ := os.ReadFile(path)
					retained[path] = digest(data)
				}
				a := account("claude", "native", "synthetic-selected-key")
				a.AuthMode = mode
				for _, key := range []string{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_USE_ANTHROPIC_AWS", "CLAUDE_CODE_USE_MANTLE", "ANTHROPIC_CUSTOM_HEADERS", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_PROFILE"} {
					t.Setenv(key, "old-provider")
				}
				var pin string
				s.Start = func(_ context.Context, p Plan) (int, error) {
					pin = p.ClaudeSettingsPin
					if pin == "" || !filepath.IsAbs(pin) || strings.Contains(strings.Join(p.Args, " "), a.Key) {
						t.Fatal("missing or unsafe runtime pin")
					}
					if !reflect.DeepEqual(p.Args[len(p.Args)-2:], []string{"--settings", pin}) {
						t.Fatal("CLI did not receive settings path")
					}
					var settings map[string]json.RawMessage
					data, err := os.ReadFile(pin)
					if err != nil || json.Unmarshal(data, &settings) != nil || len(settings) != 1 {
						t.Fatal("pin copied unrelated settings or hooks")
					}
					var env map[string]string
					if json.Unmarshal(settings["env"], &env) != nil {
						t.Fatal("invalid pin env")
					}
					normalized, _, _ := config.APIBase(a.BaseURL)
					for key, want := range toolcfg.ClaudeAPIEnvironment(normalized, a.Key, a.AuthMode) {
						if env[key] != want || envValue(p, key) != want {
							t.Fatalf("pin/environment differs for %s", key)
						}
					}
					if env["ANTHROPIC_MODEL"] != a.Model {
						t.Fatal("lost selected model")
					}
					assertRetained(t, retained)
					return 123, nil
				}
				var err error
				if action == "switch" {
					_, err = s.Switch(context.Background(), a, "claude", cwd, nil)
				} else {
					_, err = s.Resume(context.Background(), a, "claude", "11111111-2222-3333-4444-555555555555", cwd, nil)
				}
				if err != nil {
					t.Fatal(err)
				}
				defer agents.RemoveClaudeRuntimeSettings(pin)
				if _, err := os.Stat(pin); err != nil {
					t.Fatal("successful launch removed pin before child exit")
				}
				assertRetained(t, retained)
			})
		}
	}
}

func TestNativeClaudeFailedStartCleansPinAndRestoresConfig(t *testing.T) {
	for _, action := range []string{"switch", "resume"} {
		t.Run(action, func(t *testing.T) {
			s, profile, _ := switchFixture(t, "claude", "claude")
			original := `{"env":{"CLAUDE_CODE_USE_BEDROCK":"1"},"model":"original-model"}`
			settings := fixtureFile(t, profile, "settings.json", original)
			var pin string
			s.Start = func(_ context.Context, p Plan) (int, error) {
				pin = p.ClaudeSettingsPin
				return 0, errors.New("synthetic failure")
			}
			a := account("claude", "native", "synthetic-key")
			var err error
			if action == "switch" {
				_, err = s.Switch(context.Background(), a, "claude", t.TempDir(), nil)
			} else {
				_, err = s.Resume(context.Background(), a, "claude", "11111111-2222-3333-4444-555555555555", t.TempDir(), nil)
			}
			if err == nil || pin == "" {
				t.Fatal("failed start not exercised")
			}
			if _, err := os.Stat(filepath.Dir(pin)); !os.IsNotExist(err) {
				t.Fatal("failed start left credentials")
			}
			assertRetained(t, map[string]string{settings: digest([]byte(original))})
		})
	}
}

func TestNativeClaudePreviewAndApplyDoNotCreateRuntimePins(t *testing.T) {
	s, _, _ := switchFixture(t, "claude", "claude")
	a := account("claude", "native", "synthetic-key")
	s.Start = func(context.Context, Plan) (int, error) { t.Fatal("unexpected process start"); return 0, nil }
	if _, err := s.PreviewSwitch(context.Background(), a, "claude", t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyAuthorized(context.Background(), a, "claude", t.TempDir(), nil, nil); err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(os.TempDir(), "salcara-claude-runtime-*"))
	if err != nil || len(paths) != 0 {
		t.Fatal("preview/apply allocated runtime credentials")
	}
}

func TestNormalizeClaudeNativeNPMShimPreservesArguments(t *testing.T) {
	for _, install := range []string{"global", "project"} {
		for _, extension := range []string{".cmd", ".bat"} {
			t.Run(install+extension, func(t *testing.T) {
				root := t.TempDir()
				shimDir := root
				modules := filepath.Join(root, "node_modules")
				if install == "project" {
					shimDir = filepath.Join(modules, ".bin")
				}
				shim := fixtureFile(t, shimDir, "claude"+extension, "synthetic npm shim")
				native := fixtureFile(t, modules, "@anthropic-ai/claude-code/bin/claude.exe", "synthetic native CLI")
				// Native releases must not require Node or reinterpret model/session
				// arguments through CMD, even if an obsolete JS entry remains.
				fixtureFile(t, modules, "@anthropic-ai/claude-code/cli.js", "obsolete JS entry")
				args := []string{"--resume", "11111111-2222-3333-4444-555555555555", "--model", "model & literal", "--settings", filepath.Join(root, "settings with spaces.json")}
				p := Plan{Tool: Tool{ID: "claude", Kind: "claude", Path: shim}, Args: append([]string(nil), args...)}
				if err := normalizeCLI(&p); err != nil {
					t.Fatal(err)
				}
				if p.Tool.Path != native || !reflect.DeepEqual(p.Args, args) {
					t.Fatal("native npm executable or literal arguments lost")
				}
			})
		}
	}
}
