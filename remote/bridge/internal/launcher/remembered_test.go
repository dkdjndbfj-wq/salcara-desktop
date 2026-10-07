package launcher

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func locationFixture(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("synthetic executable, never started"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDetectedLocationsSurviveRestartWithoutCopyingSecrets(t *testing.T) {
	dir, exe := t.TempDir(), locationFixture(t, "Codex.exe")
	s := New(dir)
	s.FindTools = func(context.Context, map[string]string) []Tool {
		return []Tool{{ID: "codex-desktop", Kind: "codex", Path: exe, Family: "fixture_family", AppID: "App", Available: true}}
	}
	s.Tools(context.Background(), nil)
	raw, err := os.ReadFile(filepath.Join(dir, "tool-locations.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]map[string]any
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 1 {
		t.Fatal("invalid location metadata")
	}
	for field := range fields["codex-desktop"] {
		if field != "path" && field != "family" && field != "appId" {
			t.Fatal("unexpected sensitive metadata field")
		}
	}
	restarted := New(dir)
	restarted.FindTools = func(context.Context, map[string]string) []Tool { return []Tool{{ID: "codex-desktop", Kind: "codex"}} }
	got := restarted.Tools(context.Background(), nil)
	if len(got) != 1 || !got[0].Available || got[0].Custom || got[0].Path != exe || got[0].AppID != "App" {
		t.Fatal("remembered desktop was lost")
	}
	paths := restarted.RememberedPaths()
	paths["codex-desktop"] = "mutated"
	if restarted.RememberedPaths()["codex-desktop"] != exe {
		t.Fatal("shared mutable paths")
	}
}

func TestRememberedLocationsNeverOverrideExplicitSelectionOrRemovedInstall(t *testing.T) {
	dir, exe := t.TempDir(), locationFixture(t, "Claude.exe")
	s := New(dir)
	s.rememberLocations([]Tool{{ID: "claude-desktop", Path: exe, Available: true}})
	s.FindTools = func(context.Context, map[string]string) []Tool {
		return []Tool{{ID: "claude-desktop", Custom: true, Path: "explicit-missing"}}
	}
	got := s.Tools(context.Background(), map[string]string{"claude-desktop": "explicit-missing"})
	if got[0].Available || got[0].Path != "explicit-missing" {
		t.Fatal("explicit path replaced")
	}
	custom := locationFixture(t, "private-choice.exe")
	s.rememberLocations([]Tool{{ID: "claude-desktop", Path: custom, Available: true, Custom: true}})
	if s.RememberedPaths()["claude-desktop"] != exe {
		t.Fatal("custom choice leaked into discovered metadata")
	}
	if err := os.Remove(exe); err != nil {
		t.Fatal(err)
	}
	if len(New(dir).RememberedPaths()) != 0 || len(s.RememberedPaths()) != 0 {
		t.Fatal("removed executable retained")
	}
}

func TestLocationMetadataRejectsMalformedAndOversizedCache(t *testing.T) {
	dir, exe := t.TempDir(), locationFixture(t, "Codex.exe")
	for _, location := range []toolLocation{{Path: "relative"}, {Path: t.TempDir()}, {Path: exe, Family: "bad\nfamily"}, {Path: exe, AppID: "bad\x00app"}} {
		if validLocation(location) {
			t.Fatal("invalid location admitted")
		}
	}
	raw, _ := json.Marshal(map[string]toolLocation{"unknown-target": {Path: exe}, "codex": {Path: "relative"}})
	path := filepath.Join(dir, "tool-locations.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if len(New(dir).RememberedPaths()) != 0 {
		t.Fatal("invalid cache admitted")
	}
	if err := os.WriteFile(path, make([]byte, (32<<10)+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if len(New(dir).RememberedPaths()) != 0 {
		t.Fatal("oversized cache admitted")
	}
}
