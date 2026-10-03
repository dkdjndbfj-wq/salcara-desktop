package launcher

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"salcara/bridge/internal/toolcfg"
)

func fixtureTree(t *testing.T, root string) map[string]string {
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
		info, err := entry.Info()
		if err != nil {
			return err
		}
		out[rel] = info.Mode().String()
		if !entry.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out[rel] += ":" + digest(data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestClaudeDesktopAPIEntryPointsRejectWithoutSideEffects(t *testing.T) {
	s, codeDir, appData := switchFixture(t, "claude", "claude-desktop")
	fixtureFile(t, codeDir, "settings.json", `{"keep":"code"}`)
	fixtureFile(t, appData, "config.json", `{"theme":"dark"}`)
	fixtureFile(t, appData, "claude_desktop_config.json", `{"deploymentMode":"1p"}`)
	fixtureFile(t, appData, "configLibrary/_meta.json", `{"appliedId":"11111111-1111-4111-8111-111111111111","entries":[{"id":"11111111-1111-4111-8111-111111111111","name":"Original"}]}`)
	fixtureFile(t, appData, "configLibrary/11111111-1111-4111-8111-111111111111.json", `{"inferenceProvider":"gateway","keep":true}`)
	fixtureFile(t, appData, "local-agent-mode-sessions/original.json", "original-chat")
	fixtureFile(t, s.Dir, "default-backups/claude-desktop.json", "existing-backup")
	stops, starts, discoveries := 0, 0, 0
	s.Stop = func(context.Context, Plan) error { stops++; return nil }
	s.Start = func(context.Context, Plan) (int, error) { starts++; return 101, nil }
	s.FindTools = func(context.Context, map[string]string) []Tool { discoveries++; return nil }
	tool := stubTool(t, "claude-desktop")
	a := account("claude", "chosen", "fixture-private-key")
	ctx, cwd := context.Background(), t.TempDir()
	calls := map[string]func() error{
		"Prepare":       func() error { _, err := s.Prepare(a, tool, cwd); return err },
		"PrepareSwitch": func() error { _, err := s.PrepareSwitch(ctx, a, tool.ID, cwd, nil); return err },
		"PreviewSwitch": func() error { _, err := s.PreviewSwitch(ctx, a, tool.ID, cwd, nil); return err },
		"Switch":        func() error { _, err := s.Switch(ctx, a, tool.ID, cwd, nil); return err },
		"Launch":        func() error { _, err := s.Launch(ctx, a, tool.ID, cwd, nil); return err },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			before := map[string]map[string]string{}
			for _, dir := range []string{s.Dir, codeDir, appData, cwd} {
				before[dir] = fixtureTree(t, dir)
			}
			err := call()
			if !errors.Is(err, toolcfg.ErrClaudeDesktopAutomaticAPIUnavailable) || strings.Contains(err.Error(), a.Key) {
				t.Fatalf("expected a safe compatibility rejection, got %v", err)
			}
			for dir, want := range before {
				if !reflect.DeepEqual(fixtureTree(t, dir), want) {
					t.Fatalf("rejection changed files or directories in %s", filepath.Base(dir))
				}
			}
			if stops != 0 || starts != 0 || discoveries != 0 {
				t.Fatalf("rejection had process side effects: stop=%d start=%d discovery=%d", stops, starts, discoveries)
			}
		})
	}
}
