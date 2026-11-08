package console

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/launcher"
)

func TestManagedLaunchAuthorizationFailureDoesNotStartAndStartFailureRevokes(t *testing.T) {
	for _, phase := range []string{"authorize-save", "start", "start-save"} {
		t.Run(phase, func(t *testing.T) {
			s, h := newTestServer(t)
			root := t.TempDir()
			t.Setenv("CODEX_HOME", root)
			exe := filepath.Join(t.TempDir(), "fixture-codex.exe")
			if err := os.WriteFile(exe, []byte("fixture"), 0700); err != nil {
				t.Fatal(err)
			}
			s.d.Local.FindTools = func(context.Context, map[string]string) []launcher.Tool {
				return []launcher.Tool{{ID: "codex", Kind: "codex", Name: "Codex CLI", Path: exe, Available: true}}
			}
			id := createLocal(t, s, h, "codex", "fixture", "synthetic-private", "https://synthetic.invalid")
			s.d.Store.SetRuntimeGatewayKey("synthetic-process")
			blockSave := func() {
				if err := os.Remove(s.d.Store.Path()); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(s.d.Store.Path(), 0700); err != nil {
					t.Fatal(err)
				}
			}
			starts := 0
			s.d.Local.Start = func(context.Context, launcher.Plan) (int, error) {
				starts++
				if s.d.Store.Get().LocalGatewayCredential("codex") == "" {
					t.Fatal("connection not ready at start")
				}
				if phase == "start-save" {
					blockSave()
				}
				return 0, errors.New("synthetic start failure")
			}
			if phase == "authorize-save" {
				blockSave()
			}
			w := localRequest(s, h, "POST", "/api/local/switch", map[string]any{"id": id, "target": "codex", "confirmed": true})
			wantStarts := 1
			if phase == "authorize-save" {
				wantStarts = 0
			}
			if w.Code != 400 || starts != wantStarts || s.d.Store.Get().LocalGatewayCredential("codex") != "" {
				t.Fatalf("failed launch retained authorization: %s %d %d", phase, w.Code, starts)
			}
			if _, err := os.Stat(filepath.Join(root, "auth.json")); !os.IsNotExist(err) {
				t.Fatal("failed launch did not restore original profile")
			}
		})
	}
}

func TestRuntimeRevocationIgnoresOlderLaunchAndWorksWithoutWritableConfig(t *testing.T) {
	s, _ := newTestServer(t)
	if err := s.d.Store.Update(func(c *config.Config) error {
		a := config.LocalAccount{ID: config.NewUUID(), Kind: "codex", Name: "fixture", BaseURL: "https://synthetic.invalid", Key: "synthetic-private", Model: "gpt-fixture"}
		c.LocalAccounts = []config.LocalAccount{a}
		c.RecordAppliedAPI("codex", a, "gateway")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c := s.d.Store.Get()
	lease := c.ToolAPIApplied["codex"].LaunchLease
	epoch := c.GatewayAuthorizationRevision("codex")
	s.d.Store.RevokeAppliedLaunchLease("codex", "older-lease")
	if s.d.Store.Get().ToolAPIApplied["codex"].LaunchLease != lease {
		t.Fatal("old failure revoked new connection")
	}
	if err := os.Remove(s.d.Store.Path()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.d.Store.Path(), 0700); err != nil {
		t.Fatal(err)
	}
	s.d.Store.RevokeAppliedLaunchLease("codex-desktop", lease)
	c = s.d.Store.Get()
	if c.ToolAPIApplied["codex"].LaunchLease != "" || c.GatewayAuthorizationRevision("codex") == epoch {
		t.Fatal("runtime revocation failed to fence old requests")
	}
}
