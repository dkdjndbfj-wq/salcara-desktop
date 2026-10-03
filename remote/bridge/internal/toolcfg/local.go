package toolcfg

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// ApplyLocalCodex also writes official API-key authentication so the desktop
// client recognizes a signed-in API account. Originals are backed up.
func ApplyLocalCodex(dir, base, key, model string) error {
	if err := ApplyCodex(filepath.Join(dir, "config.toml"), CodexParams{BaseURL: base, Token: key, Model: model}); err != nil {
		return err
	}
	path := filepath.Join(dir, "auth.json")
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := backupOnce(path, b, err == nil); err != nil {
		return err
	}
	out, _ := json.MarshalIndent(map[string]any{"auth_mode": "apikey", "OPENAI_API_KEY": key}, "", "  ")
	return writeFileKeepMode(path, append(out, '\n'), 0o600)
}

func RestoreLocalCodexAuth(dir string) error {
	path := filepath.Join(dir, "auth.json")
	if ok, err := restoreBackup(path); ok || err != nil {
		return err
	}
	return nil
}

func ApplyLocalClaude(path, base, key, model, authMode string) error {
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	root, err := parseOrdered(b)
	if err != nil {
		return err
	}
	env, err := envOf(root)
	if err != nil {
		return err
	}
	env.set(EnvBaseURL, jsonStr(base))
	env.set(EnvNoTraffic, jsonStr("1"))
	env.set("CLAUDE_CODE_OAUTH_TOKEN", jsonStr(""))
	// Clear the other authentication source so old keys cannot win a switch.
	if authMode == "api-key" {
		env.set("ANTHROPIC_API_KEY", jsonStr(key))
		env.set(EnvAuthToken, jsonStr(""))
	} else {
		env.set(EnvAuthToken, jsonStr(key))
		env.set("ANTHROPIC_API_KEY", jsonStr(""))
	}
	if model != "" {
		root.set("model", jsonStr(model))
		env.set("ANTHROPIC_MODEL", jsonStr(model))
	}
	eb, err := env.marshal()
	if err != nil {
		return err
	}
	root.set("env", eb)
	out, err := root.marshal()
	if err != nil {
		return err
	}
	if err := backupOnce(path, b, b != nil); err != nil {
		return err
	}
	return writeFileKeepMode(path, out, 0o600)
}

// WriteClaudeDesktopProfile is disabled until the native 3P config contract is supported.
func WriteClaudeDesktopProfile(dir, id, name, base, key, authMode string, models []string) error {
	return ErrClaudeDesktopAutomaticAPIUnavailable
}
