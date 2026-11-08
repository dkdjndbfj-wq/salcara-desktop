package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/launcher"
	"salcara/bridge/internal/protocol"
)

func managedLaunchFixture(t *testing.T) (*Server, http.Handler, *int) {
	t.Helper()
	s, h := newTestServer(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	exe := filepath.Join(t.TempDir(), "synthetic-codex.exe")
	if err := os.WriteFile(exe, []byte("not executable"), 0600); err != nil {
		t.Fatal(err)
	}
	s.d.Local.FindTools = func(context.Context, map[string]string) []launcher.Tool {
		return []launcher.Tool{{ID: "codex", Kind: "codex", Available: true, Path: exe}}
	}
	starts := 0
	s.d.Local.Start = func(context.Context, launcher.Plan) (int, error) { starts++; return 123, nil }
	s.d.Local.Stop = func(context.Context, launcher.Plan) error { return nil }
	s.launchModelProbe = func(_ context.Context, a config.LocalAccount) ([]string, error) {
		return append([]string(nil), a.Models...), nil
	}
	s.d.Store.SetRuntimeGatewayKey("synthetic-managed-runtime")
	if err := s.d.Store.Update(func(c *config.Config) error {
		a := summaryAPI("upstream")
		a.Models = []string{"gpt-fixture"}
		c.LocalAccounts = []config.LocalAccount{a}
		c.CodexAPI.Enabled = true
		c.CodexAPI.RuntimeLease = c.GatewayRuntimeKey
		c.CodexAPI.Clients = []config.NamedAPIClient{summaryClient("client", "upstream")}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return s, h, &starts
}

func managedPreview(t *testing.T, s *Server, h http.Handler) string {
	t.Helper()
	w := localRequest(s, h, "POST", "/api/local/codex-api/launch/preview", map[string]any{"clientId": "client", "target": "codex"})
	if w.Code != 200 {
		t.Fatalf("preview %d: %s", w.Code, w.Body)
	}
	var body struct {
		Revision string `json:"revision"`
	}
	if json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Revision == "" {
		t.Fatal("no preview")
	}
	if strings.Contains(w.Body.String(), "synthetic-summary-secret") || strings.Contains(w.Body.String(), "ml-salcara-") {
		t.Fatal("preview secret exposed")
	}
	return body.Revision
}

func TestManagedLaunchWritesScopedCredentialWithoutRotatingManualClient(t *testing.T) {
	s, h, starts := managedLaunchFixture(t)
	before := s.d.Store.Get().CodexAPI.Clients
	revision := managedPreview(t, s, h)
	if *starts != 0 {
		t.Fatal("preview launched")
	}
	body := map[string]any{"clientId": "client", "target": "codex", "revision": revision, "confirmed": true, "catalogOverride": true, "workspace": t.TempDir()}
	w := localRequest(s, h, "POST", "/api/local/codex-api/launch", body)
	if w.Code != 200 || *starts != 1 {
		t.Fatalf("launch %d: %s", w.Code, w.Body)
	}
	c := s.d.Store.Get()
	key := c.ManagedAPICredential("codex")
	if key == "" || !reflect.DeepEqual(before, c.CodexAPI.Clients) {
		t.Fatal("manual client replaced or managed credential missing")
	}
	if _, ok := c.AuthorizedAppliedToolAccount("codex"); ok {
		t.Fatal("pooled local service masquerades as remote account")
	}
	if strings.Contains(w.Body.String(), key) {
		t.Fatal("launch returned secret")
	}
	if next := localRequest(s, h, "POST", "/api/local/codex-api/launch", body); next.Code != 409 || *starts != 1 {
		t.Fatal("duplicate confirmation relaunched")
	}
	if err := s.d.Store.Update(func(c *config.Config) error { c.CodexAPI.Clients[0].Disabled = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if s.d.Store.Get().ManagedAPICredential("codex") != "" {
		t.Fatal("disabled client retained native authorization")
	}
}

func TestManagedLaunchRejectsChangedSourceAndFailedStartRetiresAuthorization(t *testing.T) {
	for _, mode := range []string{"changed-source", "failed-start"} {
		t.Run(mode, func(t *testing.T) {
			s, h, starts := managedLaunchFixture(t)
			revision := managedPreview(t, s, h)
			if mode == "changed-source" {
				if err := s.d.Store.Update(func(c *config.Config) error { c.LocalAccounts[0].Key = "synthetic-replaced-key"; return nil }); err != nil {
					t.Fatal(err)
				}
			} else {
				s.d.Local.Start = func(context.Context, launcher.Plan) (int, error) {
					*starts++
					return 0, errors.New("synthetic start failure")
				}
			}
			w := localRequest(s, h, "POST", "/api/local/codex-api/launch", map[string]any{"clientId": "client", "target": "codex", "revision": revision, "confirmed": true, "workspace": t.TempDir()})
			if w.Code != 409 || s.d.Store.Get().ManagedAPICredential("codex") != "" {
				t.Fatal("invalid launch remained authorized")
			}
			if mode == "changed-source" && *starts != 0 {
				t.Fatal("changed source launched")
			}
		})
	}
}

func TestManagedLaunchRejectsRecursiveLocalUpstream(t *testing.T) {
	s, h, starts := managedLaunchFixture(t)
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.LocalAccounts[0].BaseURL = "http://127.0.0.1:47831/codex-api/v1"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w := localRequest(s, h, "POST", "/api/local/codex-api/launch/preview", map[string]any{"clientId": "client", "target": "codex"})
	if w.Code != 409 || *starts != 0 {
		t.Fatal("recursive upstream accepted")
	}
}

func TestManagedLaunchRefreshesChangedModelGroupBeforePreview(t *testing.T) {
	s, h, starts := managedLaunchFixture(t)
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.LocalAccounts[0].Model = "gpt-fixture"
		c.ToolAPISelections = map[string]string{"codex": "upstream"}
		c.ToolModels = map[string]string{"codex": "gpt-fixture"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := s.d.Store.Get().CodexAPI.Clients[0].AuthEpoch
	calls := 0
	s.launchModelProbe = func(context.Context, config.LocalAccount) ([]string, error) {
		calls++
		return []string{"deepseek-fixture"}, nil
	}
	revision := managedPreview(t, s, h)
	preview := s.managedLaunchPreviews[revision]
	if calls != 1 || *starts != 0 || !reflect.DeepEqual(preview.Models, []string{"deepseek-fixture"}) {
		t.Fatal("preview reused a retired model group")
	}
	c := s.d.Store.Get()
	if c.LocalAccounts[0].Model != "" || c.ToolModels["codex"] != "" || c.CodexAPI.Clients[0].AuthEpoch == before {
		t.Fatal("retired model or authorization remained")
	}
	w := localRequest(s, h, "POST", "/api/local/codex-api/launch", map[string]any{"clientId": "client", "target": "codex", "revision": revision, "confirmed": true, "workspace": t.TempDir()})
	if w.Code != 200 || *starts != 1 || calls != 1 {
		t.Fatalf("refreshed preview could not launch: %d %s", w.Code, w.Body)
	}
}

func TestManagedLaunchCatalogPreservesManualAndUnavailableDirectories(t *testing.T) {
	for _, mode := range []string{"manual", "unsupported", "unauthorized", "empty"} {
		t.Run(mode, func(t *testing.T) {
			s, h, starts := managedLaunchFixture(t)
			if err := s.d.Store.Update(func(c *config.Config) error { c.LocalAccounts[0].ModelsManual = mode == "manual"; return nil }); err != nil {
				t.Fatal(err)
			}
			s.launchModelProbe = func(context.Context, config.LocalAccount) ([]string, error) {
				switch mode {
				case "manual":
					return []string{"new-provider-model"}, nil
				case "unsupported":
					return nil, &launcher.ModelCatalogHTTPError{StatusCode: 404}
				case "unauthorized":
					return nil, &launcher.ModelCatalogHTTPError{StatusCode: 401}
				default:
					return nil, launcher.ErrEmptyModelCatalog
				}
			}
			w := localRequest(s, h, "POST", "/api/local/codex-api/launch/preview", map[string]string{"clientId": "client", "target": "codex"})
			want := 200
			if mode == "unauthorized" || mode == "empty" {
				want = 409
			}
			if w.Code != want || *starts != 0 {
				t.Fatalf("preview status %d, want %d", w.Code, want)
			}
			if want == 200 && !strings.Contains(w.Body.String(), "gpt-fixture") {
				t.Fatal("manual/unsupported catalog was discarded")
			}
			if mode == "empty" && len(s.d.Store.Get().LocalAccounts[0].Models) != 0 {
				t.Fatal("empty group retained stale catalog")
			}
		})
	}
}

func TestManagedLaunchRefreshChecksClientChangeBeforeAndAfterCommit(t *testing.T) {
	for _, timing := range []string{"during-probe", "after-commit"} {
		t.Run(timing, func(t *testing.T) {
			s, h, starts := managedLaunchFixture(t)
			rotate := func() {
				if err := s.d.Store.Update(func(c *config.Config) error { c.CodexAPI.Clients[0].Disabled = true; return nil }); err != nil {
					t.Fatal(err)
				}
				if err := s.d.Store.Update(func(c *config.Config) error { c.CodexAPI.Clients[0].Disabled = false; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			if timing == "after-commit" {
				done := false
				s.d.Store.OnChange(func(_, now config.Config) {
					if !done && len(now.LocalAccounts[0].Models) == 1 && now.LocalAccounts[0].Models[0] == "fresh-model" {
						done = true
						rotate()
					}
				})
			}
			s.launchModelProbe = func(context.Context, config.LocalAccount) ([]string, error) {
				if timing == "during-probe" {
					rotate()
				}
				return []string{"fresh-model"}, nil
			}
			w := localRequest(s, h, "POST", "/api/local/codex-api/launch/preview", map[string]string{"clientId": "client", "target": "codex"})
			if w.Code != 409 || *starts != 0 || len(s.managedLaunchPreviews) != 0 {
				t.Fatal("preview adopted another authorization generation")
			}
			if timing == "during-probe" && s.d.Store.Get().LocalAccounts[0].Models[0] != "gpt-fixture" {
				t.Fatal("stale probe committed after client change")
			}
		})
	}
}

func TestManagedLaunchRefreshesOnlyTheSelectedPoolMember(t *testing.T) {
	s, h, _ := managedLaunchFixture(t)
	if err := s.d.Store.Update(func(c *config.Config) error {
		other := summaryAPI("other")
		other.Models = []string{"unused-model"}
		c.LocalAccounts = append(c.LocalAccounts, other)
		c.CodexAPI.Clients[0].AccountIDs = []string{"upstream", "other"}
		c.CodexAPI.Clients[0].PoolStrategy, c.CodexAPI.Clients[0].PoolAccountID = config.PoolStrategySingleAccount, "upstream"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.launchModelProbe = func(_ context.Context, a config.LocalAccount) ([]string, error) {
		calls++
		if a.ID != "upstream" {
			t.Fatal("queried an inactive member")
		}
		return []string{"fresh-model"}, nil
	}
	revision := managedPreview(t, s, h)
	if calls != 1 || !reflect.DeepEqual(s.managedLaunchPreviews[revision].Models, []string{"fresh-model"}) {
		t.Fatal("wrong effective member catalog")
	}
}

func TestManagedLaunchAlwaysRefreshesOfficialPickerHints(t *testing.T) {
	s, h := officialConsole(t)
	ref := importOfficialConsole(t, s, h)
	s.d.OfficialGateway = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
		t.Fatal("catalog read invoked inference")
		return nil, nil
	}
	s.d.Store.SetRuntimeGatewayKey("synthetic-catalog-runtime")
	if err := s.d.Store.Update(func(c *config.Config) error {
		for i, a := range c.LocalAccounts {
			if a.CredentialRef == ref {
				c.LocalAccounts[i].Models = []string{"old-hint"}
				c.CodexAPI.Clients = []config.NamedAPIClient{summaryClient("client", a.ID)}
			}
		}
		c.CodexAPI.Enabled, c.CodexAPI.RuntimeLease = true, c.GatewayRuntimeKey
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.apiService.ModelCatalog = func(ctx context.Context, a config.LocalAccount, refresh bool) (protocol.ModelCatalog, error) {
		calls++
		if !refresh {
			t.Fatal("official picker used stale saved hints")
		}
		if err := s.d.Store.Update(func(c *config.Config) error {
			for i := range c.LocalAccounts {
				if c.LocalAccounts[i].ID == a.ID {
					c.LocalAccounts[i].Models = []string{"fresh-hint"}
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return protocol.ModelCatalog{Models: []string{"fresh-hint"}}, nil
	}
	c := s.d.Store.Get()
	_, models, err := s.managedLaunchModels(context.Background(), c, c.CodexAPI.Clients[0], "codex")
	if err != nil || calls != 1 || !reflect.DeepEqual(models, []string{"fresh-hint"}) {
		t.Fatalf("official catalog refresh failed: %v", err)
	}
}
