package launcher

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"salcara/bridge/internal/config"
)

func isolatedLaunchFixture(t *testing.T, kind string, existing bool) (*Service, config.LocalAccount, Tool, string, []savedFile) {
	t.Helper()
	s := New(t.TempDir())
	a := account(kind, "transaction-fixture", "synthetic-new-key")
	a.Models = []string{a.Model, "synthetic-other-model"}
	a.CatalogOverride = kind == "codex"
	tool := stubTool(t, kind)
	s.FindTools = func(context.Context, map[string]string) []Tool { return []Tool{tool} }
	profile := filepath.Join(s.Dir, "instances", a.ID, a.Kind)
	if existing {
		if err := os.MkdirAll(profile, 0o700); err != nil {
			t.Fatal(err)
		}
		files := map[string]string{"settings.json": `{"env":{"ANTHROPIC_AUTH_TOKEN":"synthetic-old-key"},"model":"old-model"}`}
		if kind == "codex" {
			files = map[string]string{
				"config.toml":         "model = \"old-model\"\n",
				"auth.json":           `{"OPENAI_API_KEY":"synthetic-old-key"}`,
				"salcara-models.json": `{"models":[{"slug":"old-model"}]}`,
			}
		}
		for name, contents := range files {
			if err := os.WriteFile(filepath.Join(profile, name), []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	before, err := captureFiles(isolatedProfileFiles(profile, a.Kind))
	if err != nil {
		t.Fatal(err)
	}
	return s, a, tool, t.TempDir(), before
}

func assertIsolatedProfileUnchanged(t *testing.T, before []savedFile) {
	t.Helper()
	for _, f := range before {
		after, err := os.ReadFile(f.Path)
		if !f.Existed {
			if !os.IsNotExist(err) {
				t.Errorf("failed launch left file %s", filepath.Base(f.Path))
			}
			continue
		}
		if err != nil || !bytes.Equal(f.Data, after) {
			t.Errorf("failed launch changed existing %s", filepath.Base(f.Path))
		}
	}
}

func TestIsolatedLaunchFailureRestoresProfile(t *testing.T) {
	for _, kind := range []string{"codex", "claude"} {
		for _, existing := range []bool{false, true} {
			for _, failure := range []string{"hook", "start", "cancel-in-hook", "cancel-in-discovery"} {
				t.Run(kind+"/"+map[bool]string{false: "new", true: "existing"}[existing]+"/"+failure, func(t *testing.T) {
					s, a, tool, workspace, before := isolatedLaunchFixture(t, kind, existing)
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					sentinel := errors.New("synthetic-launch-failure")
					started := false
					s.Start = func(context.Context, Plan) (int, error) { started = true; return 0, sentinel }
					var hook func() error
					switch failure {
					case "hook":
						hook = func() error { return sentinel }
					case "cancel-in-hook":
						hook = func() error { cancel(); return nil }
					case "cancel-in-discovery":
						s.FindTools = func(context.Context, map[string]string) []Tool { cancel(); return []Tool{tool} }
					}
					_, err := s.LaunchAuthorized(ctx, a, tool.ID, workspace, nil, hook)
					if err == nil {
						t.Fatal("launch succeeded despite failure")
					}
					if started != (failure == "start") {
						t.Fatalf("unexpected spawn: %v", started)
					}
					if (failure == "cancel-in-hook" || failure == "cancel-in-discovery") && !errors.Is(err, context.Canceled) {
						t.Fatalf("lost cancellation error: %v", err)
					}
					assertIsolatedProfileUnchanged(t, before)
				})
			}
		}
	}
}

func TestIsolatedPrepareLateFailureRestoresBothToolFamilies(t *testing.T) {
	for _, kind := range []string{"codex", "claude"} {
		for _, existing := range []bool{false, true} {
			for _, failure := range []string{"appdata", "npm-shim"} {
				t.Run(kind+"/"+map[bool]string{false: "new", true: "existing"}[existing]+"/"+failure, func(t *testing.T) {
					s, a, tool, workspace, before := isolatedLaunchFixture(t, kind, existing)
					if failure == "appdata" {
						root := filepath.Join(s.Dir, "instances", a.ID)
						if err := os.MkdirAll(root, 0o700); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(filepath.Join(root, tool.ID+"-app"), []byte("synthetic blocker"), 0o600); err != nil {
							t.Fatal(err)
						}
					} else {
						tool.Path = filepath.Join(t.TempDir(), "unresolvable.cmd")
						if err := os.WriteFile(tool.Path, []byte("synthetic npm shim"), 0o600); err != nil {
							t.Fatal(err)
						}
					}
					if _, err := s.Prepare(a, tool, workspace); err == nil {
						t.Fatal("expected preparation failure")
					}
					assertIsolatedProfileUnchanged(t, before)
				})
			}
		}
	}
}

func TestIsolatedLaunchRechecksCancellationAfterWaitingForService(t *testing.T) {
	s, a, tool, workspace, before := isolatedLaunchFixture(t, "codex", false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.mu.Lock()
	done := make(chan error, 1)
	s.Start = func(context.Context, Plan) (int, error) { t.Error("cancelled launch reached Start"); return 1, nil }
	go func() { _, err := s.Launch(ctx, a, tool.ID, workspace, nil); done <- err }()
	cancel()
	s.mu.Unlock()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation: %v", err)
	}
	assertIsolatedProfileUnchanged(t, before)
}

func TestIsolatedLaunchAuthorizedSuccessPersistsPreparedProfile(t *testing.T) {
	s, a, tool, workspace, _ := isolatedLaunchFixture(t, "codex", false)
	authorized := false
	s.Start = func(_ context.Context, p Plan) (int, error) {
		if !authorized {
			t.Error("Start ran before authorization")
		}
		if envValue(p, "OPENAI_API_KEY") != a.Key {
			t.Error("prepared wrong synthetic key")
		}
		return 321, nil
	}
	result, err := s.LaunchAuthorized(context.Background(), a, tool.ID, workspace, nil, func() error { authorized = true; return nil })
	if err != nil || result.PID != 321 {
		t.Fatalf("launch: result=%+v err=%v", result, err)
	}
	if _, err := os.Stat(filepath.Join(result.ProfileDir, "auth.json")); err != nil {
		t.Fatal("successful launch profile was rolled back")
	}
}
