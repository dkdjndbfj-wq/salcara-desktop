package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"salcara/bridge/internal/toolcfg"
)

func TestCatalogTransactionRemovesOnlyItsCreatedFileOnRestore(t *testing.T) {
	s, root, _ := switchFixture(t, "codex", "codex-desktop")
	configPath := fixtureFile(t, root, "config.toml", "# original profile\n")
	authPath := fixtureFile(t, root, "auth.json", `{"OPENAI_API_KEY":"synthetic-original"}`)
	cachePath := fixtureFile(t, root, "models_cache.json", `{"models":[{"slug":"user-cache"}]}`)
	before, err := captureFiles([]string{configPath, authPath, cachePath, toolcfg.CodexCatalogPath(root)})
	if err != nil {
		t.Fatal(err)
	}
	a := account("codex", "catalog-new", "synthetic-new")
	a.Models = []string{"gpt-fixture", "claude-fixture"}
	a.CatalogOverride = true
	if _, err := s.Switch(context.Background(), a, "codex-desktop", t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(toolcfg.CodexCatalogPath(root)); err != nil {
		t.Fatal("override did not publish a catalog")
	}
	assertIsolatedProfileUnchanged(t, before[2:3])
	if err := s.RestoreDefault("codex"); err != nil {
		t.Fatal(err)
	}
	assertIsolatedProfileUnchanged(t, before)
}

func TestCatalogTransactionFailedStartDoesNotLeaveCreatedCatalog(t *testing.T) {
	s, root, _ := switchFixture(t, "codex", "codex-desktop")
	fixtureFile(t, root, "config.toml", "# original\n")
	cachePath := fixtureFile(t, root, "models_cache.json", "synthetic user cache")
	paths := append(codexDefaultPaths(), cachePath)
	before, err := captureFiles(paths)
	if err != nil {
		t.Fatal(err)
	}
	a := account("codex", "catalog-failure", "synthetic-key")
	a.Models, a.CatalogOverride = []string{"gpt-fixture", "grok-fixture"}, true
	s.Start = func(context.Context, Plan) (int, error) { return 0, errors.New("synthetic spawn failure") }
	if _, err := s.Switch(context.Background(), a, "codex-desktop", t.TempDir(), nil); err == nil {
		t.Fatal("failed Start was accepted")
	}
	assertIsolatedProfileUnchanged(t, before)
	// The failed switch's manifest must also describe the rolled-back state,
	// otherwise a later restore would fail or recreate the discarded catalog.
	if err := s.RestoreDefault("codex"); err != nil {
		t.Fatal(err)
	}
	assertIsolatedProfileUnchanged(t, before)
}

func TestLegacyCatalogManifestMigrationAndOwnership(t *testing.T) {
	for _, operation := range []string{"switch", "apply-default", "restore"} {
		for _, currentCatalog := range []string{"absent", "orphan-file", "bridge-pointer"} {
			t.Run(operation+"/"+currentCatalog, func(t *testing.T) {
				s, root, _ := switchFixture(t, "codex", "codex-desktop")
				originalConfig := []byte("# original configuration\n")
				originalAuth := []byte(`{"OPENAI_API_KEY":"synthetic-original"}`)
				currentConfig := "model = \"legacy-model\"\n"
				if currentCatalog == "bridge-pointer" {
					currentConfig = toolcfg.SetCodexCatalogTOML(currentConfig, root, toolcfg.CodexCatalogPath(root))
				}
				configPath := fixtureFile(t, root, "config.toml", currentConfig)
				authPath := fixtureFile(t, root, "auth.json", `{"OPENAI_API_KEY":"synthetic-legacy"}`)
				cachePath := fixtureFile(t, root, "models_cache.json", "synthetic user cache")
				if currentCatalog == "orphan-file" {
					fixtureFile(t, root, toolcfg.CodexCatalogFile, `{"models":[{"slug":"user-owned"}]}`)
				}
				current, err := captureFiles([]string{configPath, authPath})
				if err != nil {
					t.Fatal(err)
				}
				legacy := []savedFile{
					{Path: configPath, Existed: true, Data: originalConfig, AppliedHash: digest(current[0].Data)},
					{Path: authPath, Existed: true, Data: originalAuth, AppliedHash: digest(current[1].Data)},
				}
				if err := saveSnapshot(s.snapshotPath("codex"), legacy); err != nil {
					t.Fatal(err)
				}
				before, err := captureFiles(append(codexDefaultPaths(), cachePath, s.snapshotPath("codex")))
				if err != nil {
					t.Fatal(err)
				}
				started, stopped := false, false
				s.Start = func(context.Context, Plan) (int, error) { started = true; return 99, nil }
				s.Stop = func(context.Context, Plan) error { stopped = true; return nil }
				a := account("codex", "migration", "synthetic-key")
				a.Models, a.CatalogOverride = []string{"gpt-fixture", "deepseek-fixture"}, true
				switch operation {
				case "switch":
					_, err = s.Switch(context.Background(), a, "codex-desktop", t.TempDir(), nil)
				case "apply-default":
					err = s.ApplyDefault(a)
				case "restore":
					err = s.RestoreDefault("codex")
				}
				if currentCatalog != "absent" {
					if err == nil || started || stopped {
						t.Fatal("ambiguous legacy catalog ownership did not fail before process changes")
					}
					assertIsolatedProfileUnchanged(t, before)
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if operation != "restore" {
					data, readErr := os.ReadFile(s.snapshotPath("codex"))
					var migrated []savedFile
					if readErr != nil || json.Unmarshal(data, &migrated) != nil || len(migrated) != 3 {
						t.Fatal("legacy manifest did not upgrade to include catalog ownership")
					}
					if err := s.RestoreDefault("codex"); err != nil {
						t.Fatal(err)
					}
				}
				want := []savedFile{
					{Path: configPath, Existed: true, Data: originalConfig},
					{Path: authPath, Existed: true, Data: originalAuth},
					{Path: toolcfg.CodexCatalogPath(root)},
					before[3],
				}
				assertIsolatedProfileUnchanged(t, want)
			})
		}
	}
}

func TestCatalogTransactionRejectsMalformedSameLengthManifest(t *testing.T) {
	s, root, _ := switchFixture(t, "codex", "codex-desktop")
	fixtureFile(t, root, "config.toml", "# original\n")
	paths := codexDefaultPaths()
	// A type error can still populate a three-entry slice. Matching its length
	// must never turn the failed JSON decode into an accepted backup manifest.
	entries := make([]map[string]any, 0, len(paths))
	for _, path := range paths {
		entries = append(entries, map[string]any{"path": path, "existed": "invalid-boolean"})
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	fixtureFile(t, filepath.Dir(s.snapshotPath("codex")), "codex.json", string(raw))
	before, err := captureFiles(append(paths, s.snapshotPath("codex")))
	if err != nil {
		t.Fatal(err)
	}
	s.Stop = func(context.Context, Plan) error { t.Fatal("malformed backup stopped tool"); return nil }
	s.Start = func(context.Context, Plan) (int, error) { t.Fatal("malformed backup started tool"); return 0, nil }
	if _, err := s.Switch(context.Background(), account("codex", "malformed", "synthetic-key"), "codex-desktop", t.TempDir(), nil); err == nil {
		t.Fatal("partially decoded manifest was accepted")
	}
	assertIsolatedProfileUnchanged(t, before)
}
