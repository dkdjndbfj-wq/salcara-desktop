package toolcfg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexMCPKeepsOtherContentAndReplacesOwnTable(t *testing.T) {
	in := "model = \"gpt-5\"\n# keep me\n\n[mcp_servers.github]\ncommand = \"old\"\n\n[mcp_servers.github.env]\nA = \"1\"\n\n[profiles.work]\nmodel = \"o3\"\n"
	out, err := CodexMCPTOML(in, []MCPServer{{Name: "github", Command: "npx", Args: []string{"-y", "@x/github"}, Env: map[string]string{"TOKEN": "t\"q"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"model = \"gpt-5\"", "# keep me", "[profiles.work]", "model = \"o3\"", "[mcp_servers.github]", "command = \"npx\"", "args = [\"-y\", \"@x/github\"]", "[mcp_servers.github.env]", "\"TOKEN\" = \"t\\\"q\""} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "command = \"old\"") || strings.Contains(out, "A = \"1\"") {
		t.Fatalf("old table kept:\n%s", out)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadCodexMCP(path)
	if err != nil {
		t.Fatal(err)
	}
	if s := got["github"]; s.Command != "npx" || len(s.Args) != 2 || s.Env["TOKEN"] != "t\"q" {
		t.Fatalf("round trip: %+v", s)
	}
	removed, err := CodexMCPTOML(out, nil, []string{"github"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(removed, "mcp_servers") || !strings.Contains(removed, "[profiles.work]") {
		t.Fatalf("remove:\n%s", removed)
	}
}

func TestCodexMCPRejectsUnsafeNames(t *testing.T) {
	if _, err := CodexMCPTOML("", []MCPServer{{Name: "a.b", Command: "x"}}, nil); err == nil {
		t.Fatal("dotted name accepted")
	}
	if _, err := CodexMCPTOML("", []MCPServer{{Name: "ok", Command: "x", URL: "https://x"}}, nil); err == nil {
		t.Fatal("command and url together accepted")
	}
	if _, err := CodexMCPTOML("", []MCPServer{{Name: "ok", URL: "file:///etc"}}, nil); err == nil {
		t.Fatal("non-http url accepted")
	}
}

func TestClaudeMCPEditsOnlyServers(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude.json")
	orig := `{"numStartups": 3, "mcpServers": {"mine": {"type": "stdio", "command": "keep", "extra": true}}}`
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ApplyClaudeMCP(path, []MCPServer{{Name: "remote", URL: "https://mcp.example.test/mcp"}}, nil); err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &root); err != nil {
		t.Fatal(err)
	}
	servers := root["mcpServers"].(map[string]any)
	if root["numStartups"].(float64) != 3 || servers["mine"].(map[string]any)["extra"] != true {
		t.Fatalf("unrelated data changed: %s", b)
	}
	if servers["remote"].(map[string]any)["type"] != "http" {
		t.Fatalf("remote: %s", b)
	}
	got, err := ReadClaudeMCP(path)
	if err != nil || got["remote"].URL != "https://mcp.example.test/mcp" {
		t.Fatalf("read: %+v %v", got, err)
	}
	before, _ := os.Stat(path)
	if err := ApplyClaudeMCP(path, []MCPServer{{Name: "remote", URL: "https://mcp.example.test/mcp"}}, []string{"absent"}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("unchanged servers rewrote the file")
	}
	if err := ApplyClaudeMCP(path, nil, []string{"remote"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadClaudeMCP(path); got["remote"].Name != "" || got["mine"].Command != "keep" {
		t.Fatalf("remove: %+v", got)
	}
}

func TestApplyMissingFilesCreatesThem(t *testing.T) {
	dir := t.TempDir()
	codex := filepath.Join(dir, "codex", "config.toml")
	if err := ApplyCodexMCP(codex, []MCPServer{{Name: "pw", Command: "npx", Args: []string{"@playwright/mcp@latest"}}}, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadCodexMCP(codex); err != nil || got["pw"].Command != "npx" {
		t.Fatalf("codex: %+v %v", got, err)
	}
	claude := filepath.Join(dir, ".claude.json")
	if err := ApplyClaudeMCP(claude, []MCPServer{{Name: "pw", Command: "npx"}}, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadClaudeMCP(claude); err != nil || got["pw"].Command != "npx" {
		t.Fatalf("claude: %+v %v", got, err)
	}
}
