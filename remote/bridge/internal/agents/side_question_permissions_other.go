//go:build !windows

package agents

import "os"

func protectSideConversationDirectory(path string) error { return os.Chmod(path, 0700) }
