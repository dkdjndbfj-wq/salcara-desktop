package agents

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// Explicit opt-in only. This does not load a user's CODEX_HOME/auth.json, inherit
// API credentials, open/resume/create a thread, or send a model request. It only
// initializes isolated app-server processes and reads their configuration.
func TestNativeCodexProviderOverrideProbe(t *testing.T) {
	probe := os.Getenv("SALCARA_NATIVE_CODEX_PROBE")
	if probe == "" {
		t.Skip("set SALCARA_NATIVE_CODEX_PROBE=1 or the native codex executable path to opt in")
	}
	exe := probe
	if probe == "1" {
		exe = nativeCodexExe(filepath.Join(os.Getenv("APPDATA"), "npm", "codex.cmd"))
	}
	if exe == "" {
		t.Fatal("native codex executable was not found")
	}
	if st, err := os.Stat(exe); err != nil || st.IsDir() {
		t.Fatal("native codex executable is unavailable")
	}
	home := t.TempDir()
	const provider = "original-isolated-probe"
	const content = `model_provider = "original-isolated-probe"
[model_providers.original-isolated-probe]
name = "Synthetic old provider"
base_url = "http://127.0.0.1:9/retired/v1"
wire_api = "responses"
requires_openai_auth = false
env_key = "SYNTHETIC_OLD_UNUSED_KEY"
http_headers = { "x-salcara-old-probe" = "synthetic-old-header" }
env_http_headers = { "x-salcara-old-env-probe" = "SYNTHETIC_OLD_HEADER_ENV" }
`
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	read := func(settings Settings, expectGuardRefusal bool) map[string]any {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.Command(exe, codexArgs(exe, settings)...)
		prepareCmd(cmd)
		cmd.Dir = home
		// Allow only OS runtime paths, never the calling process's credentials.
		for _, key := range []string{"SystemRoot", "WINDIR", "ComSpec", "PATH", "PATHEXT", "TEMP", "TMP", "APPDATA", "LOCALAPPDATA", "USERPROFILE"} {
			if value := os.Getenv(key); value != "" {
				cmd.Env = append(cmd.Env, key+"="+value)
			}
		}
		cmd.Env = append(cmd.Env, "CODEX_HOME="+home, "SUB2API_API_KEY=SYNTHETIC_LOOPBACK_TOKEN", "SYNTHETIC_OLD_HEADER_ENV=synthetic-env-header")
		conn, err := startRPC(cmd, func(string, json.RawMessage) {}, nil)
		if err != nil {
			t.Fatal("isolated native app-server could not start")
		}
		defer func() {
			conn.Kill()
			select {
			case <-conn.done:
			case <-time.After(5 * time.Second):
				t.Error("isolated native app-server did not exit")
			}
		}()
		if err := conn.Call(ctx, "initialize", map[string]any{
			"clientInfo":   map[string]any{"name": "salcara_isolated_config_probe", "version": "1"},
			"capabilities": map[string]any{"experimentalApi": false},
		}, nil); err != nil {
			t.Fatalf("isolated initialize failed: %v", err)
		}
		conn.Notify("initialized", map[string]any{})
		var response map[string]any
		if err := conn.Call(ctx, "config/read", map[string]any{"includeLayers": false}, &response); err != nil {
			t.Fatalf("isolated config/read failed: %v", err)
		}
		cfg, _ := response["config"].(map[string]any)
		providers, _ := cfg["model_providers"].(map[string]any)
		entry, _ := providers[provider].(map[string]any)
		if entry == nil {
			t.Fatal("config/read did not return the isolated provider table")
		}
		if needsCodexProviderGuard(settings) {
			err := validateCodexEffectiveProvider(ctx, conn, settings)
			if expectGuardRefusal {
				if err == nil || err.Error() != errCodexRemoteProvider {
					t.Fatalf("guard did not safely reject inherited synthetic authentication: %v", err)
				}
				t.Log("effective-provider guard refused the isolated inherited authentication before any thread RPC")
			} else {
				if err != nil {
					t.Fatalf("guard refused the isolated clean native configuration: %v", err)
				}
				t.Log("effective-provider guard accepted the isolated clean native configuration without a model request")
			}
		}
		keys := make([]string, 0, len(entry))
		for key := range entry {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		t.Logf("isolated provider configuration fields: %s", strings.Join(keys, ", "))
		return entry
	}
	before := read(Settings{}, false)
	settings := Settings{CodexRoot: "http://127.0.0.1:9/gateway/codex", CodexProvider: provider, CodexKey: "SYNTHETIC_LOOPBACK_TOKEN"}
	after := read(settings, true)
	if after["base_url"] != "http://127.0.0.1:9/gateway/codex/v1" || after["env_key"] != "SUB2API_API_KEY" {
		t.Fatal("native CLI did not apply the isolated provider URL/environment override")
	}
	retainedMarkers := 0
	for _, field := range []string{"http_headers", "env_http_headers", "experimental_bearer_token", "auth"} {
		if before[field] == nil {
			t.Logf("synthetic baseline does not configure field %s; inheritance of that field is unverified", field)
			continue
		}
		if field == "http_headers" || field == "env_http_headers" {
			oldHeaders, _ := before[field].(map[string]any)
			newHeaders, _ := after[field].(map[string]any)
			for key, marker := range oldHeaders {
				if newHeaders[key] == marker {
					retainedMarkers++
					t.Logf("native CLI retained old synthetic marker in provider field %s; effective-provider guard rejected it", field)
				}
			}
			continue
		}
		if after[field] != nil {
			t.Logf("native CLI retained old synthetic provider field %s; effective-provider guard rejected it", field)
		}
	}
	if retainedMarkers != 2 {
		t.Fatal("native fixture did not reproduce both inherited header markers; probe assumptions need review")
	}
	const cleanContent = `model_provider = "original-isolated-probe"
[model_providers.original-isolated-probe]
name = "Synthetic clean provider"
base_url = "http://127.0.0.1:9/retired/v1"
wire_api = "responses"
requires_openai_auth = false
env_key = "SYNTHETIC_OLD_UNUSED_KEY"
`
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(cleanContent), 0o600); err != nil {
		t.Fatal(err)
	}
	read(settings, false)
	if _, err := os.Stat(filepath.Join(home, "auth.json")); !os.IsNotExist(err) {
		t.Error("probe unexpectedly created an auth.json file")
	}
}
