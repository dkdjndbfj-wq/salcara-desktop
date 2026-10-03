package agents

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

func claudeSettingsSig(s Settings, overridePath string) string {
	path := overridePath
	if path == "" {
		path = s.ClaudePath
	}
	h := sha256.Sum256([]byte(strings.TrimRight(s.claudeRoot(), "/") + "\x00" + s.ClaudeKey + "\x00" + s.ClaudeAuthMode + "\x00" + path + "\x00" + s.ClaudeModel))
	return hex.EncodeToString(h[:])
}
