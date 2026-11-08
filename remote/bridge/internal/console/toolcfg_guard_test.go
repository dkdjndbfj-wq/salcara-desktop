package console

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
)

func armLegacyToolcfgFixture(t *testing.T, s *Server) {
	t.Helper()
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.RelayRoot = "https://relay.fixture.invalid"
		c.AccountKey = "synthetic-relay-key"
		c.LocalAccounts = []config.LocalAccount{{
			ID: "fixture-account", Name: "Fixture", Kind: "api", SourceType: "reverse_proxy",
			BaseURL: "https://upstream.fixture.invalid", Key: "synthetic-upstream-private", Model: "gpt-fixture",
		}}
		c.ActiveCodexAccount = "fixture-account"
		c.ActiveClaudeAccount = "fixture-account"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.d.Store.SetRuntimeGatewayKey("synthetic-toolcfg-runtime")
}

func TestLegacyToolcfgApplyAndRestoreCannotBypassManagedLease(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	s, h := newTestServer(t)
	armLegacyToolcfgFixture(t, s)
	for _, tc := range []struct {
		tool string
		path string
	}{
		{tool: "codex", path: filepath.Join(root, "config.toml")},
		{tool: "claude", path: filepath.Join(root, "settings.json")},
	} {
		t.Run(tc.tool+"-apply", func(t *testing.T) {
			w := localRequest(s, h, http.MethodPost, "/api/toolcfg", map[string]any{"tool": tc.tool, "action": "apply"})
			if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "工具工作台") {
				t.Fatalf("legacy %s apply was not fenced: %d %s", tc.tool, w.Code, w.Body)
			}
			if _, err := os.Stat(tc.path); !os.IsNotExist(err) {
				t.Fatalf("legacy %s apply wrote native profile: %v", tc.tool, err)
			}
		})
		t.Run(tc.tool+"-restore", func(t *testing.T) {
			w := localRequest(s, h, http.MethodPost, "/api/toolcfg", map[string]any{"tool": tc.tool, "action": "restore"})
			if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "工具工作台") {
				t.Fatalf("legacy %s restore was not fenced: %d %s", tc.tool, w.Code, w.Body)
			}
		})
	}
}

func TestToolcfgStatusExposesManagedBoundary(t *testing.T) {
	s, h := newTestServer(t)
	armLegacyToolcfgFixture(t, s)
	w := localRequest(s, h, http.MethodGet, "/api/toolcfg", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("toolcfg status: %d %s", w.Code, w.Body)
	}
	var out struct {
		Legacy struct {
			Managed bool `json:"managed"`
			Codex   struct {
				RequiresWorkbench bool `json:"requiresWorkbench"`
			} `json:"codex"`
		} `json:"legacyToolcfg"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Legacy.Managed || !out.Legacy.Codex.RequiresWorkbench {
		t.Fatalf("status did not expose managed boundary: %s", w.Body)
	}
}

func TestLegacyToolcfgAllowsExplicitUnmanagedCompatibilityOnly(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	s, _ := newTestServer(t)
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.RelayRoot = "https://relay.fixture.invalid"
		c.AccountKey = "synthetic-relay-key"
		c.LocalAccounts = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if blocked, _ := legacyToolcfgMutationBlock(s.d.Store.Get(), "codex"); blocked {
		t.Fatal("unmanaged compatibility path unexpectedly reports managed")
	}
}
