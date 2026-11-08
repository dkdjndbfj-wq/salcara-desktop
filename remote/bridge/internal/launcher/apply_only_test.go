package launcher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyOnlyCommitsConfigurationWithoutStoppingOrStarting(t *testing.T) {
	for _, target := range []string{"codex", "codex-desktop", "claude"} {
		t.Run(target, func(t *testing.T) {
			kind := strings.Split(target, "-")[0]
			s, root, _ := switchFixture(t, kind, target)
			s.Start = func(context.Context, Plan) (int, error) { t.Fatal("apply started a process"); return 0, nil }
			s.Stop = func(context.Context, Plan) error { t.Fatal("apply stopped a process"); return nil }
			authorized := false
			result, err := s.ApplyAuthorized(context.Background(), account(kind, "apply-fixture", "synthetic-lease"), target, t.TempDir(), nil, func() error { authorized = true; return nil })
			if err != nil || !authorized || result.PID != 0 {
				t.Fatalf("apply failed: %v", err)
			}
			name := "settings.json"
			if kind == "codex" {
				name = "config.toml"
			}
			if _, err := os.Stat(filepath.Join(root, name)); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(result.Message, "未启动") {
				t.Fatal("apply result claimed a launch")
			}
		})
	}
}

func TestApplyOnlyAuthorizationFailureRestoresOriginal(t *testing.T) {
	s, root, _ := switchFixture(t, "codex", "codex")
	path := fixtureFile(t, root, "config.toml", "model = \"existing-model\"\n")
	_, err := s.ApplyAuthorized(context.Background(), account("codex", "apply-fixture", "synthetic-lease"), "codex", t.TempDir(), nil, func() error { return errors.New("fixture authorization failure") })
	data, readErr := os.ReadFile(path)
	if err == nil || readErr != nil || string(data) != "model = \"existing-model\"\n" {
		t.Fatal("failed apply did not restore previous configuration")
	}
}
