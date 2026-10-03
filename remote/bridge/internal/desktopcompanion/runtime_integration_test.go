package desktopcompanion

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// This test exercises the real probe source and runtime, but never a real
// user's configuration or desktop. The default tool call has no host metadata,
// and the child receives no inherited Codex signals or Node startup overrides.
func TestInstalledProbeRuntimeIsolatedStdio(t *testing.T) {
	bundle := os.Getenv("SALCARA_TEST_COMPANION_BUNDLE")
	if bundle == "" {
		bundle = filepath.Join("..", "..", "..", "desktop-companion")
	}
	var err error
	bundle, err = filepath.Abs(bundle)
	if err != nil {
		t.Fatal("could not resolve test bundle")
	}
	node := os.Getenv("SALCARA_TEST_PROBE_NODE")
	if node == "" {
		node, err = exec.LookPath("node")
		if err != nil {
			t.Skip("Node runtime unavailable for isolated integration test")
		}
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal("could not resolve test Node runtime")
	}

	const fixtureKey = "runtime-fixture-secret-key-do-not-output"
	original := "# retain exact original fixture bytes\r\nmodel = \"fixture-model\"\r\nmodel_provider = \"fixture-relay\"\r\n[model_providers.fixture-relay]\r\nbase_url = \"https://fixture.invalid/v1\"\r\nexperimental_bearer_token = \"" + fixtureKey + "\"\r\n"
	s, o := fixture(t, original)
	s.o.BundleDir, s.o.NodePath = bundle, node
	retained := map[string]string{
		"auth.json":                    `{"api_key":"isolated-auth-fixture"}`,
		"history.jsonl":                "isolated-history-fixture\n",
		"sessions/original-chat.jsonl": "isolated-existing-session-fixture\n",
	}
	for name, content := range retained {
		path := filepath.Join(o.CodexHome, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal("could not prepare isolated session fixtures")
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal("could not write isolated session fixtures")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	status, err := s.Install(ctx)
	if err != nil || !status.Installed || status.CatalogOnly {
		t.Fatalf("isolated installation failed: %v", err)
	}
	installedConfig := configBytes(t, o)
	if !bytes.HasPrefix(installedConfig, []byte(original)) {
		t.Fatal("installation rewrote original fixture configuration")
	}
	var config map[string]any
	if err := toml.Unmarshal(installedConfig, &config); err != nil {
		t.Fatal("installed fixture configuration is invalid")
	}
	servers, ok := config["mcp_servers"].(map[string]any)
	if !ok {
		t.Fatal("installed probe entry missing")
	}
	entry, ok := servers[ServerName].(map[string]any)
	if !ok {
		t.Fatal("installed probe entry invalid")
	}
	command, commandOK := entry["command"].(string)
	cwd, cwdOK := entry["cwd"].(string)
	args, argsOK := entry["args"].([]any)
	if !commandOK || !cwdOK || !argsOK || len(args) != 1 || !filepath.IsAbs(command) || !filepath.IsAbs(cwd) {
		t.Fatal("installed runtime command is not a fixed absolute entry")
	}
	index, ok := args[0].(string)
	if !ok || !filepath.IsAbs(index) || index != filepath.Join(o.DataDir, "desktop-companion", "v"+Version, "src", "index.mjs") {
		t.Fatal("runtime did not use copied isolated probe entrypoint")
	}

	requests := []map[string]any{
		{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "isolated-installer-test", "version": "1"}}},
		{"jsonrpc": "2.0", "method": "notifications/initialized"},
		{"jsonrpc": "2.0", "id": 2, "method": "tools/list"},
		{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{"name": ServerName, "arguments": map[string]any{}}},
	}
	var input bytes.Buffer
	for _, request := range requests {
		if err := json.NewEncoder(&input).Encode(request); err != nil {
			t.Fatal("could not encode isolated protocol fixture")
		}
	}
	cmd := exec.CommandContext(ctx, command, index)
	cmd.Dir = cwd
	cmd.Env = isolatedProbeRuntimeEnv()
	for _, setting := range cmd.Env {
		name := strings.SplitN(setting, "=", 2)[0]
		if strings.HasPrefix(strings.ToUpper(name), "CODEX_") || strings.HasPrefix(strings.ToUpper(name), "NODE_") {
			t.Fatal("test child received host signals or Node overrides")
		}
	}
	cmd.Stdin = &input
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("isolated probe runtime failed: %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatal("probe emitted unexpected stderr")
	}
	if bytes.Contains(stdout.Bytes(), []byte(fixtureKey)) || bytes.Contains(stdout.Bytes(), []byte(o.CodexHome)) {
		t.Fatal("probe stdout leaked fixture credentials or configuration path")
	}

	decoder := json.NewDecoder(&stdout)
	var responses []map[string]any
	for {
		var response map[string]any
		if err := decoder.Decode(&response); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal("probe stdout contained non-JSON-RPC output")
		}
		responses = append(responses, response)
	}
	if len(responses) != 3 {
		t.Fatal("expected exactly initialize, catalog, and default probe responses")
	}
	for i, response := range responses {
		if response["jsonrpc"] != "2.0" || response["id"] != float64(i+1) || response["error"] != nil {
			t.Fatal("probe response envelope was invalid")
		}
	}
	catalog, ok := responses[1]["result"].(map[string]any)
	if !ok {
		t.Fatal("probe tool catalog response missing")
	}
	tools, ok := catalog["tools"].([]any)
	if !ok || len(tools) != 2 {
		t.Fatal("companion did not advertise exactly the probe and approved connection tools")
	}
	tool, ok := tools[0].(map[string]any)
	if !ok || tool["name"] != ServerName {
		t.Fatal("probe advertised an unexpected tool")
	}
	connectTool,ok:=tools[1].(map[string]any)
	if !ok||connectTool["name"]!=ConnectTool {t.Fatal("companion connection tool missing")}
	call, ok := responses[2]["result"].(map[string]any)
	if !ok || call["isError"] != false {
		t.Fatal("default probe call did not succeed")
	}
	result, ok := call["structuredContent"].(map[string]any)
	if !ok {
		t.Fatal("default probe structured output missing")
	}
	for _, field := range []string{"desktopControl", "remoteSend", "hostEnvironmentPresent", "hostEnvironmentMatches", "hostMetadataPresent", "catalogRequested", "catalogAttempted", "catalogAvailable"} {
		if result[field] != false {
			t.Fatalf("default isolated probe unexpectedly enabled %s", field)
		}
	}
	if result["probeVersion"] != Version || result["errorCode"] != "NONE" {
		t.Fatal("default probe returned an unexpected version or error")
	}
	if !bytes.Equal(configBytes(t, o), installedConfig) {
		t.Fatal("probe runtime changed installed fixture configuration")
	}
	for name, originalContent := range retained {
		current, err := os.ReadFile(filepath.Join(o.CodexHome, filepath.FromSlash(name)))
		if err != nil || string(current) != originalContent {
			t.Fatal("installation or runtime changed original auth/session fixture")
		}
	}
}

func isolatedProbeRuntimeEnv() []string {
	// Do not inherit PATH, CODEX_HOME, pipe/resources variables, or NODE_OPTIONS.
	// An absolute runtime path removes the need for a child PATH. Windows still
	// needs its OS root, and both platforms may need a temporary directory.
	keys := []string{"TEMP", "TMP", "TMPDIR"}
	if runtime.GOOS == "windows" {
		keys = append(keys, "SystemRoot", "WINDIR")
	}
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}
