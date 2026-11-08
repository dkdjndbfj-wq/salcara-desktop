package console

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/launcher"
	"salcara/bridge/internal/toolcfg"
)

func restoreDefaultFixture(t *testing.T, kind string) (*Server, http.Handler, string, string) {
	t.Helper()
	s, h := newTestServer(t)
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	path := toolcfg.ClaudeSettingsPath()
	original := `{"keep":"original"}`
	if kind == "codex" {
		path = toolcfg.CodexConfigPath()
		original = "# original configuration\n"
	}
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	s.d.Store.SetRuntimeGatewayKey("synthetic-restore-runtime")
	a := config.LocalAccount{ID: "restore-account", Name: "Restore fixture", Kind: kind, BaseURL: "https://synthetic.invalid", Key: "synthetic-upstream-private", Model: "gpt-fixture", GatewayLaunchLease: "synthetic-original-launch"}
	if err := s.d.Store.Update(func(c *config.Config) error { c.LocalAccounts = []config.LocalAccount{a}; return nil }); err != nil {
		t.Fatal(err)
	}
	a = s.d.Store.Get().APIForTool(a, kind)
	a.GatewayLaunchLease = "synthetic-original-launch"
	if err := s.d.Local.ApplyDefault(s.d.Store.Get().ToolConnection(a, kind, 47831)); err != nil {
		t.Fatal(err)
	}
	if err := s.d.Store.Update(func(c *config.Config) error { c.RecordAppliedAPI(kind, a, "gateway"); return nil }); err != nil {
		t.Fatal(err)
	}
	return s, h, path, s.d.Store.Get().LocalGatewayCredential(kind)
}

func assertRestoredKeyDenied(t *testing.T, h http.Handler, target, key string) {
	t.Helper()
	r := httptest.NewRequest("GET", "/gateway/"+target+"/v1/models", nil)
	r.Host, r.RemoteAddr = "127.0.0.1:47831", "127.0.0.1:3456"
	r.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 && w.Code != 503 {
		t.Fatalf("restored profile retained gateway authorization: %d %s", w.Code, w.Body)
	}
}

func blockRestoreSave(t *testing.T, s *Server) {
	t.Helper()
	if err := os.Remove(s.d.Store.Path()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.d.Store.Path(), 0700); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultRestoreRetiresKeyBeforeWritesAndSurvivesPersistenceFailure(t *testing.T) {
	for _, kind := range []string{"codex", "claude"} {
		for _, failure := range []string{"none", "write", "save"} {
			t.Run(kind+"-"+failure, func(t *testing.T) {
				s, h, path, key := restoreDefaultFixture(t, kind)
				applied, _ := os.ReadFile(path)
				retired := false
				s.d.Store.OnChange(func(old, next config.Config) {
					if old.ToolAPIApplied[kind].LaunchLease != "" && next.ToolAPIApplied[kind].LaunchLease == "" {
						retired = true
						current, _ := os.ReadFile(path)
						if !bytes.Equal(current, applied) {
							t.Fatal("restore wrote profile before revoking key")
						}
						if next.LocalGatewayCredential(kind) != "" {
							t.Fatal("revocation did not retire local credential")
						}
						if failure == "write" {
							if err := os.Remove(path); err != nil {
								t.Fatal(err)
							}
							if err := os.Mkdir(path, 0700); err != nil {
								t.Fatal(err)
							}
							if err := os.WriteFile(filepath.Join(path, "block"), []byte("fixture"), 0600); err != nil {
								t.Fatal(err)
							}
						}
						if failure == "save" {
							blockRestoreSave(t, s)
						}
					}
				})
				w := localRequest(s, h, "POST", "/api/local/default", map[string]string{"kind": kind, "action": "restore"})
				want := 200
				if failure != "none" {
					want = 400
				}
				if w.Code != want || !retired || s.d.Store.Get().LocalGatewayCredential(kind) != "" {
					t.Fatalf("failed restore kept lease: %s %d %s", failure, w.Code, w.Body)
				}
				assertRestoredKeyDenied(t, h, kind, key)
				if failure == "save" && !strings.Contains(w.Body.String(), "已撤销") {
					t.Fatal("save failure hid runtime retirement")
				}
			})
		}
	}
}

func TestDefaultRestoreDoesNotRevokeWhenExternalEditsRejectIt(t *testing.T) {
	s, h, path, key := restoreDefaultFixture(t, "codex")
	if err := os.WriteFile(path, []byte("# later external edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	w := localRequest(s, h, "POST", "/api/local/default", map[string]string{"kind": "codex", "action": "restore"})
	if w.Code != 400 || s.d.Store.Get().LocalGatewayCredential("codex") != key {
		t.Fatal("rejected external modification retired unchanged connection")
	}
}

func TestDefaultRestoreOldCleanupCannotDeleteNewAuthorization(t *testing.T) {
	s, h, _, oldKey := restoreDefaultFixture(t, "codex")
	s.d.Store.OnChange(func(old, next config.Config) {
		if old.ToolAPIApplied["codex"].LaunchLease != "" && next.ToolAPIApplied["codex"].LaunchLease == "" {
			if err := s.d.Store.Update(func(c *config.Config) error {
				a := c.APIForTool(c.LocalAccounts[0], "codex")
				a.GatewayLaunchLease = "synthetic-later-apply"
				c.RecordAppliedAPI("codex", a, "gateway")
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	})
	w := localRequest(s, h, "POST", "/api/local/default", map[string]string{"kind": "codex", "action": "restore"})
	c := s.d.Store.Get()
	if w.Code != 200 || c.ToolAPIApplied["codex"].LaunchLease != "synthetic-later-apply" || c.LocalGatewayCredential("codex") == "" {
		t.Fatal("older restore cleanup deleted newer application")
	}
	assertRestoredKeyDenied(t, h, "codex", oldKey)
}

func TestClaudeRestoreRetiresBeforeRestartAndNeverResurrectsOnFailure(t *testing.T) {
	for _, failure := range []string{"none", "write", "start", "save", "new-apply", "external-on-quit"} {
		t.Run(failure, func(t *testing.T) {
			s, h, hdr, root, _ := consoleClaude3PFixture(t)
			s.d.Store.SetRuntimeGatewayKey("synthetic-claude-restore-runtime")
			if w := do(h, "POST", "/api/local/claude-desktop/switch", "127.0.0.1:47831", hdr, `{"id":"shared-api","confirmed":true,"allowModeChange":true}`); w.Code != 200 {
				t.Fatal(w.Body)
			}
			key := s.d.Store.Get().LocalGatewayCredential("claude-desktop")
			retired, starts := false, 0
			s.d.Store.OnChange(func(old, next config.Config) {
				if old.ToolAPIApplied["claude-desktop"].LaunchLease != "" && next.ToolAPIApplied["claude-desktop"].LaunchLease == "" {
					retired = true
					if failure == "write" {
						path := filepath.Join(root, "claude_desktop_config.json")
						if err := os.Remove(path); err != nil {
							t.Fatal(err)
						}
						if err := os.Mkdir(path, 0700); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(filepath.Join(path, "block"), []byte("fixture"), 0600); err != nil {
							t.Fatal(err)
						}
					}
				}
			})
			if failure == "external-on-quit" {
				s.d.Local.Stop = func(context.Context, launcher.Plan) error {
					if err := os.WriteFile(filepath.Join(root, "claude_desktop_config.json"), []byte(`{"deploymentMode":"3p","external":true}`), 0600); err != nil {
						t.Fatal(err)
					}
					return nil
				}
			}
			s.d.Local.Start = func(context.Context, launcher.Plan) (int, error) {
				starts++
				if !retired || s.d.Store.Get().LocalGatewayCredential("claude-desktop") != "" {
					t.Fatal("restarted restored Claude with old gateway access")
				}
				if failure == "save" {
					blockRestoreSave(t, s)
				}
				if failure == "new-apply" {
					if err := s.d.Store.Update(func(c *config.Config) error {
						a := c.APIForTool(c.LocalAccounts[0], "claude-desktop")
						a.CatalogOverride = true
						a.GatewayLaunchLease = "synthetic-new-claude-apply"
						c.RecordAppliedAPI("claude-desktop", a, "gateway")
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				if failure == "start" {
					return 0, errors.New("synthetic start failure")
				}
				return 123, nil
			}
			w := do(h, "POST", "/api/local/claude-desktop/restore", "127.0.0.1:47831", hdr, `{"confirmed":true}`)
			if failure == "external-on-quit" {
				if w.Code != 400 || retired || starts != 0 || s.d.Store.Get().LocalGatewayCredential("claude-desktop") != key {
					t.Fatal("external change revoked before checks")
				}
				return
			}
			want := 200
			if failure == "write" || failure == "start" {
				want = 400
			}
			if w.Code != want || !retired {
				t.Fatalf("restore result %s: %d %s", failure, w.Code, w.Body)
			}
			c := s.d.Store.Get()
			if failure == "new-apply" {
				if c.ToolAPIApplied["claude-desktop"].LaunchLease != "synthetic-new-claude-apply" || c.LocalGatewayCredential("claude-desktop") == "" {
					t.Fatal("old cleanup deleted new Claude authorization")
				}
			} else if c.LocalGatewayCredential("claude-desktop") != "" {
				t.Fatal("restore failure resurrected old Claude connection")
			}
			assertRestoredKeyDenied(t, h, "claude-desktop", key)
			if failure == "write" && starts != 0 {
				t.Fatal("restart after restore-write failure")
			}
			if failure == "save" && !strings.Contains(w.Body.String(), "已撤销") {
				t.Fatal("save failure hid authorization retirement")
			}
		})
	}
}

func waitForRestoreMutex(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		stack := make([]byte, 1<<20)
		n := runtime.Stack(stack, true)
		if bytes.Contains(stack[:n], []byte("handleLocalDefault")) && bytes.Contains(stack[:n], []byte("RestoreDefaultAuthorized")) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("restore did not wait for the in-flight local switch")
}

func TestDefaultRestoreReadsLeaseAfterWaitingForInflightApply(t *testing.T) {
	s, h, _, oldKey := restoreDefaultFixture(t, "codex")
	exe := filepath.Join(t.TempDir(), "synthetic-codex.exe")
	if err := os.WriteFile(exe, []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	s.d.Local.FindTools = func(context.Context, map[string]string) []launcher.Tool {
		close(entered)
		<-release
		return []launcher.Tool{{ID: "codex", Kind: "codex", Name: "Codex", Path: exe, Available: true}}
	}
	s.d.Local.Start = func(context.Context, launcher.Plan) (int, error) { return 123, nil }
	c := s.d.Store.Get()
	a := c.APIForTool(c.LocalAccounts[0], "codex")
	a.GatewayLaunchLease = "synthetic-intervening-apply"
	switchDone := make(chan error, 1)
	go func() {
		_, err := s.d.Local.SwitchAuthorized(context.Background(), c.ToolConnection(a, "codex", 47831), "codex", t.TempDir(), nil, func() error {
			return s.d.Store.Update(func(c *config.Config) error { c.RecordAppliedAPI("codex", a, "gateway"); return nil })
		})
		switchDone <- err
	}()
	<-entered
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response <- localRequest(s, h, "POST", "/api/local/default", map[string]string{"kind": "codex", "action": "restore"})
	}()
	waitForRestoreMutex(t)
	close(release)
	if err := <-switchDone; err != nil {
		t.Fatal(err)
	}
	select {
	case w := <-response:
		if w.Code != 200 {
			t.Fatal(w.Body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("restore remained blocked")
	}
	if s.d.Store.Get().LocalGatewayCredential("codex") != "" {
		t.Fatal("restore captured endpoint's old nonce and missed the newer apply")
	}
	assertRestoredKeyDenied(t, h, "codex", oldKey)
}
