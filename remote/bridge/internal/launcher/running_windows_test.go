//go:build windows

package launcher

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRunningDesktopPathRequiresElectronAndKnownDesktopIdentity(t *testing.T) {
	root := t.TempDir()
	for _, app := range []struct{ folder, name, kind string }{
		{"Codex", "Codex.exe", "codex"}, {"Claude", "Claude.exe", "claude"},
		{"Codex", "ChatGPT.exe", "codex"}, {"ChatGPT", "ChatGPT.exe", ""}, {"other", "other.exe", ""},
	} {
		dir := filepath.Join(root, app.folder)
		if err := os.MkdirAll(filepath.Join(dir, "resources"), 0o700); err != nil {
			t.Fatal(err)
		}
		exe := filepath.Join(dir, app.name)
		if err := os.WriteFile(exe, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "resources", "app.asar"), []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
		tool, kind := toolFromRunningPath(exe, app.name)
		if kind != app.kind || kind != "" && tool.Path != exe {
			t.Fatal("wrong desktop classification")
		}
	}
	cli := locationFixture(t, "Codex.exe")
	if _, kind := toolFromRunningPath(cli, "codex.exe"); kind != "" {
		t.Fatal("CLI mistaken for desktop")
	}
}

func TestRunningDesktopProbeHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if len(runningDesktopTools(ctx)) != 0 {
		t.Fatal("cancelled probe read processes")
	}
}

// Read-only, opt-in machine smoke check. Reports no paths, command lines or
// account information, and never starts/stops a real Agent.
func TestRunningDesktopProbeOnLocalMachine(t *testing.T) {
	if os.Getenv("SALCARA_TEST_RUNNING_DISCOVERY") != "1" {
		t.Skip("opt-in read-only Windows process discovery")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tools := runningDesktopTools(ctx)
	if len(tools) == 0 {
		t.Fatal("no running supported desktop discovered")
	}
	for kind, tool := range tools {
		if kind != "codex" && kind != "claude" || !validLocation(toolLocation{Path: tool.Path, Family: tool.Family, AppID: tool.AppID}) {
			t.Fatal("invalid discovered executable")
		}
		if tool.Family != "" && tool.AppID == "" {
			t.Fatal("package identity incomplete")
		}
	}
}
