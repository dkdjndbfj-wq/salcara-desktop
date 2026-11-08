package agents

import (
	"os"
	"path/filepath"
	"strings"
)

// Current Claude npm releases use bin/claude.exe instead of cli.js. Resolve
// that native executable without sending settings/model arguments through CMD.
// Older JS packages and explicit native paths keep their existing behavior.
func nativeClaudeExe(shim string) string {
	ext := strings.ToLower(filepath.Ext(shim))
	if ext != ".cmd" && ext != ".bat" {
		return ""
	}
	dir := filepath.Dir(shim)
	for _, root := range []string{filepath.Join(dir, "node_modules"), filepath.Dir(dir)} {
		native := filepath.Join(root, "@anthropic-ai", "claude-code", "bin", "claude.exe")
		if info, err := os.Stat(native); err == nil && !info.IsDir() {
			return native
		}
	}
	return ""
}
