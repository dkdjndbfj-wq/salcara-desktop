// Package desktoplink only navigates to existing chats via documented native deep links.
// It is not a desktop conversation executor.
package desktoplink

import (
	"errors"
	"regexp"
	"runtime"

	"salcara/bridge/internal/autostart"
)

var codexKey = regexp.MustCompile(`^codex:[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

// CodexURL accepts only a canonical thread UUID, never a caller-supplied URL or query.
func CodexURL(sessionKey string) (string, error) {
	if !codexKey.MatchString(sessionKey) {
		return "", errors.New("桌面定位仅支持已有 Codex 会话的标准 UUID")
	}
	return "codex://threads/" + sessionKey[len("codex:"):], nil
}

var claudeKey = regexp.MustCompile(`^claude:[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

// ClaudeKey reports whether sessionKey is a canonical Claude Code session key.
func ClaudeKey(sessionKey string) bool { return claudeKey.MatchString(sessionKey) }

// ClaudeURL is claude:// (bring Claude Desktop forward) or, for a session
// Claude Desktop has never seen, claude://resume?session=<uuid>, which imports
// it into the Code tab. Importing a session Desktop already has would open a
// duplicate tab, so only sessions created from the phone are imported.
func ClaudeURL(sessionKey string, importSession bool) (string, error) {
	if !ClaudeKey(sessionKey) {
		return "", errors.New("桌面定位仅支持已有 Claude 会话的标准 UUID")
	}
	if importSession {
		return "claude://resume?session=" + sessionKey[len("claude:"):], nil
	}
	return "claude://", nil
}

func OpenClaude(sessionKey string, importSession bool) error {
	url, err := ClaudeURL(sessionKey, importSession)
	if err != nil {
		return err
	}
	if runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		return errors.New("当前系统尚未接入 Claude Desktop 深链接")
	}
	return autostart.OpenBrowser(url)
}

func OpenCodex(sessionKey string) error {
	url, err := CodexURL(sessionKey)
	if err != nil {
		return err
	}
	if runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		return errors.New("当前系统尚未接入 Codex Desktop 深链接")
	}
	// The platform URL handler receives a fixed, validated scheme with no shell text.
	return autostart.OpenBrowser(url)
}
