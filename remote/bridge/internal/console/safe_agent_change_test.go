package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/launcher"
	"salcara/bridge/internal/toolcfg"
)

type safeChangeWorker struct {
	*sessionAgent
	busy  bool
	stops int
	stop  func(context.Context) error
}

func (a *safeChangeWorker) ActiveRemoteTurns() bool { return a.busy }
func (a *safeChangeWorker) StopForAPIChange(ctx context.Context) error {
	a.stops++
	if a.stop != nil {
		return a.stop(ctx)
	}
	a.busy = false
	return ctx.Err()
}

func safeChangeFixture(t *testing.T) (*Server, http.Handler, *safeChangeWorker, *int, map[string]any) {
	t.Helper()
	s, h, starts := managedLaunchFixture(t)
	worker := &safeChangeWorker{sessionAgent: &sessionAgent{kind: "codex"}, busy: true}
	s.SetManager(consoleCatalogManager{agent: worker})
	s.nativeConfigurationStop = func(context.Context, launcher.Plan) error { return nil }
	c := s.d.Store.Get()
	a, _ := c.LocalAccount("upstream")
	body := map[string]any{"protocol": safeAgentChangeProtocol, "operationId": "11111111-1111-4111-8111-111111111111", "target": "codex", "id": a.ID, "model": "gpt-fixture", "wireProtocol": "responses", "override": true, "workspace": t.TempDir(), "expectedSourceRevision": c.AccountSourceRevision(a)}
	return s, h, worker, starts, body
}

func TestSafeAgentChangeStopsBeforeApplyingAndRejectsReplay(t *testing.T) {
	s, h, worker, starts, body := safeChangeFixture(t)
	order := []string{}
	worker.stop = func(context.Context) error { order = append(order, "adapter-stop"); worker.busy = false; return nil }
	s.nativeConfigurationStop = func(context.Context, launcher.Plan) error {
		order = append(order, "native-stop")
		if s.d.Store.Get().ToolAPISelections["codex"] != "" {
			t.Fatal("selection changed before stop")
		}
		if _, err := os.Stat(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml")); !os.IsNotExist(err) {
			t.Fatal("native config written before stop")
		}
		return nil
	}
	s.d.Local.Start = func(context.Context, launcher.Plan) (int, error) {
		order = append(order, "start")
		*starts++
		return 123, nil
	}
	w := localRequest(s, h, "POST", "/api/local/agent-change/v1", body)
	if w.Code != 200 || *starts != 1 || worker.stops != 1 || !reflect.DeepEqual(order, []string{"adapter-stop", "native-stop", "native-stop", "start"}) {
		t.Fatalf("%d %s order=%v", w.Code, w.Body, order)
	}
	var result map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	if result["operationId"] != body["operationId"] || result["protocol"] != safeAgentChangeProtocol || result["ok"] != true {
		t.Fatal("missing versioned acknowledgement", result)
	}
	if w = localRequest(s, h, "POST", "/api/local/agent-change/v1", body); w.Code != 409 || *starts != 1 || worker.stops != 1 {
		t.Fatal("replay executed twice", w.Code, w.Body)
	}
}

func TestSafeAgentChangeValidationAndUnconfirmedStopNeverWrite(t *testing.T) {
	for _, mode := range []string{"protocol", "target", "source", "model", "relative-workspace", "missing-workspace", "missing-tool", "unknown", "stop-error", "native-stop-error", "source-during-stop", "source-during-catalog"} {
		t.Run(mode, func(t *testing.T) {
			s, h, worker, starts, body := safeChangeFixture(t)
			before := s.d.Store.Get()
			switch mode {
			case "protocol":
				body["protocol"] = "old"
			case "target":
				body["target"] = "unrelated"
			case "source":
				body["expectedSourceRevision"] = "stale"
			case "model":
				body["model"] = "absent-model"
			case "relative-workspace":
				body["workspace"] = "relative-workspace"
			case "missing-workspace":
				body["workspace"] = filepath.Join(t.TempDir(), "missing")
			case "missing-tool":
				s.d.Local.FindTools = func(context.Context, map[string]string) []launcher.Tool { return nil }
			case "unknown":
				worker.stop = func(context.Context) error { return nil }
			case "stop-error":
				worker.stop = func(context.Context) error { return errors.New("synthetic stop failure") }
			case "native-stop-error":
				s.nativeConfigurationStop = func(context.Context, launcher.Plan) error { return errors.New("synthetic native failure") }
			case "source-during-stop":
				worker.stop = func(context.Context) error {
					worker.busy = false
					return s.d.Store.Update(func(c *config.Config) error { c.LocalAccounts[0].Key = "replacement"; return nil })
				}
			case "source-during-catalog":
				s.launchModelProbe = func(context.Context, config.LocalAccount) ([]string, error) {
					_ = s.d.Store.Update(func(c *config.Config) error { c.LocalAccounts[0].Key = "replacement"; return nil })
					return []string{"gpt-fixture"}, nil
				}
			}
			w := localRequest(s, h, "POST", "/api/local/agent-change/v1", body)
			now := s.d.Store.Get()
			if w.Code < 400 || *starts != 0 || !reflect.DeepEqual(before.ToolAPISelections, now.ToolAPISelections) || !reflect.DeepEqual(before.ToolAPIApplied, now.ToolAPIApplied) {
				t.Fatalf("failed change wrote config: %d %s", w.Code, w.Body)
			}
			if _, err := os.Stat(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml")); !os.IsNotExist(err) {
				t.Fatal("failed stop wrote native config")
			}
			if (mode == "protocol" || mode == "target" || mode == "source" || mode == "model" || mode == "relative-workspace" || mode == "missing-workspace" || mode == "missing-tool") && worker.stops != 0 {
				t.Fatal("invalid request stopped a task")
			}
		})
	}
}

func TestSafeAgentChangeAllTargetsApplyOnlyPreservesHistory(t *testing.T) {
	for _, target := range []string{"codex", "codex-desktop", "claude", "claude-desktop", "deepseek-harness", "hermes"} {
		t.Run(target, func(t *testing.T) {
			s, h, worker, starts, body := safeChangeFixture(t)
			profiles := map[string]string{}
			for _, variable := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "DSH_HOME", "HERMES_HOME", "CODEX_ELECTRON_USER_DATA_PATH"} {
				profiles[variable] = t.TempDir()
				t.Setenv(variable, profiles[variable])
			}
			root := profiles["CODEX_HOME"]
			switch target {
			case "claude":
				root = profiles["CLAUDE_CONFIG_DIR"]
			case "claude-desktop":
				root = t.TempDir()
			case "deepseek-harness":
				root = profiles["DSH_HOME"]
			case "hermes":
				root = profiles["HERMES_HOME"]
			}
			sentinel := filepath.Join(root, "history-must-stay.txt")
			_ = os.WriteFile(sentinel, []byte("previous phone and desktop conversation"), 0600)
			tool := s.d.Local.Tools(context.Background(), nil)[0]
			tool.ID, tool.Kind = target, config.ToolKind(target)
			s.d.Local.FindTools = func(context.Context, map[string]string) []launcher.Tool { return []launcher.Tool{tool} }
			if target == "claude-desktop" {
				s.d.ClaudeDesktop = launcher.NewClaudeDesktop3P(s.d.Local)
				s.d.ClaudeDesktop.Platform = func(context.Context, launcher.Tool) (launcher.Claude3PPlatform, error) {
					return launcher.Claude3PPlatform{Root: root, Version: toolcfg.ClaudeDesktop3PVersion}, nil
				}
			}
			worker.kind = config.ToolFamily(target)
			body["target"], body["applyOnly"], body["allowModeChange"], body["wireProtocol"] = target, true, true, config.NativeProtocol(config.ToolKind(target))
			w := localRequest(s, h, "POST", "/api/local/agent-change/v1", body)
			if w.Code != 200 || *starts != 0 || worker.stops != 1 {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			if binding, ok := s.d.Store.Get().AppliedToolBinding(target); !ok || binding.LaunchLease == "" {
				t.Fatal("missing applied lease")
			}
			if text, _ := os.ReadFile(sentinel); string(text) != "previous phone and desktop conversation" {
				t.Fatal("history overwritten")
			}
		})
	}
}

func TestSafeAgentChangeApplyOnlyAndCancelledStop(t *testing.T) {
	for _, cancelAtStop := range []bool{false, true} {
		t.Run(map[bool]string{false: "apply-only", true: "cancelled-stop"}[cancelAtStop], func(t *testing.T) {
			s, h, worker, starts, body := safeChangeFixture(t)
			body["applyOnly"] = true
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if cancelAtStop {
				worker.stop = func(context.Context) error { worker.busy = false; cancel(); return nil }
			}
			data, _ := json.Marshal(body)
			r := httptest.NewRequest("POST", "/api/local/agent-change/v1", strings.NewReader(string(data))).WithContext(ctx)
			r.Host = "127.0.0.1:47831"
			r.Header.Set("Cookie", CookieName+"="+s.Token())
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if *starts != 0 || worker.stops != 1 {
				t.Fatal("apply-only started a process")
			}
			if cancelAtStop {
				if w.Code < 400 || s.d.Store.Get().ToolAPISelections["codex"] != "" {
					t.Fatal("cancelled stop applied config")
				}
			} else if w.Code != 200 || s.d.Store.Get().ToolAPISelections["codex"] != "upstream" {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
		})
	}
}

func TestSafeAgentChangeSuccessfulStartSurvivesHTTPCancellation(t *testing.T) {
	s, h, _, starts, body := safeChangeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.d.Local.Start = func(context.Context, launcher.Plan) (int, error) { *starts++; cancel(); return 123, nil }
	data, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", "/api/local/agent-change/v1", strings.NewReader(string(data))).WithContext(ctx)
	r.Host = "127.0.0.1:47831"
	r.Header.Set("Cookie", CookieName+"="+s.Token())
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	binding, exists := s.d.Store.Get().AppliedToolBinding("codex")
	if w.Code != 200 || *starts != 1 || !exists || binding.LaunchLease == "" {
		t.Fatalf("cancelled HTTP revoked running process: %d %s", w.Code, w.Body)
	}
}

func TestSafeManagedLaunchApplyOnlyUsesTargetFamily(t *testing.T) {
	s, h, starts := managedLaunchFixture(t)
	worker := &safeChangeWorker{sessionAgent: &sessionAgent{kind: "codex"}, busy: true}
	s.SetManager(consoleCatalogManager{agent: worker})
	s.nativeConfigurationStop = func(context.Context, launcher.Plan) error { return nil }
	revision := managedPreview(t, s, h)
	w := localRequest(s, h, "POST", "/api/local/codex-api/launch/safe-v1", map[string]any{"protocol": safeAgentChangeProtocol, "operationId": "22222222-2222-4222-8222-222222222222", "clientId": "client", "target": "codex", "revision": revision, "catalogOverride": true, "workspace": t.TempDir(), "applyOnly": true})
	if w.Code != 200 || *starts != 0 || worker.stops != 1 || s.d.Store.Get().ManagedAPICredential("codex") == "" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestSafeAgentChangeFailedStartRestoresSelection(t *testing.T) {
	s, h, _, _, body := safeChangeFixture(t)
	before := s.d.Store.Get()
	s.d.Local.Start = func(context.Context, launcher.Plan) (int, error) {
		return 0, errors.New("synthetic definite start failure")
	}
	w := localRequest(s, h, "POST", "/api/local/agent-change/v1", body)
	now := s.d.Store.Get()
	if w.Code != 409 || len(now.ToolAPISelections) != len(before.ToolAPISelections) || now.ToolAPISelections["codex"] != before.ToolAPISelections["codex"] || len(now.ToolAPIApplied) != len(before.ToolAPIApplied) {
		t.Fatalf("failed start retained new selection: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml")); !os.IsNotExist(err) {
		t.Fatal("failed start retained native config")
	}
}

func TestSafeAgentChangeUncertainStartAndRecordFailureKeepLease(t *testing.T) {
	for _, mode := range []string{"submitted", "record-source-changed"} {
		t.Run(mode, func(t *testing.T) {
			s, h, _, _, body := safeChangeFixture(t)
			s.d.Local.Start = func(context.Context, launcher.Plan) (int, error) {
				if mode == "submitted" {
					return 0, nil
				}
				if err := s.d.Store.Update(func(c *config.Config) error { c.ToolAPISelections["codex"] = ""; return nil }); err != nil {
					t.Fatal(err)
				}
				return 123, nil
			}
			w := localRequest(s, h, "POST", "/api/local/agent-change/v1", body)
			binding, ok := s.d.Store.Get().AppliedToolBinding("codex")
			if !ok || binding.LaunchLease == "" {
				t.Fatalf("committed process lost lease: %d %s", w.Code, w.Body)
			}
			if mode == "submitted" {
				if w.Code != 200 || !strings.Contains(w.Body.String(), `"uncertain":true`) || !strings.Contains(w.Body.String(), `"submitted":true`) {
					t.Fatal(w.Code, w.Body)
				}
			} else if w.Code != 409 || !strings.Contains(w.Body.String(), `"started":true`) || !strings.Contains(w.Body.String(), `"applied":true`) || !strings.Contains(w.Body.String(), `"uncertain":true`) {
				t.Fatal(w.Code, w.Body)
			}
		})
	}
}

func TestSafeManagedLaunchAllTargets(t *testing.T) {
	for _, target := range []string{"codex", "codex-desktop", "claude", "claude-desktop", "deepseek-harness", "hermes"} {
		t.Run(target, func(t *testing.T) {
			s, h, starts := managedLaunchFixture(t)
			for _, variable := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "DSH_HOME", "HERMES_HOME", "CODEX_ELECTRON_USER_DATA_PATH"} {
				t.Setenv(variable, t.TempDir())
			}
			tool := s.d.Local.Tools(context.Background(), nil)[0]
			tool.ID, tool.Kind = target, config.ToolKind(target)
			s.d.Local.FindTools = func(context.Context, map[string]string) []launcher.Tool { return []launcher.Tool{tool} }
			if target == "claude-desktop" {
				root := t.TempDir()
				s.d.ClaudeDesktop = launcher.NewClaudeDesktop3P(s.d.Local)
				s.d.ClaudeDesktop.Platform = func(context.Context, launcher.Tool) (launcher.Claude3PPlatform, error) {
					return launcher.Claude3PPlatform{Root: root, Version: toolcfg.ClaudeDesktop3PVersion}, nil
				}
			}
			worker := &safeChangeWorker{sessionAgent: &sessionAgent{kind: config.ToolFamily(target)}, busy: true}
			s.SetManager(consoleCatalogManager{agent: worker})
			s.nativeConfigurationStop = func(context.Context, launcher.Plan) error { return nil }
			preview := localRequest(s, h, "POST", "/api/local/codex-api/launch/preview", map[string]any{"clientId": "client", "target": target})
			if preview.Code != 200 {
				t.Fatal(preview.Code, preview.Body)
			}
			var value struct {
				Revision string `json:"revision"`
			}
			_ = json.Unmarshal(preview.Body.Bytes(), &value)
			w := localRequest(s, h, "POST", "/api/local/codex-api/launch/safe-v1", map[string]any{"protocol": safeAgentChangeProtocol, "operationId": "33333333-3333-4333-8333-333333333333", "clientId": "client", "target": target, "revision": value.Revision, "catalogOverride": true, "allowModeChange": true, "applyOnly": true, "workspace": t.TempDir()})
			if w.Code != 200 || *starts != 0 || worker.stops != 1 || s.d.Store.Get().ManagedAPICredential(target) == "" {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
		})
	}
}
