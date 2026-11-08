package console

import (
	"context"
	"os"
	"path/filepath"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/launcher"
	"salcara/bridge/internal/toolcfg"
	"strings"
	"testing"
)

func TestLocalApplyOnlyCommitsLeasedSourceWithoutStarting(t *testing.T) {
	for _, target := range []string{"codex", "claude", "deepseek-harness", "hermes"} {
		t.Run(target, func(t *testing.T) {
			s, h := newTestServer(t)
			home := t.TempDir()
			for _, key := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "DSH_HOME", "HERMES_HOME"} {
				t.Setenv(key, home)
			}
			exe := filepath.Join(t.TempDir(), "fixture-agent.exe")
			if err := os.WriteFile(exe, []byte("fixture"), 0700); err != nil {
				t.Fatal(err)
			}
			s.d.Local.FindTools = func(context.Context, map[string]string) []launcher.Tool {
				return []launcher.Tool{{ID: target, Kind: target, Name: target, Path: exe, Available: true}}
			}
			s.d.Local.Start = func(context.Context, launcher.Plan) (int, error) {
				t.Fatal("apply-only started process")
				return 0, nil
			}
			s.d.Local.Stop = func(context.Context, launcher.Plan) error { t.Fatal("apply-only stopped process"); return nil }
			s.launchModelProbe = func(context.Context, config.LocalAccount) ([]string, error) { return []string{"coding-model"}, nil }
			id := createLocal(t, s, h, "api", "fixture", "upstream-private-fixture", "https://provider.test")
			if err := s.d.Store.Update(func(c *config.Config) error { c.GatewayRuntimeKey = "runtime-fixture"; return nil }); err != nil {
				t.Fatal(err)
			}
			request := map[string]any{"id": id, "target": target, "applyOnly": true}
			if w := localRequest(s, h, "POST", "/api/local/switch", request); w.Code != 400 {
				t.Fatal("bypassed task confirmation")
			}
			request["confirmed"] = true
			w := localRequest(s, h, "POST", "/api/local/switch", request)
			if w.Code != 200 || strings.Contains(w.Body.String(), "upstream-private-fixture") || !strings.Contains(w.Body.String(), `"pid":0`) {
				t.Fatalf("apply: %d %s", w.Code, w.Body)
			}
			c := s.d.Store.Get()
			binding, ok := c.AppliedToolBinding(target)
			if !ok || binding.AccountID != id || binding.LaunchLease == "" {
				t.Fatal("applied source not committed")
			}
			lease := c.LocalGatewayCredential(target)
			if lease == "" || strings.Contains(w.Body.String(), lease) {
				t.Fatal("missing or exposed route lease")
			}
			paths := []string{filepath.Join(home, "settings.json")}
			if target == "codex" {
				paths = []string{filepath.Join(home, "config.toml"), filepath.Join(home, "auth.json")}
			} else if config.IsAdditionalTool(target) {
				paths = toolcfg.AdditionalPaths(target, home)
			}
			found := false
			for _, path := range paths {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(data), "upstream-private-fixture") {
					t.Fatal("wrote provider key to native profile")
				}
				found = found || strings.Contains(string(data), lease)
			}
			if !found {
				t.Fatal("native profile does not carry leased authorization")
			}
		})
	}
}
