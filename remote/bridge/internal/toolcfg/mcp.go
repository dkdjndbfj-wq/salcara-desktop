package toolcfg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

// MCPServer is one MCP server definition in the shape both Codex and Claude Code understand:
// a local command (stdio) or a remote URL (streamable HTTP).
type MCPServer struct {
	Name    string            `json:"name"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
}

var mcpNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidMCPName reports whether name is safe as a TOML bare key and a JSON object key.
func ValidMCPName(name string) bool { return mcpNameRe.MatchString(name) }

// Validate checks the definition before it is written into any tool's configuration.
func (s MCPServer) Validate() error {
	if !ValidMCPName(s.Name) {
		return errors.New("名称只能包含字母、数字、- 和 _，最多 64 个字符")
	}
	hasCommand, hasURL := strings.TrimSpace(s.Command) != "", strings.TrimSpace(s.URL) != ""
	if hasCommand == hasURL {
		return errors.New("请填写启动命令或服务地址中的一个")
	}
	if hasURL && !strings.HasPrefix(s.URL, "https://") && !strings.HasPrefix(s.URL, "http://") {
		return errors.New("服务地址必须以 http:// 或 https:// 开头")
	}
	for k := range s.Env {
		if k == "" || strings.ContainsAny(k, "=\x00\r\n") {
			return errors.New("环境变量名称无效")
		}
	}
	return nil
}

// ---- Codex: ~/.codex/config.toml [mcp_servers.<name>] ----

type codexMCPFile struct {
	MCPServers map[string]struct {
		Command string            `toml:"command"`
		Args    []string          `toml:"args"`
		Env     map[string]string `toml:"env"`
		URL     string            `toml:"url"`
	} `toml:"mcp_servers"`
}

// ReadCodexMCP lists the MCP servers declared in a Codex config.toml. A missing file is empty.
func ReadCodexMCP(path string) (map[string]MCPServer, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]MCPServer{}, nil
	}
	if err != nil {
		return nil, err
	}
	var f codexMCPFile
	if err := toml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("config.toml 格式有误: %w", err)
	}
	out := make(map[string]MCPServer, len(f.MCPServers))
	for name, s := range f.MCPServers {
		out[name] = MCPServer{Name: name, Command: s.Command, Args: s.Args, Env: s.Env, URL: s.URL}
	}
	return out, nil
}

func isMCPTable(h []string, name string) bool {
	return len(h) >= 2 && h[0] == "mcp_servers" && h[1] == name
}

// removeCodexMCP deletes [mcp_servers.<name>] and its sub-tables, keeping everything else verbatim.
func removeCodexMCP(lines []string, name string) []string {
	info := scanTOML(lines)
	var out []string
	for i := 0; i < len(lines); {
		if info[i].kind == lkHeader && isMCPTable(info[i].header, name) {
			end := i + 1
			for end < len(lines) && info[end].kind != lkHeader {
				end++
			}
			i = end
			continue
		}
		out = append(out, lines[i])
		i++
	}
	return out
}

func tomlStringArray(values []string) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = TOMLString(v)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func codexMCPBlock(s MCPServer) []string {
	block := []string{"", "[mcp_servers." + s.Name + "]"}
	if s.URL != "" {
		block = append(block, "url = "+TOMLString(s.URL))
	} else {
		block = append(block, "command = "+TOMLString(s.Command))
		if len(s.Args) > 0 {
			block = append(block, "args = "+tomlStringArray(s.Args))
		}
	}
	if len(s.Env) > 0 {
		keys := make([]string, 0, len(s.Env))
		for k := range s.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		block = append(block, "", "[mcp_servers."+s.Name+".env]")
		for _, k := range keys {
			block = append(block, TOMLString(k)+" = "+TOMLString(s.Env[k]))
		}
	}
	return block
}

// CodexMCPTOML rewrites only the named [mcp_servers.*] tables: upserts replace a table in place
// order-wise at the end of the file; removals drop the table. Other content stays byte-for-byte.
func CodexMCPTOML(content string, upsert []MCPServer, remove []string) (string, error) {
	lines, eol := splitLines(content)
	for _, name := range remove {
		if !ValidMCPName(name) {
			return "", fmt.Errorf("MCP 名称无效: %q", name)
		}
		lines = removeCodexMCP(lines, name)
	}
	for _, s := range upsert {
		if err := s.Validate(); err != nil {
			return "", err
		}
		lines = removeCodexMCP(lines, s.Name)
		lines = append(lines, codexMCPBlock(s)...)
	}
	out := joinLines(lines, eol)
	var probe map[string]any
	if err := toml.Unmarshal([]byte(out), &probe); err != nil {
		return "", fmt.Errorf("写入后 config.toml 无法解析，已放弃修改: %w", err)
	}
	return out, nil
}

// ApplyCodexMCP edits config.toml at path; it does nothing when the result is unchanged.
func ApplyCodexMCP(path string, upsert []MCPServer, remove []string) error {
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	existed := err == nil && len(b) > 0
	out, err := CodexMCPTOML(string(b), upsert, remove)
	if err != nil {
		return err
	}
	if out == string(b) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := backupOnce(path, b, existed); err != nil {
		return err
	}
	return writeFileKeepMode(path, []byte(out), 0o600)
}

// ---- Claude Code: ~/.claude.json "mcpServers" (user scope) ----

// ClaudeUserConfigPath is the Claude Code user config (~/.claude.json, or inside CLAUDE_CONFIG_DIR).
func ClaudeUserConfigPath() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, ".claude.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude.json")
}

type claudeMCPEntry struct {
	Type    string            `json:"type,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
}

func readClaudeRoot(path string) (map[string]json.RawMessage, []byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]json.RawMessage{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	root := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, &root); err != nil {
			return nil, nil, fmt.Errorf(".claude.json 格式有误，未修改: %w", err)
		}
	}
	return root, b, nil
}

// ReadClaudeMCP lists the user-scope MCP servers of Claude Code.
func ReadClaudeMCP(path string) (map[string]MCPServer, error) {
	root, _, err := readClaudeRoot(path)
	if err != nil {
		return nil, err
	}
	out := map[string]MCPServer{}
	raw, ok := root["mcpServers"]
	if !ok {
		return out, nil
	}
	entries := map[string]claudeMCPEntry{}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf(".claude.json 里的 mcpServers 格式有误: %w", err)
	}
	for name, e := range entries {
		out[name] = MCPServer{Name: name, Command: e.Command, Args: e.Args, Env: e.Env, URL: e.URL}
	}
	return out, nil
}

// ApplyClaudeMCP edits only the "mcpServers" object; unknown fields of each server it does not
// manage are kept as they were.
func ApplyClaudeMCP(path string, upsert []MCPServer, remove []string) error {
	root, orig, err := readClaudeRoot(path)
	if err != nil {
		return err
	}
	servers := map[string]json.RawMessage{}
	if raw, ok := root["mcpServers"]; ok {
		if err := json.Unmarshal(raw, &servers); err != nil {
			return fmt.Errorf(".claude.json 里的 mcpServers 格式有误，未修改: %w", err)
		}
	}
	changed := false
	for _, name := range remove {
		if _, ok := servers[name]; ok {
			delete(servers, name)
			changed = true
		}
	}
	for _, s := range upsert {
		if err := s.Validate(); err != nil {
			return err
		}
		e := claudeMCPEntry{Type: "stdio", Command: s.Command, Args: s.Args, Env: s.Env}
		if s.URL != "" {
			e = claudeMCPEntry{Type: "http", URL: s.URL, Env: s.Env}
		}
		raw, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if old, ok := servers[s.Name]; ok {
			var compact bytes.Buffer
			if json.Compact(&compact, old) == nil && bytes.Equal(compact.Bytes(), raw) {
				continue
			}
		}
		servers[s.Name] = raw
		changed = true
	}
	if !changed {
		return nil
	}
	raw, err := json.Marshal(servers)
	if err != nil {
		return err
	}
	root["mcpServers"] = raw
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := backupOnce(path, orig, len(orig) > 0); err != nil {
		return err
	}
	return writeFileKeepMode(path, out, 0o600)
}
