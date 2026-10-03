//go:build windows

package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileKeepModeCleansTemporaryFileAfterReadOnlyReplacementFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("fixture-original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o600) })
	if err := WriteFileKeepMode(path, []byte("fixture-new-key"), 0o600); err == nil {
		t.Fatal("replacement of a read-only file succeeded")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "fixture-original" {
		t.Fatal("failed replacement changed the read-only configuration")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatal("failed replacement left a read-only temporary credential file")
	}
}
