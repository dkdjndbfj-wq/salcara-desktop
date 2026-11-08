package agents

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClaudeRuntimeSettingsPinsAPIWithoutDroppingHooks(t *testing.T) {
	for _, mode := range []string{"bearer", "api-key"} {
		t.Run(mode, func(t *testing.T) {
			s := Settings{ClaudeRoot: "http://127.0.0.1:12345/gateway/remote-claude/", ClaudeKey: "synthetic-api-runtime-key", ClaudeAuthMode: mode}
			const hooks = `{"hooks":{"Elicitation":[{"matcher":"*","hooks":[]}]}}`
			encoded, err := claudeRuntimeSettings(s, hooks)
			var settings map[string]json.RawMessage
			if err != nil || json.Unmarshal([]byte(encoded), &settings) != nil || len(settings) != 2 || settings["hooks"] == nil {
				t.Fatal("runtime settings lost hook or broadened unrelated settings")
			}
			var env map[string]string
			if json.Unmarshal(settings["env"], &env) != nil || env["ANTHROPIC_BASE_URL"] != strings.TrimRight(s.ClaudeRoot, "/") {
				t.Fatal("runtime API endpoint not pinned")
			}
			key, other := "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY"
			if mode == "api-key" {
				key, other = other, key
			}
			if env[key] != s.ClaudeKey || env[other] != "" {
				t.Fatal("credential mode not exclusive")
			}
			for _, name := range []string{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_USE_ANTHROPIC_AWS", "CLAUDE_CODE_USE_MANTLE", "ANTHROPIC_CUSTOM_HEADERS", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_PROFILE"} {
				if value, exists := env[name]; !exists || value != "" {
					t.Fatalf("competing routing field not cleared: %s", name)
				}
			}
		})
	}
}

func TestClaudeRuntimeSettingsRejectsNativeLoginFallback(t *testing.T) {
	const hooks = `{"hooks":{"Elicitation":[]}}`
	if _, err := claudeRuntimeSettings(Settings{}, hooks); err != errClaudeAPIRequired {
		t.Fatal("missing API fell back to native login")
	}
	if _, err := claudeRuntimeSettings(Settings{ClaudeRoot: "https://example.invalid"}, hooks); err == nil {
		t.Fatal("missing selected API key accepted")
	}
	for _, raw := range []string{"null", "[]", "{"} {
		if _, err := claudeRuntimeSettings(Settings{ClaudeRoot: "https://example.invalid", ClaudeKey: "synthetic"}, raw); err == nil {
			t.Fatal("invalid hook settings accepted")
		}
	}
}

func TestClaudeRuntimeSettingsFileCleanupIsExact(t *testing.T) {
	path, err := writeClaudeRuntimeSettings(`{"env":{"ANTHROPIC_AUTH_TOKEN":"synthetic"}}`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeClaudeRuntimeSettings(path) })
	data, err := os.ReadFile(path)
	if err != nil || !json.Valid(data) {
		t.Fatal("private settings missing")
	}
	other := filepath.Join(filepath.Dir(path), "unrelated.txt")
	if err := os.WriteFile(other, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(other); _ = os.Remove(filepath.Dir(path)) })
	removeClaudeRuntimeSettings(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("runtime settings survived cleanup")
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("cleanup removed unrelated data")
	}
}

func TestClaudeRuntimeSettingsSpawnPinsEnvironmentAndCleansOnExit(t *testing.T) {
	f := newFixture(t, "ask")
	for _, name := range []string{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_USE_MANTLE", "ANTHROPIC_CUSTOM_HEADERS"} {
		t.Setenv(name, "synthetic-old-provider")
	}
	a := f.m.Get("claude").(*claudeAgent)
	id := newUUID()
	p, err := a.spawn(context.Background(), id, f.workDir, "", "ask", false)
	if err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.procs[id] = p
	a.mu.Unlock()
	path := p.runtimeSettingsFile
	if path == "" || !filepath.IsAbs(path) {
		t.Fatal("missing runtime settings file")
	}
	var fields map[string]any
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &fields) != nil || fields["hooks"] == nil {
		t.Fatal("worker settings did not retain elicitation hook")
	}
	if strings.Contains(strings.Join(p.cmd.Args, " "), f.s.ClaudeKey) {
		t.Fatal("credential exposed in arguments")
	}
	env := map[string]string{}
	for _, item := range p.cmd.Env {
		name, value, _ := strings.Cut(item, "=")
		env[strings.ToUpper(name)] = value
	}
	for name, value := range claudePinnedAPIEnv(f.s) {
		if env[name] != value {
			t.Fatalf("worker differs from pinned settings: %s", name)
		}
	}
	for _, item := range p.cmd.Env {
		if strings.Contains(item, "synthetic-old-provider") {
			t.Fatal("worker inherited retired routing")
		}
	}
	a.stop(p)
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not exit")
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("worker left private runtime settings behind")
	}
}
