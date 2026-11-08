//go:build !windows

package launcher

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"salcara/bridge/internal/agents"
)

func TestUnixClaudeAPIPinLifetimeAndExactCleanup(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "unrelated"} {
		t.Run(outcome, func(t *testing.T) {
			pin, err := agents.WriteClaudeRuntimeSettings(`{"env":{"ANTHROPIC_AUTH_TOKEN":"synthetic-key"}}`)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { agents.RemoveClaudeRuntimeSettings(pin) })
			path := filepath.Join(filepath.Dir(pin), "launch.command")
			source := `test -f "$1" || exit 24; test -z "$CLAUDE_CODE_USE_BEDROCK" || exit 25; exit 0`
			if outcome == "failure" {
				source = `test -f "$1" || exit 24; exit 23`
			}
			p := Plan{Tool: Tool{ID: "claude", Kind: "claude", Path: "/bin/sh"}, Workspace: t.TempDir(), Args: []string{"-c", source, "child", pin}, Environment: []string{"CLAUDE_CODE_USE_BEDROCK="}, ClaudeSettingsPin: pin}
			other := filepath.Join(filepath.Dir(pin), "preserve.txt")
			if outcome == "unrelated" {
				if err := os.WriteFile(other, []byte("preserved"), 0o600); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Remove(other); _ = os.Remove(filepath.Dir(pin)) })
			}
			if err := os.WriteFile(path, []byte(unixCLILaunchScript(p, path)), 0o700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("/bin/sh", path)
			cmd.Env = []string{"PATH=/usr/bin:/bin", "CLAUDE_CODE_USE_BEDROCK=1"}
			err = cmd.Run()
			if outcome == "failure" {
				if code, ok := err.(*exec.ExitError); !ok || code.ExitCode() != 23 {
					t.Fatalf("lost child exit: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			for _, owned := range []string{pin, path} {
				if _, err := os.Stat(owned); !os.IsNotExist(err) {
					t.Fatal("owned runtime file survived")
				}
			}
			if outcome == "unrelated" {
				if data, err := os.ReadFile(other); err != nil || string(data) != "preserved" {
					t.Fatal("unrelated file removed")
				}
			} else if _, err := os.Stat(filepath.Dir(pin)); !os.IsNotExist(err) {
				t.Fatal("empty runtime directory survived")
			}
		})
	}
}
