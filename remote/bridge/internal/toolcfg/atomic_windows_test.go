//go:build windows

package toolcfg

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestWriteFileKeepModePreservesConfigWhenWindowsReplacementIsBlocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	const original = "model = \"fixture-original\"\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := syscall.CreateFile(p, syscall.GENERIC_READ, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.CloseHandle(handle)
	if err := writeFileKeepMode(path, []byte("model = \"fixture-new\"\n"), 0o600); err == nil {
		t.Fatal("replacement failure accepted by writing directly to the open file")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != original {
		t.Fatal("failed replacement changed the original tool configuration")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatal("failed replacement left a temporary credential file")
	}
}
