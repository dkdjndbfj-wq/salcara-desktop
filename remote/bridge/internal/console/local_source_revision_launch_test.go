package console

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/launcher"
)

func TestLocalSourceRevisionRejectsChangedConfirmationBeforeMutation(t *testing.T) {
	for _, route := range []string{"/api/local/bind", "/api/local/switch", "/api/local/claude-desktop/switch"} {
		for _, change := range []string{"same-mask-key", "empty", "removed", "process"} {
			t.Run(route+"/"+change, func(t *testing.T) {
				s, h, id := sourceRevisionFixture(t)
				revision, _ := listedSourceRevision(t, s, h, id)
				probes, starts := 0, 0
				s.launchModelProbe = func(context.Context, config.LocalAccount) ([]string, error) {
					probes++
					return []string{"coding-model"}, nil
				}
				s.d.Local.Start = func(context.Context, launcher.Plan) (int, error) { starts++; return 1, nil }
				switch change {
				case "same-mask-key":
					updateSourceFixture(t, s, id, func(a *config.LocalAccount) { a.Key = "same-head-bbbbbbbb-private-middle-same-tail" })
				case "empty":
					revision = ""
				case "removed":
					if err := s.d.Store.Update(func(c *config.Config) error { c.LocalAccounts = nil; return nil }); err != nil {
						t.Fatal(err)
					}
				case "process":
					s.d.Store.SetRuntimeGatewayKey("synthetic-next-runtime")
				}
				w := localRequest(s, h, http.MethodPost, route, map[string]any{"id": id, "target": "codex", "model": "new-model", "override": true, "confirmed": true, "allowModeChange": true, "expectedSourceRevision": revision})
				if w.Code < 400 || probes != 0 || starts != 0 {
					t.Fatalf("changed confirmation performed work: status=%d probes=%d starts=%d", w.Code, probes, starts)
				}
				if change != "removed" && w.Code != http.StatusConflict {
					t.Fatalf("expected conflict, got %d", w.Code)
				}
				c := s.d.Store.Get()
				if len(c.ToolAPISelections) != 0 || len(c.ToolAPIApplied) != 0 || len(c.ToolModels) != 0 {
					t.Fatal("rejected confirmation changed tool selection")
				}
			})
		}
	}
}

func TestLocalSourceRevisionConfirmedBindAndCatalogRefreshRemainCompatible(t *testing.T) {
	s, h, id := sourceRevisionFixture(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	exe := filepath.Join(t.TempDir(), "synthetic-codex.exe")
	if err := os.WriteFile(exe, []byte("not-an-executable"), 0600); err != nil {
		t.Fatal(err)
	}
	s.d.Local.FindTools = func(context.Context, map[string]string) []launcher.Tool {
		return []launcher.Tool{{ID: "codex", Kind: "codex", Available: true, Path: exe}}
	}
	probes, starts := 0, 0
	s.launchModelProbe = func(context.Context, config.LocalAccount) ([]string, error) {
		probes++
		return []string{"gpt-new-fixture"}, nil
	}
	s.d.Local.Start = func(context.Context, launcher.Plan) (int, error) { starts++; return 123, nil }
	revision, _ := listedSourceRevision(t, s, h, id)
	w := localRequest(s, h, http.MethodPost, "/api/local/bind", map[string]any{"id": id, "target": "codex", "model": "coding-model", "expectedSourceRevision": revision})
	if w.Code != http.StatusOK {
		t.Fatalf("matching bind failed: %d", w.Code)
	}
	if afterBind, _ := listedSourceRevision(t, s, h, id); afterBind != revision {
		t.Fatal("binding changed source revision")
	}
	w = localRequest(s, h, http.MethodPost, "/api/local/switch", map[string]any{"id": id, "target": "codex", "confirmed": true, "keepModel": true, "workspace": t.TempDir(), "expectedSourceRevision": revision})
	if w.Code != http.StatusOK || probes != 1 || starts != 1 {
		t.Fatalf("owned catalog refresh rejected: status=%d probes=%d starts=%d", w.Code, probes, starts)
	}
	c := s.d.Store.Get()
	a, ok := c.AuthorizedAppliedToolAccount("codex")
	if !ok || a.ID != id || a.Model != "gpt-new-fixture" {
		t.Fatal("confirmed source not applied with new catalog")
	}
	// Omission stays compatible for the existing workbench caller.
	w = localRequest(s, h, http.MethodPost, "/api/local/bind", map[string]any{"id": id, "target": "codex", "model": "gpt-new-fixture"})
	if w.Code != http.StatusOK {
		t.Fatal("legacy bind no longer compatible")
	}
}

func TestLocalSourceRevisionRejectsProcessChangedDuringCatalog(t *testing.T) {
	for _, route := range []string{"/api/local/switch", "/api/local/claude-desktop/switch"} {
		t.Run(route, func(t *testing.T) {
			s, h, id := sourceRevisionFixture(t)
			revision, _ := listedSourceRevision(t, s, h, id)
			starts := 0
			s.launchModelProbe = func(context.Context, config.LocalAccount) ([]string, error) {
				s.d.Store.SetRuntimeGatewayKey("synthetic-process-changed-during-catalog")
				return []string{"coding-model"}, nil
			}
			s.d.Local.Start = func(context.Context, launcher.Plan) (int, error) { starts++; return 1, nil }
			w := localRequest(s, h, http.MethodPost, route, map[string]any{"id": id, "target": "codex", "confirmed": true, "allowModeChange": true, "expectedSourceRevision": revision})
			if w.Code != http.StatusConflict || starts != 0 || len(s.d.Store.Get().ToolAPIApplied) != 0 {
				t.Fatal("launch adopted a later process source")
			}
		})
	}
}

func TestLocalSourceRevisionRechecksFinalDesktopAuthorization(t *testing.T) {
	for _, target := range []string{"codex-desktop", "claude-desktop"} {
		t.Run(target, func(t *testing.T) {
			var s *Server
			var h http.Handler
			var id string
			route := "/api/local/switch"
			if target == "claude-desktop" {
				s, h, _, _, _ = consoleClaude3PFixture(t)
				id = "shared-api"
				s.d.Store.SetRuntimeGatewayKey("synthetic-claude-source-runtime")
				route = "/api/local/claude-desktop/switch"
			} else {
				s, h, id = sourceRevisionFixture(t)
				t.Setenv("CODEX_HOME", t.TempDir())
				exe := filepath.Join(t.TempDir(), "synthetic-codex.exe")
				if err := os.WriteFile(exe, []byte("not-an-executable"), 0600); err != nil {
					t.Fatal(err)
				}
				s.d.Local.FindTools = func(context.Context, map[string]string) []launcher.Tool {
					return []launcher.Tool{{ID: target, Kind: "codex", Available: true, Path: exe}}
				}
			}
			s.launchModelProbe = func(context.Context, config.LocalAccount) ([]string, error) { return []string{"coding-model"}, nil }
			revision, _ := listedSourceRevision(t, s, h, id)
			stops, starts := 0, 0
			s.d.Local.Stop = func(context.Context, launcher.Plan) error {
				stops++
				updateSourceFixture(t, s, id, func(a *config.LocalAccount) { a.Key = "synthetic-replaced-during-stop" })
				return nil
			}
			s.d.Local.Start = func(context.Context, launcher.Plan) (int, error) { starts++; return 1, nil }
			w := localRequest(s, h, http.MethodPost, route, map[string]any{"id": id, "target": target, "confirmed": true, "allowModeChange": true, "workspace": t.TempDir(), "expectedSourceRevision": revision})
			if w.Code < 400 || stops != 1 || starts != 0 {
				t.Fatalf("final source check missed replacement: status=%d stops=%d starts=%d", w.Code, stops, starts)
			}
			if _, ok := s.d.Store.Get().AuthorizedAppliedToolAccount(target); ok {
				t.Fatal("replaced source received a launch lease")
			}
		})
	}
}
