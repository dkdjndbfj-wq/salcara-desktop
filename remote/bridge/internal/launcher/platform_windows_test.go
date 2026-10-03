//go:build windows

package launcher

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOriginalProcessSkipsIndependentProfilesAndChildren(t *testing.T) {
	appData := `C:\Users\fixture\AppData\Roaming\Codex`
	for _, tc := range []struct {
		command  string
		original bool
	}{{`"C:\Codex\ChatGPT.exe"`, true}, {`"C:\Codex\ChatGPT.exe" --type=renderer`, false}, {`"C:\Codex\ChatGPT.exe" --user-data-dir="C:\Bridge\instances\other-app"`, false}, {`"C:\Codex\ChatGPT.exe" --user-data-dir="` + appData + `"`, true}} {
		if originalProcess(tc.command, appData) != tc.original {
			t.Fatal("unsafe process match")
		}
	}
}

// Opt-in because it opens and closes a real, isolated terminal window.
func TestWindowsInteractiveTerminal(t *testing.T) {
	if os.Getenv("SALCARA_TEST_WINDOWS_LAUNCH") != "1" {
		t.Skip("set SALCARA_TEST_WINDOWS_LAUNCH=1 for real console smoke test")
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "console-state.json")
	fixture := filepath.Join(dir, "fixture.ps1")
	script := "@{ inputRedirected=[Console]::IsInputRedirected; outputRedirected=[Console]::IsOutputRedirected; selectedKey=($env:OPENAI_API_KEY -eq 'local-fixture-key') } | ConvertTo-Json | Set-Content -LiteralPath " + psLiteral(out) + " -Encoding UTF8"
	if err := os.WriteFile(fixture, append([]byte{0xef, 0xbb, 0xbf}, []byte(script)...), 0o600); err != nil {
		t.Fatal(err)
	}
	p := Plan{Tool: Tool{ID: "codex", Kind: "codex", Name: "terminal fixture", Path: powershell()}, Workspace: dir, ProfileDir: dir, Environment: append(os.Environ(), "OPENAI_API_KEY=local-fixture-key"), Args: []string{"-NoProfile", "-File", fixture}}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pid, err := startPlatform(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Kill()
		}
	}()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(out)
		if err == nil {
			var state struct {
				InputRedirected  bool
				OutputRedirected bool
				SelectedKey      bool
			}
			if err := json.Unmarshal([]byte(string(b)[3:]), &state); err != nil {
				t.Fatal(err)
			}
			if state.InputRedirected || state.OutputRedirected || !state.SelectedKey {
				t.Fatalf("invalid console handles or environment: %+v", state)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("fixture did not start inside the interactive console")
}
