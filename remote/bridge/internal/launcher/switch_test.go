package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"salcara/bridge/internal/toolcfg"
)

func fixtureFile(t *testing.T, root, name, content string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func switchFixture(t *testing.T, kind, target string) (*Service, string, string) {
	t.Helper()
	root, appData := t.TempDir(), t.TempDir()
	t.Setenv("CODEX_HOME", root)
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	t.Setenv("CODEX_ELECTRON_USER_DATA_PATH", appData)
	t.Setenv("CLAUDE_USER_DATA_DIR", appData)
	s := New(t.TempDir())
	tool := stubTool(t, target)
	s.FindTools = func(context.Context, map[string]string) []Tool { return []Tool{tool} }
	s.Stop = func(context.Context, Plan) error { return nil }
	s.Start = func(context.Context, Plan) (int, error) { return 101, nil }
	return s, root, appData
}

func assertRetained(t *testing.T, before map[string]string) {
	t.Helper()
	for path, want := range before {
		b, err := os.ReadFile(path)
		if err != nil || digest(b) != want {
			t.Fatalf("retained data changed: %s", filepath.Base(path))
		}
	}
}

func TestSwitchSameCodexHomeKeepsHistoryAndProvider(t *testing.T) {
	for _, provider := range []string{"", "openai", "original-relay"} {
		t.Run("provider-"+provider, func(t *testing.T) {
			s, root, appData := switchFixture(t, "codex", "codex-desktop")
			content := "# existing user settings\nmodel = \"old-model\"\n"
			if provider != "" {
				content += "model_provider = " + toolcfg.TOMLString(provider) + "\n"
			}
			content += "[projects.\"C:/existing-project\"]\ntrust_level = \"trusted\"\n[features]\nplugins = true\n"
			if provider == "original-relay" {
				content += "[model_providers.original-relay]\nname = \"My provider\"\nbase_url = \"https://old.test/v1\"\nenv_key = \"STALE_KEY\"\nexperimental_bearer_token = \"stale-key\"\nrequest_max_retries = 3\n"
			}
			path := fixtureFile(t, root, "config.toml", content)
			auth := `{"auth_mode":"chatgpt","tokens":{"access_token":"fixture-original"}}`
			fixtureFile(t, root, "auth.json", auth)
			retained := map[string]string{}
			for _, name := range []string{"sessions/2026/09/30/original.jsonl", "state_5.sqlite", "history.jsonl", "session_index.jsonl", "plugins/config.json", "skills/existing/SKILL.md"} {
				p := fixtureFile(t, root, name, "existing-data-"+name)
				b, _ := os.ReadFile(p)
				retained[p] = digest(b)
			}
			for _, name := range []string{"Local Storage/leveldb/original.ldb", "projects.json"} {
				p := fixtureFile(t, appData, name, "original-app-data")
				b, _ := os.ReadFile(p)
				retained[p] = digest(b)
			}
			workspace := t.TempDir()
			project := fixtureFile(t, workspace, "finished-work.js", "do not lose this work")
			b, _ := os.ReadFile(project)
			retained[project] = digest(b)
			stops, starts := 0, 0
			s.Stop = func(_ context.Context, p Plan) error {
				stops++
				if !p.Shared || p.ProfileDir != root || p.AppData != appData || len(p.Args) != 0 {
					t.Fatal("new/isolated desktop profile")
				}
				return nil
			}
			s.Start = func(_ context.Context, p Plan) (int, error) {
				starts++
				if envValue(p, "CODEX_HOME") != root || envValue(p, "CODEX_ELECTRON_USER_DATA_PATH") != appData {
					t.Fatal("changed original data location")
				}
				var stored map[string]any
				b, _ := os.ReadFile(filepath.Join(root, "auth.json"))
				if json.Unmarshal(b, &stored) != nil || stored["OPENAI_API_KEY"] != envValue(p, "OPENAI_API_KEY") || stored["auth_mode"] != "apikey" {
					t.Fatal("credentials not switched together")
				}
				return 101, nil
			}
			a := account("codex", "first", "first-key")
			if _, err := s.PreviewSwitch(context.Background(), a, "codex-desktop", workspace, nil); err != nil {
				t.Fatal(err)
			}
			b, _ = os.ReadFile(path)
			if string(b) != content || starts != 0 || stops != 0 {
				t.Fatal("preview mutated data")
			}
			for _, key := range []string{"first-key", "second-key", "third-key"} {
				a.Key = key
				if _, err := s.Switch(context.Background(), a, "codex-desktop", workspace, nil); err != nil {
					t.Fatal(err)
				}
				b, _ = os.ReadFile(path)
				actual, _, _, _ := toolcfg.InspectCodexTOML(string(b))
				if actual != provider {
					t.Fatal("history provider changed")
				}
				if !strings.Contains(string(b), "trust_level = \"trusted\"") || !strings.Contains(string(b), "plugins = true") {
					t.Fatal("existing settings lost")
				}
				if provider == "original-relay" && (strings.Contains(string(b), "stale-key") || strings.Contains(string(b), "env_key =") || !strings.Contains(string(b), "request_max_retries = 3")) {
					t.Fatal("provider settings or stale auth")
				}
				assertRetained(t, retained)
			}
			if _, err := os.Stat(filepath.Join(s.Dir, "instances")); !os.IsNotExist(err) {
				t.Fatal("switch created isolated instances")
			}
			if starts != 3 || stops != 3 {
				t.Fatal("incorrect restart sequence")
			}
			if err := s.RestoreDefault("codex"); err != nil {
				t.Fatal(err)
			}
			b, _ = os.ReadFile(path)
			if string(b) != content {
				t.Fatal("original config not restored exactly")
			}
			b, _ = os.ReadFile(filepath.Join(root, "auth.json"))
			if string(b) != auth {
				t.Fatal("original authentication not restored exactly")
			}
			assertRetained(t, retained)
		})
	}
}

func TestSwitchFailureRollsBackImmediatelyPreviousAccount(t *testing.T) {
	s, root, _ := switchFixture(t, "codex", "codex-desktop")
	a := account("codex", "first", "first-key")
	if _, err := s.Switch(context.Background(), a, "codex-desktop", t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	before, err := captureFiles([]string{filepath.Join(root, "config.toml"), filepath.Join(root, "auth.json")})
	if err != nil {
		t.Fatal(err)
	}
	a.Key = "second-key"
	s.Stop = func(context.Context, Plan) error { return errors.New("fixture still running") }
	if _, err := s.Switch(context.Background(), a, "codex-desktop", t.TempDir(), nil); err == nil {
		t.Fatal("stop failure accepted")
	}
	for _, f := range before {
		b, _ := os.ReadFile(f.Path)
		if string(b) != string(f.Data) {
			t.Fatal("stop failure changed credentials")
		}
	}
	s.Stop = func(context.Context, Plan) error { return nil }
	s.Start = func(context.Context, Plan) (int, error) { return 0, errors.New("fixture failed to start") }
	if _, err := s.Switch(context.Background(), a, "codex-desktop", t.TempDir(), nil); err == nil || !strings.Contains(err.Error(), "回滚") {
		t.Fatal("start failure did not roll back")
	}
	for _, f := range before {
		b, _ := os.ReadFile(f.Path)
		if string(b) != string(f.Data) {
			t.Fatal("did not restore immediately previous account")
		}
	}
}

func TestSwitchCLIResumesOriginalHistoryAndPreservesNewSettings(t *testing.T) {
	for _, kind := range []string{"codex", "claude"} {
		t.Run(kind, func(t *testing.T) {
			s, root, _ := switchFixture(t, kind, kind)
			a := account(kind, "a", "key")
			s.Start = func(_ context.Context, p Plan) (int, error) {
				if p.ProfileDir != root || !strings.Contains(strings.Join(p.Args, " "), "resume") {
					t.Fatal("CLI starts new history")
				}
				return 101, nil
			}
			if _, err := s.Switch(context.Background(), a, kind, t.TempDir(), nil); err != nil {
				t.Fatal(err)
			}
			if kind == "codex" {
				path := filepath.Join(root, "config.toml")
				b, _ := os.ReadFile(path)
				_ = os.WriteFile(path, append(b, []byte("\n[features]\nplugins = true\n")...), 0o600)
				a.Key = "second"
				if _, err := s.Switch(context.Background(), a, kind, t.TempDir(), nil); err != nil {
					t.Fatal(err)
				}
				b, _ = os.ReadFile(path)
				if !strings.Contains(string(b), "plugins = true") {
					t.Fatal("later user settings lost")
				}
				if err := s.RestoreDefault(kind); err == nil {
					t.Fatal("restore would erase later user settings")
				}
			}
		})
	}
}

func TestSwitchDoesNotOverrideNamedProfileOrLocalProvider(t *testing.T) {
	for _, content := range []string{"profile = \"work\"\n", "model_provider = \"ollama\"\n", "forced_login_method = \"chatgpt\"\n", "model_provider = \"relay\"\n[model_providers.relay.auth]\ncommand = \"get-token\"\n"} {
		if _, err := toolcfg.SwitchCodexTOML(content, "https://provider.test/v1", "key", "model"); err == nil {
			t.Fatal("unsafe provider override accepted")
		}
	}
}
