package toolcfg

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// SwitchCodexTOML deliberately retains the provider ID. Codex indexes and filters
// threads by that ID; changing it can make retained history disappear from the UI.
func SwitchCodexTOML(content, base, key, model string) (string, error) {
	lines, eol := splitLines(content)
	provider, _, _, _ := InspectCodexTOML(content)
	info := scanTOML(lines)
	for i := 0; i < firstHeader(info); i++ {
		if info[i].kind == lkKeyVal && info[i].key == "profile" && stringValue(lines[i]) != "" {
			return "", errors.New("当前启用了 Codex 命名 profile，可能覆盖账号和供应商设置；为避免隐藏历史，请先在原工具中切回默认配置后再切号")
		}
		if info[i].kind == lkKeyVal && info[i].key == "forced_login_method" && stringValue(lines[i]) == "chatgpt" {
			return "", errors.New("原配置限制为 ChatGPT 登录，不能切换 API Key；请先按管理员要求调整登录模式")
		}
	}
	if provider == "ollama" || provider == "lmstudio" || provider == "bedrock" {
		return "", errors.New("当前是本地模型或 Bedrock 配置，不能在保留供应商标识的同时切到 API Key；请先在原工具中选用 API 模式")
	}
	if model != "" {
		lines = setTopLevel(lines, "model", TOMLString(model))
	}
	lines = setTopLevel(lines, "cli_auth_credentials_store", TOMLString("file"))
	if provider == "" || provider == "openai" {
		// Supported built-in override, not an attempted override of the reserved table.
		lines = setTopLevel(lines, "openai_base_url", TOMLString(base))
	} else {
		// Reject alternate token sources before modifying anything. Retain unrelated
		// request headers/retries and all project, plugin and permission settings.
		info := scanTOML(lines)
		var table []string
		for i, l := range info {
			if l.kind == lkHeader {
				table = l.header
			}
			if len(table) >= 3 && table[0] == "model_providers" && table[1] == provider && table[2] == "auth" {
				return "", errors.New("当前供应商使用命令获取凭据；请先移除该供应商的 auth 配置，再切换 API Key")
			}
			if l.kind == lkKeyVal && len(table) >= 2 && table[0] == "model_providers" && table[1] == provider {
				if len(table) == 2 && l.key == "auth" {
					return "", errors.New("当前供应商使用额外 auth 配置，请先在原工具中处理此凭据来源再切换")
				}
				headerTable := len(table) >= 3 && (table[2] == "http_headers" || table[2] == "env_http_headers")
				text := strings.ToLower(strings.Join(lines[i:l.stmtTo+1], "\n"))
				if (headerTable || strings.HasPrefix(l.key, "http_headers") || strings.HasPrefix(l.key, "env_http_headers")) && (strings.Contains(text, "authorization") || strings.Contains(text, "x-api-key")) {
					return "", errors.New("当前供应商额外配置了认证请求头，可能覆盖新 Key；请先在原工具中处理该请求头再切号")
				}
			}
		}
		// Use official auth.json, rather than competing inline/env credentials.
		for _, kv := range [][2]string{{"base_url", TOMLString(base)}, {"wire_api", TOMLString("responses")}, {"requires_openai_auth", "true"}, {"supports_websockets", "false"}} {
			lines = setProviderKey(lines, provider, kv[0], kv[1])
		}
		lines = removeProviderKey(lines, provider, "env_key")
		lines = removeProviderKey(lines, provider, "experimental_bearer_token")
	}
	return joinLines(lines, eol), nil
}

func providerBounds(lines []string, id string) (int, int) {
	info := scanTOML(lines)
	for i, l := range info {
		if l.kind == lkHeader && len(l.header) == 2 && l.header[0] == "model_providers" && l.header[1] == id {
			end := i + 1
			for end < len(info) && info[end].kind != lkHeader {
				end++
			}
			return i, end
		}
	}
	return -1, -1
}

func setProviderKey(lines []string, id, key, value string) []string {
	start, end := providerBounds(lines, id)
	if start < 0 {
		return append(lines, "", "[model_providers."+TOMLString(id)+"]", "name = "+TOMLString(id), key+" = "+value)
	}
	info := scanTOML(lines)
	for i := start + 1; i < end; i++ {
		if info[i].kind == lkKeyVal && info[i].key == key {
			out := append([]string{}, lines[:i]...)
			out = append(out, key+" = "+value)
			return append(out, lines[info[i].stmtTo+1:]...)
		}
	}
	out := append([]string{}, lines[:start+1]...)
	out = append(out, key+" = "+value)
	return append(out, lines[start+1:]...)
}

func removeProviderKey(lines []string, id, key string) []string {
	start, end := providerBounds(lines, id)
	if start < 0 {
		return lines
	}
	info := scanTOML(lines)
	for i := start + 1; i < end; i++ {
		if info[i].kind == lkKeyVal && info[i].key == key {
			return append(append([]string{}, lines[:i]...), lines[info[i].stmtTo+1:]...)
		}
	}
	return lines
}

// No side backups here: the switch transaction captures exact originals and rolls
// back both files together. It never reads or writes session/SQLite/project data.
func ApplySwitchCodex(dir, base, key, model string) error {
	path := filepath.Join(dir, "config.toml")
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	out, err := SwitchCodexTOML(string(b), base, key, model)
	if err != nil {
		return err
	}
	if err := writeFileKeepMode(path, []byte(out), 0o600); err != nil {
		return err
	}
	auth, _ := json.MarshalIndent(map[string]any{"auth_mode": "apikey", "OPENAI_API_KEY": key}, "", "  ")
	return writeFileKeepMode(filepath.Join(dir, "auth.json"), append(auth, '\n'), 0o600)
}

const SwitchClaudeProviderID = "salcara-bridge-current"

// ApplySwitchClaudeDesktop must not change deployment mode or the visible chat store.
func ApplySwitchClaudeDesktop(dir, name, base, key, mode, model string) error {
	return ErrClaudeDesktopAutomaticAPIUnavailable
}
