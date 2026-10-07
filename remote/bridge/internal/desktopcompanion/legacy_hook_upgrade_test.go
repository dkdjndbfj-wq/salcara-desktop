package desktopcompanion

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPreviousHookVersionCanBeSafelyUninstalledAndNewVersionInstalled(t *testing.T) {
	s, o := fixture(t, "model = \"original\"\n")
	if _, err := s.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	p, _, _, err := s.prepare(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := readRegular(p.state, maxFileBytes)
	if err != nil {
		t.Fatal(err)
	}
	var own ownership
	if json.Unmarshal(raw, &own) != nil {
		t.Fatal("invalid ownership")
	}
	oldPayload := filepath.Join(p.root, "v0.4.0")
	if err := os.Rename(p.payload, oldPayload); err != nil {
		t.Fatal(err)
	}
	p.payload = oldPayload
	own.Version = "0.4.0"
	own.EntryHash, own.HookHash = entryHash(expectedEntryVersion(p, own.Version)), entryHash(expectedPermissionHook(p))
	current := []byte("model = \"original\"\n\n# Salcara desktop companion installer: " + own.ID + "\n" + installedBlockTOML(p, own.Version) + "\n[ui]\nfixture = true\n")
	if err := atomicWrite(p.config, current); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(own)
	if err := atomicWrite(p.state, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Install(context.Background()); err == nil {
		t.Fatal("old configuration silently overwritten")
	}
	if _, err := s.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if string(configBytes(t, o)) != "model = \"original\"\n\n\n[ui]\nfixture = true\n" {
		t.Fatal("user settings changed during migration")
	}
	if _, err := os.Stat(oldPayload); err != nil {
		t.Fatal("old recoverable payload removed")
	}
	if _, err := s.Install(context.Background()); err != nil {
		t.Fatal("new version could not be installed")
	}
}
