//go:build windows

package config

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestUpdatePreservesConfigWhenWindowsReplacementIsBlocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	original := s.Get()
	notified := false
	s.OnChange(func(Config, Config) { notified = true })
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	// Allow direct writes but prohibit replacing the file, as a desktop app
	// holding its configuration open can do. A direct-write fallback is unsafe.
	handle, err := syscall.CreateFile(p, syscall.GENERIC_READ, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.CloseHandle(handle)
	if err := s.Update(func(c *Config) error {
		c.AccountKey = "fixture-new-key"
		return nil
	}); err == nil {
		t.Fatal("replacement failure accepted by writing directly to the open file")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed update changed the existing configuration")
	}
	if s.Get().AccountKey != original.AccountKey || notified {
		t.Fatal("failed persistence published a changed in-memory configuration")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatal("failed persistence left a temporary credential file")
	}
}
