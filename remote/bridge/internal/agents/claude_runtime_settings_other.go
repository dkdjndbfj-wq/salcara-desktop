//go:build !windows

package agents

import "os"

func protectClaudeRuntimeDir(path string) error { return os.Chmod(path, 0o700) }
