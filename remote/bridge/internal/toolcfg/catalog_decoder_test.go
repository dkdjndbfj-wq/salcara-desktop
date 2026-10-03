package toolcfg

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Opt-in compatibility check against an installed CLI. The child has no user
// configuration or credentials, and every configured provider is a rejecting
// local test server. It only decodes model_catalog_json; it cannot run a turn.
func TestCodexCatalogOfflineDecoder(t *testing.T) {
	exe := os.Getenv("SALCARA_VERIFY_CODEX_CATALOG_EXE")
	if exe == "" {
		t.Skip("set SALCARA_VERIFY_CODEX_CATALOG_EXE for the offline decoder check")
	}
	home := t.TempDir()
	data, err := CodexCatalogJSON(home, []string{"salcara-unknown-relay-probe"}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "probe-models.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(http.StatusForbidden) }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "debug", "models",
		"-c", "model_catalog_json="+TOMLString(path),
		"-c", "model_provider=salcara_catalog_probe",
		"-c", `model_providers.salcara_catalog_probe={ name="Offline decoder probe", base_url=`+TOMLString(server.URL)+`, wire_api="responses", requires_openai_auth=false }`,
		"-c", "openai_base_url="+TOMLString(server.URL), "-c", "chatgpt_base_url="+TOMLString(server.URL))
	// Do not pass account/auth environment variables, even for an opt-in test.
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		switch strings.ToUpper(key) {
		case "PATH", "SYSTEMROOT", "WINDIR", "TEMP", "TMP", "PATHEXT":
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "CODEX_HOME="+home, "USERPROFILE="+home, "HOME="+home,
		"OPENAI_BASE_URL="+server.URL, "CHATGPT_BASE_URL="+server.URL)
	var diagnostics bytes.Buffer
	cmd.Stderr = &diagnostics
	output, err := cmd.Output()
	if requests.Load() != 0 {
		t.Fatal("offline catalog decoding attempted a provider request")
	}
	if err != nil {
		t.Fatalf("installed Codex rejected the local catalog: %v\n%s", err, diagnostics.String())
	}
	var decoded struct {
		Models []map[string]any `json:"models"`
	}
	if json.Unmarshal(output, &decoded) != nil || len(decoded.Models) != 1 || decoded.Models[0]["slug"] != "salcara-unknown-relay-probe" {
		t.Fatalf("debug decoder did not return the exact local catalog: %s", output)
	}
	entry := decoded.Models[0]
	if entry["context_window"] != nil || entry["max_context_window"] != nil || entry["default_reasoning_level"] != nil || len(entry["supported_reasoning_levels"].([]any)) != 0 {
		t.Fatal("installed Codex invented context or reasoning capacity")
	}
	modalities, ok := entry["input_modalities"].([]any)
	if !ok || len(modalities) != 1 || modalities[0] != "text" || entry["supports_image_detail_original"] != false || entry["multi_agent_version"] != "disabled" {
		t.Fatal("installed Codex expanded unknown relay capabilities")
	}
	t.Logf("local catalog accepted by installed Codex; provider requests=%d", requests.Load())
}
