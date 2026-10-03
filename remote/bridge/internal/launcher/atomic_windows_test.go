//go:build windows

package launcher

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func lockFixtureAgainstReplacement(t *testing.T, path string) {
	t.Helper()
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := syscall.CreateFile(p, syscall.GENERIC_READ, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.CloseHandle(handle) })
}

func TestRestoreFilesPreservesLockedConfiguration(t *testing.T) {
	dir := t.TempDir()
	path := fixtureFile(t, dir, "config.toml", "fixture-current")
	lockFixtureAgainstReplacement(t, path)
	if err := restoreFiles([]savedFile{{Path: path, Existed: true, Data: []byte("fixture-previous")}}); err == nil {
		t.Fatal("rollback directly overwrote a file whose replacement was blocked")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "fixture-current" {
		t.Fatal("failed rollback changed the locked configuration")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatal("failed rollback left a temporary credential file")
	}
}

func TestSaveSnapshotPreservesLockedBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex.json")
	before := []savedFile{{Path: "fixture-config.toml", Existed: true, Data: []byte("fixture-original")}}
	if err := saveSnapshot(path, before); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lockFixtureAgainstReplacement(t, path)
	if err := saveSnapshot(path, []savedFile{{Path: "fixture-new-config.toml"}}); err == nil {
		t.Fatal("accepted replacement of a locked snapshot")
	}
	got, err := os.ReadFile(path)
	if err != nil || digest(got) != digest(data) {
		t.Fatal("failed snapshot replacement changed the original backup")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatal("failed snapshot replacement left a temporary credential file")
	}
}
