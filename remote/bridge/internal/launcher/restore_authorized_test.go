package launcher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRestoreDefaultAuthorizationRunsAfterChecksBeforeFirstWrite(t *testing.T) {
	s, root, _ := switchFixture(t, "codex", "codex")
	original := "# original configuration\n"
	path := fixtureFile(t, root, "config.toml", original)
	if err := s.ApplyDefault(account("codex", "fixture", "synthetic-private")); err != nil {
		t.Fatal(err)
	}
	applied, _ := os.ReadFile(path)
	called := 0
	if err := s.RestoreDefaultAuthorized("codex", func() error {
		called++
		current, _ := os.ReadFile(path)
		if string(current) != string(applied) {
			t.Fatal("restored profile before retiring authorization")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	current, _ := os.ReadFile(path)
	if called != 1 || string(current) != original {
		t.Fatal("restore hook or original profile missing")
	}
}

func TestRestoreDefaultRejectedChecksDoNotRetireAuthorization(t *testing.T) {
	for _, failure := range []string{"external", "directory-change", "backup-path", "hook-error"} {
		t.Run(failure, func(t *testing.T) {
			s, root, _ := switchFixture(t, "codex", "codex")
			path := fixtureFile(t, root, "config.toml", "# original\n")
			if err := s.ApplyDefault(account("codex", "fixture", "synthetic-private")); err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "external":
				fixtureFile(t, root, "config.toml", "# external edit\n")
			case "directory-change":
				t.Setenv("CODEX_HOME", t.TempDir())
			case "backup-path":
				b, err := os.ReadFile(s.snapshotPath("codex"))
				if err != nil {
					t.Fatal(err)
				}
				b = []byte(strings.Replace(string(b), "config.toml", "different.toml", 1))
				if err := os.WriteFile(s.snapshotPath("codex"), b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(path)
			calls := 0
			err := s.RestoreDefaultAuthorized("codex", func() error { calls++; return errors.New("synthetic authorization hook failure") })
			wantCalls := 0
			if failure == "hook-error" {
				wantCalls = 1
			}
			after, _ := os.ReadFile(path)
			if err == nil || calls != wantCalls || string(before) != string(after) {
				t.Fatal("failed restore wrote or retired authorization before checks")
			}
		})
	}
}

func TestClaudeRestoreAuthorizationPrecedesWritesAndRestartEvenWhenStartFails(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "start-failure"}[failure], func(t *testing.T) {
			s, root, a, _, _ := fixtureClaude3P(t, true)
			if _, err := s.Switch(context.Background(), a, nil, false); err != nil {
				t.Fatal(err)
			}
			before := fixtureTree(t, root)
			retired := false
			s.Local.Start = func(context.Context, Plan) (int, error) {
				if !retired {
					t.Fatal("Claude restart began before authorization retirement")
				}
				if failure {
					return 0, errors.New("synthetic start failure")
				}
				return 1, nil
			}
			_, err := s.RestoreAuthorized(context.Background(), nil, func() error {
				if got := fixtureTree(t, root); got[filepath.Join(root, "configLibrary", cd3PID+".json")] != before[filepath.Join(root, "configLibrary", cd3PID+".json")] {
					t.Fatal("Claude profile restored before hook")
				}
				retired = true
				return nil
			})
			if !retired || (err != nil) != failure {
				t.Fatal("restore authorization order failed", err)
			}
			if failure {
				after := fixtureTree(t, root)
				for path, want := range before {
					if after[path] != want {
						t.Fatal("failed restart did not roll back profile bytes")
					}
				}
			}
		})
	}
}

func TestClaudeRestoreRejectsQuitTimeExternalChangesBeforeAuthorization(t *testing.T) {
	s, root, a, _, _ := fixtureClaude3P(t, true)
	if _, err := s.Switch(context.Background(), a, nil, false); err != nil {
		t.Fatal(err)
	}
	s.Local.Stop = func(context.Context, Plan) error {
		fixtureFile(t, root, "configLibrary/"+cd3PID+".json", `{"changedByOtherSoftware":true}`)
		return nil
	}
	retired := false
	if _, err := s.RestoreAuthorized(context.Background(), nil, func() error { retired = true; return nil }); err == nil || retired {
		t.Fatal("quit-time external edit bypassed the authorization boundary")
	}
	data, err := os.ReadFile(filepath.Join(root, "configLibrary", cd3PID+".json"))
	if err != nil || string(data) != `{"changedByOtherSoftware":true}` {
		t.Fatal("external profile unexpectedly rewritten")
	}
}
