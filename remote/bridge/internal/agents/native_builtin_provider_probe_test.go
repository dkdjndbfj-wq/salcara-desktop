package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// Opt-in reads effective configuration in a fresh synthetic CODEX_HOME only.
// It does not read user credentials, create/resume a thread or run inference.
func TestNativeCodexBuiltinProviderOverrideProbe(t *testing.T) {
	probe := os.Getenv("SALCARA_NATIVE_CODEX_PROBE")
	if probe == "" {
		t.Skip("isolated native configuration probe requires explicit opt-in")
	}
	exe := probe
	if probe == "1" {
		exe = nativeCodexExe(filepath.Join(os.Getenv("APPDATA"), "npm", "codex.cmd"))
	}
	if info, err := os.Stat(exe); err != nil || !info.Mode().IsRegular() {
		t.Fatal("native codex executable is unavailable")
	}
	home := t.TempDir()
	const oldAuth = `{"auth_mode":"apikey","OPENAI_API_KEY":"SYNTHETIC_OLD_NOT_SELECTED_KEY"}`
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(oldAuth), 0o600); err != nil {
		t.Fatal(err)
	}
	const content = `model_provider = "old-synthetic-provider"
openai_base_url = "http://127.0.0.1:9/old/v1"
[model_providers.old-synthetic-provider]
name = "Synthetic provider"
base_url = "http://127.0.0.1:9/retired/v1"
wire_api = "responses"
requires_openai_auth = false
env_key = "SYNTHETIC_UNUSED_KEY"
`
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	s := Settings{CodexRoot: "http://127.0.0.1:9/gateway/remote-codex", CodexProvider: "openai", CodexKey: "SYNTHETIC_SCOPED_KEY"}
	cmd := exec.Command(exe, codexArgs(exe, s)...)
	prepareCmd(cmd)
	cmd.Dir = home
	for _, key := range []string{"SystemRoot", "WINDIR", "ComSpec", "PATH", "PATHEXT", "TEMP", "TMP"} {
		if value := os.Getenv(key); value != "" {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	cmd.Env = append(cmd.Env, "CODEX_HOME="+home, "HOME="+home, "USERPROFILE="+home, "APPDATA="+home, "LOCALAPPDATA="+home,
		"OPENAI_API_KEY=SYNTHETIC_DECOY_KEY")
	conn, err := startRPC(cmd, func(string, json.RawMessage) {}, nil)
	if err != nil {
		t.Fatal("isolated native app-server failed to start")
	}
	defer func() {
		conn.Kill()
		select {
		case <-conn.done:
		case <-time.After(5 * time.Second):
			t.Error("isolated native app-server did not close")
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if conn.Call(ctx, "initialize", map[string]any{"clientInfo": map[string]any{"name": "salcara_synthetic_builtin_probe", "version": "1"}}, nil) != nil {
		t.Fatal("isolated initialization failed")
	}
	if conn.Notify("initialized", map[string]any{}) != nil {
		t.Fatal("isolated initialization acknowledgement failed")
	}
	var response map[string]any
	if conn.Call(ctx, "config/read", map[string]any{"includeLayers": false}, &response) != nil {
		t.Fatal("isolated effective configuration was unavailable")
	}
	config, ok := response["config"].(map[string]any)
	if !ok || config["model_provider"] != "openai" {
		t.Fatal("builtin worker did not select the requested provider")
	}
	keys := make([]string, 0, len(config))
	for key := range config {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	t.Logf("synthetic effective configuration fields: %v", keys)
	if config["openai_base_url"] != s.CodexRoot+"/v1" {
		t.Fatal("builtin provider URL could not be verified in effective configuration")
	}
	if config["cli_auth_credentials_store"] != "ephemeral" || config["forced_login_method"] != "api" {
		t.Fatal("synthetic process-only authentication configuration was not applied")
	}
	if validateCodexEffectiveProvider(ctx, conn, s) != nil || initializeCodexBuiltinAuthentication(ctx, conn, s) != nil {
		t.Fatal("synthetic process-only API login failed")
	}
	var auth struct {
		Account *struct {
			Type string `json:"type"`
		} `json:"account"`
		RequiresOpenaiAuth bool `json:"requiresOpenaiAuth"`
	}
	if err := conn.Call(ctx, "account/read", map[string]any{"refreshToken": false}, &auth); err != nil {
		t.Fatal("synthetic account status was unavailable")
	}
	if auth.Account == nil || auth.Account.Type != "apiKey" || !auth.RequiresOpenaiAuth {
		t.Fatal("builtin worker did not use the synthetic process-scoped API authentication")
	}
	stored, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil || !bytes.Equal(stored, []byte(oldAuth)) {
		t.Fatal("process-only authentication modified the original synthetic login")
	}
}
