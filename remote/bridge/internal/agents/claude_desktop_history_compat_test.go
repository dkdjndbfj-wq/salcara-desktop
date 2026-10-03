package agents

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClaudeDesktopHistoryAcceptsLaterBuildsOnlyInTheSameLine(t *testing.T) {
	root := filepath.Join(t.TempDir(), "local-agent-mode-sessions", "01234567-89ab-4cde-8fab-0123456789ab", "23456789-abcd-4ef0-8abc-0123456789ab")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"2.16120.0", "2.16120.0.0", "2.16121.0", "2.18000.4"} {
		if _, err := NewClaudeDesktopHistory(root, v); err != nil {
			t.Fatalf("%s should be readable in compatibility mode: %v", v, err)
		}
	}
	for _, v := range []string{"2.16000.0", "3.0.0", "garbage", ""} {
		if _, err := NewClaudeDesktopHistory(root, v); err == nil {
			t.Fatalf("%s must be rejected", v)
		}
	}
}
