package toolcfg

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func desktopFixtureTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out[rel] = "directory"
		if !entry.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out[rel] = string(data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestClaudeDesktopLegacyWritersRejectWithoutChanges(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, writer := range []string{"switch", "profile"} {
			t.Run(writer+map[bool]string{true: "-existing", false: "-absent"}[existing], func(t *testing.T) {
				root := t.TempDir()
				target := filepath.Join(root, "Claude")
				if existing {
					if err := os.MkdirAll(filepath.Join(target, "configLibrary"), 0o700); err != nil {
						t.Fatal(err)
					}
					for name, data := range map[string]string{"config.json": `{"theme":"dark"}`, "claude_desktop_config.json": `{"deploymentMode":"1p"}`, "configLibrary/_meta.json": "original-meta", "configLibrary/original.json": "original-provider"} {
						if err := os.WriteFile(filepath.Join(target, name), []byte(data), 0o600); err != nil {
							t.Fatal(err)
						}
					}
				}
				before := desktopFixtureTree(t, root)
				var err error
				if writer == "switch" {
					err = ApplySwitchClaudeDesktop(target, "Chosen", "https://fixture.test", "fixture-key", "bearer", "fixture-model")
				} else {
					err = WriteClaudeDesktopProfile(target, "chosen", "Chosen", "https://fixture.test", "fixture-key", "bearer", []string{"fixture-model"})
				}
				if !errors.Is(err, ErrClaudeDesktopAutomaticAPIUnavailable) {
					t.Fatalf("writer did not reject incompatible config: %v", err)
				}
				if !reflect.DeepEqual(desktopFixtureTree(t, root), before) {
					t.Fatal("rejected writer changed or created files/directories")
				}
			})
		}
	}
}
