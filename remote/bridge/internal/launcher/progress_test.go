package launcher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLaunchProgressReportsActualStopFailure(t *testing.T) {
	s, root, _ := switchFixture(t, "codex", "codex-desktop")
	a := account("codex", "fixture", "synthetic-secret")
	before := fixtureFile(t, root, "config.toml", "model = \"old\"\n")
	steps := []string{}
	ctx := WithLaunchProgress(context.Background(), func(event LaunchProgress) {
		if event.Type == "phase" {
			steps = append(steps, event.Step)
		}
	})
	starts := 0
	s.Start = func(context.Context, Plan) (int, error) { starts++; return 1, nil }
	s.Stop = func(context.Context, Plan) error { return errors.New("window close rejected: synthetic-secret") }
	_, err := s.SwitchAuthorized(ctx, a, "codex-desktop", t.TempDir(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "window close rejected") || strings.Contains(err.Error(), a.Key) || starts != 0 {
		t.Fatalf("unsafe or missing failure: %v starts=%d", err, starts)
	}
	if got := strings.Join(steps, ","); got != "lock,tool,backup,stop" {
		t.Fatalf("unexecuted phase reported: %s", got)
	}
	data, _ := os.ReadFile(before)
	if string(data) != "model = \"old\"\n" {
		t.Fatal("stop failure changed profile")
	}
}

func TestLaunchCancellationBeforeMutationOrStart(t *testing.T) {
	for _, boundary := range []string{"wait-lock", "after-stop", "after-authorize"} {
		t.Run(boundary, func(t *testing.T) {
			s, root, _ := switchFixture(t, "codex", "codex-desktop")
			original := "model = \"original\"\n"
			fixtureFile(t, root, "config.toml", original)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			starts, stops := 0, 0
			s.Start = func(context.Context, Plan) (int, error) { starts++; return 1, nil }
			s.Stop = func(context.Context, Plan) error {
				stops++
				if boundary == "after-stop" {
					cancel()
				}
				return nil
			}
			authorize := func() error {
				if boundary == "after-authorize" {
					cancel()
				}
				return nil
			}
			if boundary == "wait-lock" {
				s.mu.Lock()
				defer s.mu.Unlock()
				cancel()
			}
			_, err := s.SwitchAuthorized(ctx, account("codex", "fixture", "synthetic-key"), "codex-desktop", t.TempDir(), nil, authorize)
			if !errors.Is(err, context.Canceled) || starts != 0 {
				t.Fatalf("cancel reached start: %v %d", err, starts)
			}
			if boundary == "wait-lock" && stops != 0 {
				t.Fatal("cancelled queue stopped a tool")
			}
			data, _ := os.ReadFile(filepath.Join(root, "config.toml"))
			if string(data) != original {
				t.Fatal("cancelled launch retained new configuration")
			}
		})
	}
}

func TestLaunchWaitingLockHonorsDeadline(t *testing.T) {
	s, _, _ := switchFixture(t, "codex", "codex")
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := s.SwitchAuthorized(ctx, account("codex", "fixture", "synthetic-key"), "codex", t.TempDir(), nil, nil)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled queue still waits for launch mutex")
	}
}

func TestClaudeLaunchCancellationAfterStopAndAuthorization(t *testing.T) {
	for _, boundary := range []string{"after-stop", "after-authorize"} {
		t.Run(boundary, func(t *testing.T) {
			s, root, a, starts, _ := fixtureClaude3P(t, true)
			before := fixtureTree(t, root)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s.Local.Stop = func(context.Context, Plan) error {
				if boundary == "after-stop" {
					cancel()
				}
				return nil
			}
			_, err := s.SwitchAuthorized(ctx, a, nil, true, func() error {
				if boundary == "after-authorize" {
					cancel()
				}
				return nil
			})
			if !errors.Is(err, context.Canceled) || *starts != 0 {
				t.Fatalf("cancel reached Claude start: %v", err)
			}
			for path, want := range before {
				if fixtureTree(t, root)[path] != want {
					t.Fatal("cancel changed original Claude profile")
				}
			}
		})
	}
}
