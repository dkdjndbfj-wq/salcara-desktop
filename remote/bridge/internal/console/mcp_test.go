package console

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"salcara/bridge/internal/toolcfg"
)

func mcpFixture(t *testing.T) (codex, claude string) {
	t.Helper()
	dir := t.TempDir()
	codex, claude = filepath.Join(dir, "codex", "config.toml"), filepath.Join(dir, ".claude.json")
	oldReg, oldPaths := mcpRegistryPath, mcpAgentPaths
	mcpRegistryPath = func() (string, error) { return filepath.Join(dir, "bridge", "mcp-servers.json"), nil }
	mcpAgentPaths = func() map[string]string { return map[string]string{"codex": codex, "claude": claude} }
	t.Cleanup(func() { mcpRegistryPath, mcpAgentPaths = oldReg, oldPaths })
	return codex, claude
}

func mcpCall(t *testing.T, h http.HandlerFunc, method, body string) (int, mcpListResponse) {
	t.Helper()
	req := httptest.NewRequest(method, "/api/mcp", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h(rec, req)
	var resp mcpListResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, resp
}

func TestMCPSaveSyncsBothAgentsAndToggleRemovesOnlyManaged(t *testing.T) {
	codex, claude := mcpFixture(t)
	if err := os.MkdirAll(filepath.Dir(codex), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codex, []byte("model = \"gpt-5\"\n\n[mcp_servers.userown]\ncommand = \"mine\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	code, resp := mcpCall(t, s.handleMCPSave, "POST", `{"server":{"name":"pw","command":"npx","args":["@playwright/mcp@latest"],"enabled":true}}`)
	if code != 200 || len(resp.Servers) != 1 || !resp.Servers[0].Agents["claude"] {
		t.Fatalf("save: %d %+v", code, resp)
	}
	if len(resp.Discovered) != 1 || resp.Discovered[0].Name != "userown" {
		t.Fatalf("discovered: %+v", resp.Discovered)
	}
	if got, _ := toolcfg.ReadCodexMCP(codex); got["pw"].Command != "npx" || got["userown"].Command != "mine" {
		t.Fatalf("codex: %+v", got)
	}
	if got, _ := toolcfg.ReadClaudeMCP(claude); got["pw"].Command != "npx" {
		t.Fatalf("claude: %+v", got)
	}
	code, _ = mcpCall(t, s.handleMCPToggle, "POST", `{"name":"pw","enabled":false}`)
	if code != 200 {
		t.Fatalf("toggle: %d", code)
	}
	if got, _ := toolcfg.ReadCodexMCP(codex); got["pw"].Name != "" || got["userown"].Command != "mine" {
		t.Fatalf("after toggle codex: %+v", got)
	}
	code, _ = mcpCall(t, s.handleMCPToggle, "POST", `{"name":"pw","enabled":true,"agent":"claude"}`)
	if code != 200 {
		t.Fatalf("agent toggle: %d", code)
	}
	code, resp = mcpCall(t, s.handleMCPImport, "POST", `{}`)
	if code != 200 || len(resp.Servers) != 2 || len(resp.Discovered) != 0 {
		t.Fatalf("import: %d %+v", code, resp)
	}
	if got, _ := toolcfg.ReadClaudeMCP(claude); got["userown"].Command != "mine" {
		t.Fatalf("import did not share with claude: %+v", got)
	}
	code, resp = mcpCall(t, s.handleMCPRemove, "POST", `{"name":"userown"}`)
	if code != 200 || len(resp.Servers) != 1 {
		t.Fatalf("remove: %d %+v", code, resp)
	}
	if b, _ := os.ReadFile(codex); strings.Contains(string(b), "userown") || !strings.Contains(string(b), "model = \"gpt-5\"") {
		t.Fatalf("codex after remove:\n%s", b)
	}
}

func TestMCPSaveRejectsDuplicatesAndBadInput(t *testing.T) {
	mcpFixture(t)
	s := &Server{}
	if code, _ := mcpCall(t, s.handleMCPSave, "POST", `{"server":{"name":"a b","command":"x"}}`); code != 400 {
		t.Fatalf("bad name: %d", code)
	}
	if code, _ := mcpCall(t, s.handleMCPSave, "POST", `{"server":{"name":"a","command":"x","enabled":true}}`); code != 200 {
		t.Fatalf("first: %d", code)
	}
	if code, _ := mcpCall(t, s.handleMCPSave, "POST", `{"server":{"name":"a","command":"y"}}`); code != 409 {
		t.Fatalf("duplicate: %d", code)
	}
	if code, resp := mcpCall(t, s.handleMCPSave, "POST", `{"previousName":"a","server":{"name":"b","url":"https://x.test/mcp","enabled":true}}`); code != 200 || len(resp.Servers) != 1 || resp.Servers[0].Name != "b" {
		t.Fatalf("rename: %d %+v", code, resp)
	}
}
