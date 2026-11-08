package agents

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLocalExecutableResolvesNativeClaudeNPM(t *testing.T) {
	for _, layout := range []string{"global", "project"} {
		for _, extension := range []string{".cmd", ".bat", ".CMD"} {
			t.Run(layout+extension, func(t *testing.T) {
				root := t.TempDir()
				modules := filepath.Join(root, "node_modules")
				dir := root
				if layout == "project" {
					dir = filepath.Join(modules, ".bin")
				}
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				shim := filepath.Join(dir, "claude"+extension)
				if err := os.WriteFile(shim, []byte("synthetic npm shim"), 0o600); err != nil {
					t.Fatal(err)
				}
				if got, ok := LocalExecutable("claude", shim); !ok || got != shim {
					t.Fatal("legacy shim behavior changed")
				}
				native := filepath.Join(modules, "@anthropic-ai", "claude-code", "bin", "claude.exe")
				if err := os.MkdirAll(filepath.Dir(native), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(native, []byte("synthetic native CLI"), 0o700); err != nil {
					t.Fatal(err)
				}
				if got, ok := LocalExecutable("claude", shim); !ok || got != native {
					t.Fatal("native npm executable not selected")
				}
				if got, ok := LocalExecutable("claude", native); !ok || got != native {
					t.Fatal("explicit native executable changed")
				}
			})
		}
	}
}
