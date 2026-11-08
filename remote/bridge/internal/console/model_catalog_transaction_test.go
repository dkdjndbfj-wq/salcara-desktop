package console

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/launcher"
)

func TestModelProbeRejectsLateResponseAfterNewerProbeOrManualEdit(t *testing.T) {
	for _, change := range []string{"newer-probe", "manual-directory", "key-change"} {
		t.Run(change, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					close(entered)
					<-release
					_, _ = w.Write([]byte(`{"data":[{"id":"old-model"}]}`))
				} else {
					_, _ = w.Write([]byte(`{"data":[{"id":"new-model"}]}`))
				}
			}))
			defer upstream.Close()
			s, h := newTestServer(t)
			id := createLocal(t, s, h, "api", "catalog", "synthetic-key", upstream.URL)
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- localRequest(s, h, "POST", "/api/local/models", map[string]string{"id": id}) }()
			<-entered
			var changed *httptest.ResponseRecorder
			if change == "newer-probe" {
				changed = localRequest(s, h, "POST", "/api/local/models", map[string]string{"id": id})
			} else {
				body := map[string]any{"id": id, "name": "catalog", "kind": "api", "baseUrl": upstream.URL, "models": []string{"manual-model"}}
				if change == "key-change" {
					body["key"] = "synthetic-new-key"
				}
				changed = localRequest(s, h, "POST", "/api/local/accounts", body)
			}
			close(release)
			late := <-done
			if changed.Code != 200 || late.Code != http.StatusConflict {
				t.Fatalf("change=%d late=%d", changed.Code, late.Code)
			}
			a, _ := s.d.Store.Get().LocalAccount(id)
			if containsLocalModel(a.Models, "old-model") {
				t.Fatal("old response overwrote current catalog")
			}
		})
	}
}

func TestModelDirectoryClearRetiresAllPendingModelSelections(t *testing.T) {
	for _, change := range []string{"empty", "null", "key-change"} {
		t.Run(change, func(t *testing.T) {
			s, h := newTestServer(t)
			id := createLocal(t, s, h, "api", "catalog", "synthetic-key", "https://provider.test")
			if err := s.d.Store.Update(func(c *config.Config) error {
				c.LocalAccounts[0].Models = []string{"coding-model"}
				c.ToolAPISelections = map[string]string{"codex": id, "claude": id}
				c.ToolModels = map[string]string{"codex": "coding-model"}
				c.ToolProtocols = map[string]string{"codex": "responses", "claude": "chat"}
				c.RemoteAPI = map[string]config.RemoteAPI{"codex": {AccountID: id, Model: "coding-model"}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			body := map[string]any{"id": id, "kind": "api", "name": "catalog", "baseUrl": "https://provider.test", "model": "coding-model", "models": []string{}}
			if change == "null" {
				body["models"] = nil
			}
			if change == "key-change" {
				delete(body, "models")
				body["key"] = "new-synthetic-key"
			}
			w := localRequest(s, h, "POST", "/api/local/accounts", body)
			if w.Code != 200 {
				t.Fatalf("save %d", w.Code)
			}
			c := s.d.Store.Get()
			a, _ := c.LocalAccount(id)
			if a.Model != "" || len(a.Models) != 0 || len(c.ToolModels) != 0 || len(c.ToolProtocols) != 0 || c.RemoteAPI["codex"].Model != "" {
				t.Fatal("retired model/protocol survived the directory change")
			}
		})
	}
}

func TestLaunchCatalogRefreshAndFallback(t *testing.T) {
	s, h := newTestServer(t)
	id := createLocal(t, s, h, "api", "catalog", "synthetic-key", "https://provider.test")
	a, _ := s.d.Store.Get().LocalAccount(id)
	s.launchModelProbe = func(context.Context, config.LocalAccount) ([]string, error) {
		return []string{"deepseek-chat", "grok-fixture"}, nil
	}
	updated, err := s.refreshLaunchCatalog(context.Background(), a)
	if err != nil || !reflect.DeepEqual(updated.Models, []string{"deepseek-chat", "grok-fixture"}) || updated.Model != "" {
		t.Fatalf("catalog refresh: %v", err)
	}
	effective := launchCatalogDefault(s.d.Store.Get().APIForTool(updated, "codex"))
	if effective.Model != "deepseek-chat" || effective.Protocol != "chat" || !config.NeedsGateway(effective) {
		t.Fatal("old selection was not replaced with a compatible route")
	}
	s.launchModelProbe = func(context.Context, config.LocalAccount) ([]string, error) {
		return nil, errors.New("synthetic unsupported endpoint")
	}
	retained, err := s.refreshLaunchCatalog(context.Background(), updated)
	if err != nil || !reflect.DeepEqual(retained.Models, updated.Models) {
		t.Fatal("discovery failure discarded saved manual catalog")
	}
}

func TestDirectLaunchRejectsAccountChangedDuringDiscoveryAndRollsBack(t *testing.T) {
	s, h := newTestServer(t)
	id := createLocal(t, s, h, "codex", "catalog", "synthetic-key", "https://provider.test")
	exe := filepath.Join(t.TempDir(), "codex.exe")
	if err := os.WriteFile(exe, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	s.d.Local.FindTools = func(context.Context, map[string]string) []launcher.Tool {
		if err := s.d.Store.Update(func(c *config.Config) error { c.LocalAccounts[0].Key = "changed-key"; return nil }); err != nil {
			t.Fatal(err)
		}
		return []launcher.Tool{{ID: "codex", Kind: "codex", Path: exe, Available: true}}
	}
	started := false
	s.d.Local.Start = func(context.Context, launcher.Plan) (int, error) { started = true; return 1, nil }
	w := localRequest(s, h, "POST", "/api/local/launch", map[string]string{"id": id, "target": "codex", "workspace": t.TempDir()})
	if w.Code != 400 || started {
		t.Fatalf("stale launch status=%d started=%v", w.Code, started)
	}
	if _, err := os.Stat(filepath.Join(s.d.Local.Dir, "instances", id, "codex", "auth.json")); !os.IsNotExist(err) {
		t.Fatal("stale launch left credential file")
	}
}

func TestSuccessfulEmptyUpstreamCatalogRetiresSavedModels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"data":[]}`)) }))
	defer upstream.Close()
	for _, mode := range []string{"explicit", "launch"} {
		t.Run(mode, func(t *testing.T) {
			s, h := newTestServer(t)
			id := createLocal(t, s, h, "api", "catalog", "synthetic-key", upstream.URL)
			if err := s.d.Store.Update(func(c *config.Config) error { c.LocalAccounts[0].Models = []string{"old-model"}; return nil }); err != nil {
				t.Fatal(err)
			}
			if mode == "launch" {
				s.launchModelProbe = nil
				a, _ := s.d.Store.Get().LocalAccount(id)
				if _, err := s.refreshLaunchCatalog(context.Background(), a); err == nil {
					t.Fatal("empty group accepted at launch")
				}
			} else if w := localRequest(s, h, "POST", "/api/local/models", map[string]string{"id": id}); w.Code != 502 {
				t.Fatalf("empty explicit refresh %d", w.Code)
			}
			a, _ := s.d.Store.Get().LocalAccount(id)
			if len(a.Models) != 0 || a.Model != "" {
				t.Fatal("empty upstream group left stale models")
			}
		})
	}
}

func TestManualDirectorySurvivesAutomaticRefreshUntilExplicitRefresh(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"provider-model"}]}`))
	}))
	defer upstream.Close()
	s, h := newTestServer(t)
	id := createLocal(t, s, h, "api", "catalog", "synthetic-key", upstream.URL)
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.LocalAccounts[0].Models = []string{"manual-route"}
		c.LocalAccounts[0].ModelsManual = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.launchModelProbe = nil
	a, _ := s.d.Store.Get().LocalAccount(id)
	got, err := s.refreshLaunchCatalog(context.Background(), a)
	if err != nil || !got.ModelsManual || !reflect.DeepEqual(got.Models, []string{"manual-route"}) {
		t.Fatalf("manual directory lost: %v", err)
	}
	w := localRequest(s, h, "POST", "/api/local/models", map[string]string{"id": id})
	got, _ = s.d.Store.Get().LocalAccount(id)
	if w.Code != 200 || got.ModelsManual || !reflect.DeepEqual(got.Models, []string{"provider-model"}) {
		t.Fatal("explicit refresh did not replace manual directory")
	}
}

func TestUnknownModelBootstrapKeepsGatewayCredentialStable(t *testing.T) {
	a := config.LocalAccount{ID: "fixture", Kind: "api", Models: []string{"provider/custom-model"}}
	c := config.Config{LocalAccounts: []config.LocalAccount{a}, GatewayRuntimeKey: "synthetic-process-key"}
	effective := launchCatalogDefault(c.APIForTool(a, "codex"))
	effective.GatewayLaunchLease = "synthetic-launch-lease"
	connection := c.ToolConnection(effective, "codex", 47831)
	c.RecordAppliedAPI("codex", effective, "openai")
	if effective.Protocol != "responses" || connection.Key != c.LocalGatewayCredential("codex") {
		t.Fatal("unknown model bootstrap changed the configured gateway credential")
	}
}

func TestSwitchDoesNotOverwriteSelectionChangedDuringStop(t *testing.T) {
	s, h := newTestServer(t)
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	exe := filepath.Join(t.TempDir(), "codex.exe")
	if err := os.WriteFile(exe, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	a := createLocal(t, s, h, "codex", "one", "fixture-a", "https://provider.test")
	b := createLocal(t, s, h, "codex", "two", "fixture-b", "https://provider.test")
	s.d.Local.FindTools = func(context.Context, map[string]string) []launcher.Tool {
		return []launcher.Tool{{ID: "codex-desktop", Kind: "codex", Path: exe, Available: true}}
	}
	s.d.Local.Stop = func(context.Context, launcher.Plan) error {
		return s.d.Store.Update(func(c *config.Config) error { c.ToolAPISelections = map[string]string{"codex-desktop": b}; return nil })
	}
	started := false
	s.d.Local.Start = func(context.Context, launcher.Plan) (int, error) { started = true; return 1, nil }
	w := localRequest(s, h, "POST", "/api/local/switch", map[string]any{"id": a, "target": "codex-desktop", "confirmed": true, "workspace": t.TempDir()})
	if w.Code != 400 || started || s.d.Store.Get().ToolAPISelections["codex-desktop"] != b {
		t.Fatal("older apply swallowed a newer selection")
	}
}

func TestLaunchEntriesRejectSelectionChangedDuringCatalogRequest(t *testing.T) {
	for _, entry := range []struct{ route, target string }{
		{"/api/local/switch", "codex"}, {"/api/local/launch", "codex"}, {"/api/local/claude-desktop/switch", "claude-desktop"},
	} {
		t.Run(entry.route, func(t *testing.T) {
			s, h := newTestServer(t)
			a := createLocal(t, s, h, "api", "one", "fixture-a", "https://provider.test")
			b := createLocal(t, s, h, "api", "two", "fixture-b", "https://provider.test")
			if err := s.d.Store.Update(func(c *config.Config) error { c.ToolAPISelections = map[string]string{entry.target: a}; return nil }); err != nil {
				t.Fatal(err)
			}
			s.launchModelProbe = func(context.Context, config.LocalAccount) ([]string, error) {
				if err := s.d.Store.Update(func(c *config.Config) error { c.ToolAPISelections[entry.target] = b; return nil }); err != nil {
					t.Fatal(err)
				}
				return []string{"gpt-fixture"}, nil
			}
			w := localRequest(s, h, "POST", entry.route, map[string]any{"id": a, "target": entry.target, "confirmed": true})
			if w.Code != http.StatusConflict || s.d.Store.Get().ToolAPISelections[entry.target] != b {
				t.Fatalf("old launch adopted newer selection: %d", w.Code)
			}
		})
	}
}
