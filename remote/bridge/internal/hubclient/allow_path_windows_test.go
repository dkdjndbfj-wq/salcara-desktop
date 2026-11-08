//go:build windows

package hubclient

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"salcara/bridge/internal/protocol"
)

func TestCheckCwdRejectsNetworkAndDevicePathsBeforeResolution(t *testing.T) {
	for _, path := range []string{`\\example.invalid\share\project`, `\\?\UNC\example.invalid\share\project`, `\\.\C:\project`, `\\?\C:\project`} {
		if _, err := CheckCwd(path, []protocol.Project{{Path: path}}); !errors.Is(err, ErrNotAllowed) {
			t.Fatalf("nonlocal namespace was resolved: %v", err)
		}
	}
}

func TestCheckCwdLocalSymlinkAndNetworkTarget(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "local-link")
	if err := os.Symlink(project, link); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if cwd, err := CheckCwd(link, []protocol.Project{{Path: project}}); err != nil || cwd != project {
		t.Fatal(cwd, err)
	}
	unc := filepath.Join(root, "unc-link")
	if err := os.Symlink(`\\example.invalid\share\project`, unc); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckCwd(unc, []protocol.Project{{Path: root}}); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("network link followed: %v", err)
	}
}

func TestCheckCwdWindowsCaseSensitiveSiblings(t *testing.T) {
	base := t.TempDir()
	// Only this disposable directory is modified; no real project settings.
	if out, err := exec.Command("fsutil.exe", "file", "setCaseSensitiveInfo", base, "enable").CombinedOutput(); err != nil {
		t.Skipf("case-sensitive fixture unavailable: %s", out)
	}
	upper, lower := filepath.Join(base, "Project"), filepath.Join(base, "project")
	if err := os.Mkdir(upper, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(lower, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckCwd(lower, []protocol.Project{{Path: upper}}); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("case-sensitive sibling authorized: %v", err)
	}
	if _, err := CheckCwd(upper, []protocol.Project{{Path: upper}}); err != nil {
		t.Fatal(err)
	}
}
