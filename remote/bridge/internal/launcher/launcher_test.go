package launcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/toolcfg"
)

func account(kind, id, key string) config.LocalAccount {
	return config.LocalAccount{ID: id, Name: "local " + id, Kind: kind, BaseURL: "https://provider.test/proxy/v1", Key: key, Model: "coding-model", AuthMode: "bearer"}
}

func stubTool(t *testing.T, kind string) Tool {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tool.exe")
	if err := os.WriteFile(path, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	return Tool{ID: kind, Kind: strings.Split(kind, "-")[0], Name: toolNames[kind], Path: path, Available: true}
}

func envValue(p Plan, key string) string {
	for _, kv := range p.Environment {
		k, v, _ := strings.Cut(kv, "=")
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}

func TestIsolatedCodexProfilesAndEnvironment(t *testing.T) {
	global := t.TempDir()
	t.Setenv("CODEX_HOME", global)
	t.Setenv("OPENAI_API_KEY", "stale-global-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "unrelated-secret")
	t.Setenv("OPENAI_BASE_URL", "https://stale.test")
	original := []byte("model_provider = \"openai\"\n# user settings\n")
	if err := os.WriteFile(filepath.Join(global, "config.toml"), original, 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(t.TempDir())
	tool := stubTool(t, "codex-desktop")
	cwd := t.TempDir()
	p1, err := s.Prepare(account("codex", "first", "first-key"), tool, cwd)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := s.Prepare(account("codex", "second", "second-key"), tool, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if p1.ProfileDir == p2.ProfileDir || p1.AppData == p2.AppData {
		t.Fatal("shared profile")
	}
	for _, p := range []Plan{p1, p2} {
		if envValue(p, "CODEX_HOME") != p.ProfileDir || envValue(p, "CODEX_ELECTRON_USER_DATA_PATH") != p.AppData || envValue(p, "ANTHROPIC_AUTH_TOKEN") != "" || envValue(p, "OPENAI_BASE_URL") != "" {
			t.Fatal("environment isolation failed")
		}
		b, _ := os.ReadFile(filepath.Join(p.ProfileDir, "config.toml"))
		if !strings.Contains(string(b), `base_url = "https://provider.test/proxy/v1"`) || !strings.Contains(string(b), `model = "coding-model"`) || !strings.Contains(string(b), `wire_api = "responses"`) {
			t.Fatalf("wrong config %s", b)
		}
		var auth map[string]any
		b, _ = os.ReadFile(filepath.Join(p.ProfileDir, "auth.json"))
		if json.Unmarshal(b, &auth) != nil || auth["OPENAI_API_KEY"] != envValue(p, "OPENAI_API_KEY") {
			t.Fatal("auth key differs")
		}
	}
	if envValue(p1, "OPENAI_API_KEY") != "first-key" || envValue(p2, "OPENAI_API_KEY") != "second-key" {
		t.Fatal("keys mixed")
	}
	b, _ := os.ReadFile(filepath.Join(global, "config.toml"))
	if string(b) != string(original) {
		t.Fatal("global config mutated by launch")
	}
	bad := account("codex", "../escape", "key")
	if _, err := s.Prepare(bad, tool, cwd); err == nil {
		t.Fatal("accepted traversal")
	}
	bad = account("codex", "nomodel", "key")
	bad.Model = ""
	if _, err := s.Prepare(bad, tool, cwd); err == nil {
		t.Fatal("no model launch")
	}
}

func TestClaudeCodeProfile(t *testing.T) {
	s := New(t.TempDir())
	a := account("claude", "claude-a", "claude-key")
	a.AuthMode = "api-key"
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "stale-token")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "stale-oauth")
	p, err := s.Prepare(a, stubTool(t, "claude"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if envValue(p, "ANTHROPIC_AUTH_TOKEN") != "" || envValue(p, "CLAUDE_CODE_OAUTH_TOKEN") != "" || envValue(p, "ANTHROPIC_API_KEY") != a.Key {
		t.Fatal("stale Claude auth was kept")
	}
	var settings map[string]any
	b, _ := os.ReadFile(filepath.Join(p.ProfileDir, "settings.json"))
	if json.Unmarshal(b, &settings) != nil {
		t.Fatal("invalid settings")
	}
	env := settings["env"].(map[string]any)
	if env["ANTHROPIC_AUTH_TOKEN"] != "" || env["ANTHROPIC_API_KEY"] != a.Key || settings["model"] != a.Model {
		t.Fatal("wrong settings")
	}
	if _, err := s.Prepare(a, stubTool(t, "codex"), t.TempDir()); err == nil {
		t.Fatal("mismatched protocol accepted")
	}
}

func TestModelsAuthenticationAndNoRedirect(t *testing.T) {
	for _, tc := range []struct{ kind, mode, header, want string }{{"codex", "bearer", "Authorization", "Bearer private-key"}, {"claude", "bearer", "Authorization", "Bearer private-key"}, {"claude", "api-key", "x-api-key", "private-key"}} {
		t.Run(tc.kind+tc.mode, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/proxy/v1/models" || r.Header.Get(tc.header) != tc.want {
					t.Errorf("wrong model request %s %s", r.Method, r.URL.Path)
				}
				if tc.kind == "claude" && r.Header.Get("anthropic-version") == "" {
					t.Error("missing version")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":[{"id":"z"},{"id":"a"},{"id":"a"},{"id":""}]}`))
			}))
			defer srv.Close()
			a := account(tc.kind, "a", "private-key")
			a.BaseURL = srv.URL + "/proxy"
			a.AuthMode = tc.mode
			models, err := Models(context.Background(), a)
			if err != nil || strings.Join(models, ",") != "a,z" {
				t.Fatal(models, err)
			}
		})
	}
	called := false
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer dest.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, dest.URL, 302) }))
	defer srv.Close()
	a := account("claude", "a", "never-forward")
	a.BaseURL = srv.URL
	a.AuthMode = "api-key"
	if _, err := Models(context.Background(), a); err == nil || called {
		t.Fatal("redirect forwarded credentials")
	}
	errorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401); _, _ = w.Write([]byte(a.Key)) }))
	defer errorServer.Close()
	a.BaseURL = errorServer.URL
	if _, err := Models(context.Background(), a); err == nil || strings.Contains(err.Error(), a.Key) {
		t.Fatal("secret in error")
	}
}

func TestDefaultApplyRestoreExactAndAbsentFiles(t *testing.T) {
	for _, kind := range []string{"codex", "claude"} {
		for _, existed := range []bool{false, true} {
			t.Run(kind+map[bool]string{true: "existing", false: "absent"}[existed], func(t *testing.T) {
				global := t.TempDir()
				t.Setenv("CODEX_HOME", global)
				t.Setenv("CLAUDE_CONFIG_DIR", global)
				path := toolcfg.ClaudeSettingsPath()
				original := []byte(`{"theme":"dark","env":{"ANTHROPIC_API_KEY":"original-key"}}`)
				if kind == "codex" {
					path = toolcfg.CodexConfigPath()
					original = []byte("# original\nmodel = \"original\"\n")
				}
				if existed {
					if err := os.WriteFile(path, original, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				s := New(t.TempDir())
				a := account(kind, "account", "new-key")
				if err := s.ApplyDefault(a); err != nil {
					t.Fatal(err)
				}
				a.Key = "second-key"
				if err := s.ApplyDefault(a); err != nil {
					t.Fatal(err)
				}
				if err := s.RestoreDefault(kind); err != nil {
					t.Fatal(err)
				}
				b, err := os.ReadFile(path)
				if existed && (err != nil || string(b) != string(original)) {
					t.Fatal("original not restored")
				}
				if !existed && !os.IsNotExist(err) {
					t.Fatal("new default file retained")
				}
				if kind == "codex" {
					if _, err := os.Stat(filepath.Join(global, "auth.json")); !os.IsNotExist(err) {
						t.Fatal("created auth.json not restored to absence")
					}
				}
			})
		}
	}
}

func TestRestoreNeverClobbersLaterUserEdits(t *testing.T) {
	global := t.TempDir()
	t.Setenv("CODEX_HOME", global)
	s := New(t.TempDir())
	if err := s.ApplyDefault(account("codex", "account", "key")); err != nil {
		t.Fatal(err)
	}
	path := toolcfg.CodexConfigPath()
	user := []byte("# later user modification\n")
	if err := os.WriteFile(path, user, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreDefault("codex"); err == nil {
		t.Fatal("restore did not detect user edit")
	}
	b, _ := os.ReadFile(path)
	if string(b) != string(user) {
		t.Fatal("user edit lost")
	}
	if _, err := os.Stat(s.snapshotPath("codex")); err != nil {
		t.Fatal("original snapshot lost")
	}
}

func TestVersionSorting(t *testing.T) {
	if !versionLess("app-1.9.0", "app-1.10.0") || versionLess("app-2.0.0", "app-1.99.0") {
		t.Fatal("version sorting")
	}
}
