package agents

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"salcara/bridge/internal/config"
)

type claudeAPIRequiredError struct{}

func (*claudeAPIRequiredError) Error() string {
	return "Claude 远程仅支持 API，请先选择有效的 API"
}
func (*claudeAPIRequiredError) ExecutionErrorCode() string { return "claude_api_required" }

var errClaudeAPIRequired = &claudeAPIRequiredError{}

// Claude workers never fall back to ambient settings or a saved native login.
// Validate before admission/retirement, so a missing API cannot stop an already
// accepted task merely because a new phone request tried to enter its worker.
func validateClaudeAPI(s Settings) error {
	if strings.TrimSpace(s.claudeRoot()) == "" || strings.TrimSpace(s.ClaudeKey) == "" {
		return errClaudeAPIRequired
	}
	if root, _, err := config.APIBase(s.claudeRoot()); err != nil || root == "" {
		return errClaudeAPIRequired
	}
	return nil
}

// Pin only the provider routing of a Bridge-owned API worker. Merely setting
// its environment is insufficient: Claude reapplies user/project settings.env
// on startup and reload. Other settings, hooks and managed policy stay intact.
// This is not an override of managed gateway sign-in or organization policy.
func claudeRuntimeSettings(s Settings, hookSettings string) (string, error) {
	if err := validateClaudeAPI(s); err != nil {
		return "", err
	}
	settings := map[string]any{}
	if hookSettings != "" {
		if err := json.Unmarshal([]byte(hookSettings), &settings); err != nil || settings == nil {
			return "", errors.New("Claude 进程配置无效")
		}
	}
	env := claudePinnedAPIEnv(s)
	settings["env"] = env
	encoded, err := json.Marshal(settings)
	if err != nil {
		return "", errors.New("无法生成 Claude 进程配置")
	}
	return string(encoded), nil
}

func claudePinnedAPIEnv(s Settings) map[string]string {
	env := map[string]string{
		"ANTHROPIC_BASE_URL": strings.TrimRight(s.claudeRoot(), "/"),
		"ANTHROPIC_API_KEY":  "", "ANTHROPIC_AUTH_TOKEN": "",
		"ANTHROPIC_CUSTOM_HEADERS":        "",
		"CLAUDE_CODE_OAUTH_TOKEN":         "",
		"CLAUDE_CODE_OAUTH_REFRESH_TOKEN": "", "CLAUDE_CODE_OAUTH_SCOPES": "",
		"CLAUDE_CODE_USE_BEDROCK": "", "CLAUDE_CODE_USE_VERTEX": "",
		"CLAUDE_CODE_USE_FOUNDRY": "", "CLAUDE_CODE_USE_ANTHROPIC_AWS": "",
		"CLAUDE_CODE_USE_MANTLE": "",
		"ANTHROPIC_PROFILE":      "", "ANTHROPIC_FEDERATION_RULE_ID": "",
		"ANTHROPIC_ORGANIZATION_ID": "",
	}
	if s.ClaudeAuthMode == "api-key" {
		env["ANTHROPIC_API_KEY"] = s.ClaudeKey
	} else {
		env["ANTHROPIC_AUTH_TOKEN"] = s.ClaudeKey
	}
	return env
}

// API credentials must not appear in --settings JSON arguments. The directory
// is private before contents are written (0600 alone is not an ACL on Windows).
// Cleanup removes exactly these two paths, never a recursively computed root.
func writeClaudeRuntimeSettings(contents string) (string, error) {
	dir, err := os.MkdirTemp("", "salcara-claude-runtime-")
	if err != nil {
		return "", errors.New("无法创建 Claude 进程配置目录")
	}
	if err := protectClaudeRuntimeDir(dir); err != nil {
		_ = os.Remove(dir)
		return "", errors.New("无法保护 Claude 进程配置目录")
	}
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		removeClaudeRuntimeSettings(path)
		return "", errors.New("无法保存 Claude 进程配置")
	}
	return path, nil
}

func removeClaudeRuntimeSettings(path string) {
	if path == "" {
		return
	}
	_ = os.Remove(path)
	_ = os.Remove(filepath.Dir(path)) // non-recursive; unrelated files survive
}

// WriteClaudeRuntimeSettings shares the protected file boundary with native
// launchers. The caller owns its lifetime, including cleanup on launch failure.
func WriteClaudeRuntimeSettings(contents string) (string, error) {
	return writeClaudeRuntimeSettings(contents)
}

// RemoveClaudeRuntimeSettings removes only a previously created runtime file
// and its empty parent directory. It never recursively deletes other content.
func RemoveClaudeRuntimeSettings(path string) {
	removeClaudeRuntimeSettings(path)
}
