package console

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/launcher"
)

func TestProductionRelayAPIAlsoCannotWriteDirectProfiles(t *testing.T) {
	s, h := newTestServer(t)
	id := createLocal(t, s, h, "codex", "relay fixture", "synthetic-relay-private", "https://synthetic.invalid")
	s.d.Store.SetRuntimeGatewayKey("synthetic-relay-runtime")
	for _, row := range []struct {
		path string
		body map[string]any
	}{
		{"/api/local/default", map[string]any{"id": id, "action": "apply"}},
		{"/api/local/launch", map[string]any{"id": id, "target": "codex"}},
	} {
		w := localRequest(s, h, "POST", row.path, row.body)
		if w.Code != http.StatusConflict {
			t.Fatalf("direct relay profile path accepted: %s %d %s", row.path, w.Code, w.Body)
		}
	}
	c := s.d.Store.Get()
	a, _ := c.LocalAccount(id)
	a = c.APIForTool(a, "codex")
	a.GatewayLaunchLease = config.RandomToken(32)
	connection := c.ToolConnection(a, "codex", 47831)
	if connection.Key == "" || connection.Key == a.Key || connection.Key == c.GatewayRuntimeKey || connection.BaseURL != "http://127.0.0.1:47831/gateway/codex" {
		t.Fatal("native-protocol relay bypassed the managed gateway")
	}
}

func TestManagedSwitchPublishesOnlySavedUnchangedSource(t *testing.T) {
	for _, failure := range []string{"none", "account-change", "save"} {
		t.Run(failure, func(t *testing.T) {
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
			id := createLocal(t, s, h, "codex", "relay fixture", "synthetic-relay-private", "https://synthetic.invalid")
			s.d.Store.SetRuntimeGatewayKey("synthetic-launch-runtime")
			var profileKey string
			s.d.Local.Start = func(_ context.Context, p launcher.Plan) (int, error) {
				for _, entry := range p.Environment {
					if strings.HasPrefix(entry, "OPENAI_API_KEY=") {
						profileKey = strings.TrimPrefix(entry, "OPENAI_API_KEY=")
					}
				}
				if profileKey == "" || s.d.Store.Get().LocalGatewayCredential("codex") != profileKey {
					t.Fatal("tool could issue its first request before authorization was committed")
				}
				if failure == "account-change" {
					if err := s.d.Store.Update(func(c *config.Config) error { c.LocalAccounts[0].Key = "synthetic-replaced-private"; return nil }); err != nil {
						t.Fatal(err)
					}
				} else if failure == "save" {
					if err := os.Remove(s.d.Store.Path()); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(s.d.Store.Path(), 0700); err != nil {
						t.Fatal(err)
					}
				}
				return 101, nil
			}
			w := localRequest(s, h, "POST", "/api/local/switch", map[string]any{"id": id, "target": "codex", "confirmed": true})
			if profileKey == "" || profileKey == "synthetic-relay-private" || profileKey == "synthetic-launch-runtime" {
				t.Fatal("launcher used upstream or shared process secret")
			}
			auth, err := os.ReadFile(filepath.Join(root, "auth.json"))
			if err != nil || strings.Contains(string(auth), "synthetic-relay-private") || !strings.Contains(string(auth), profileKey) {
				t.Fatal("profile did not receive exact scoped loopback key")
			}
			current := s.d.Store.Get().LocalGatewayCredential("codex")
			if failure == "none" {
				if w.Code != 200 || current != profileKey {
					t.Fatalf("saved profile not authorized: %d %s", w.Code, w.Body)
				}
			} else if w.Code != 409 || !strings.Contains(w.Body.String(), `"started":true`) || current == profileKey || current != "" {
				t.Fatalf("failed launch authorization claimed success: %d %s", w.Code, w.Body)
			}
		})
	}
}
