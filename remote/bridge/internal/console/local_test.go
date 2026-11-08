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
)

func localRequest(s *Server, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	b := ""
	if body != nil {
		data, _ := json.Marshal(body)
		b = string(data)
	}
	return do(h, method, path, "127.0.0.1:47831", map[string]string{"Cookie": CookieName + "=" + s.Token(), "Content-Type": "application/json", "Origin": "http://127.0.0.1:47831"}, b)
}

func TestLocalSwitchNeedsConfirmationAndRetainsOriginalHome(t *testing.T) {
	s, h := newTestServer(t)
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	exe := filepath.Join(t.TempDir(), "fixture-codex.exe")
	if err := os.WriteFile(exe, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	s.d.Local.FindTools = func(context.Context, map[string]string) []launcher.Tool {
		return []launcher.Tool{{ID: "codex", Kind: "codex", Name: "Codex CLI", Path: exe, Available: true}}
	}
	called := 0
	s.d.Local.Start = func(_ context.Context, p launcher.Plan) (int, error) {
		called++
		if p.ProfileDir != root || !p.Shared {
			t.Fatal("switch used new profile")
		}
		return 101, nil
	}
	id1 := createLocal(t, s, h, "codex", "one", "private-switch-key-one", "https://provider.test")
	id2 := createLocal(t, s, h, "codex", "two", "private-switch-key-two", "https://provider.test")
	w := localRequest(s, h, "POST", "/api/local/switch/preview", map[string]any{"id": id2, "target": "codex"})
	if w.Code != 200 || called != 0 || strings.Contains(w.Body.String(), "private-switch-key") {
		t.Fatal("unsafe switch preview")
	}
	if _, err := os.Stat(filepath.Join(root, "auth.json")); !os.IsNotExist(err) {
		t.Fatal("preview wrote auth")
	}
	w = localRequest(s, h, "POST", "/api/local/switch", map[string]any{"id": id2, "target": "codex"})
	if w.Code != 400 || called != 0 {
		t.Fatal("switch bypassed confirmation")
	}
	w = localRequest(s, h, "POST", "/api/local/switch", map[string]any{"id": id2, "target": "codex", "confirmed": true})
	if w.Code != 200 || called != 1 || s.d.Store.Get().ActiveCodexAccount != id2 || strings.Contains(w.Body.String(), "private-switch-key") {
		t.Fatal("switch not recorded safely")
	}
	before, _ := os.ReadFile(filepath.Join(root, "auth.json"))
	s.d.Local.Start = func(context.Context, launcher.Plan) (int, error) { return 0, errors.New("fixture launch failed") }
	w = localRequest(s, h, "POST", "/api/local/switch", map[string]any{"id": id1, "target": "codex", "confirmed": true})
	if w.Code != 400 || s.d.Store.Get().ActiveCodexAccount != id2 {
		t.Fatal("failed switch changed active account")
	}
	after, _ := os.ReadFile(filepath.Join(root, "auth.json"))
	if string(before) != string(after) {
		t.Fatal("failed API switch changed auth")
	}
}

func createLocal(t *testing.T, s *Server, h http.Handler, kind, name, key, url string) string {
	t.Helper()
	w := localRequest(s, h, "POST", "/api/local/accounts", map[string]any{"name": name, "kind": kind, "baseUrl": url, "key": key, "model": "coding-model"})
	if w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), key) {
		t.Fatal("raw key returned by save")
	}
	var result struct{ Account struct{ ID string } }
	if json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Account.ID == "" {
		t.Fatal("no saved ID")
	}
	return result.Account.ID
}

func TestLocalCRUDWorksWithoutRemoteAndSurvivesRestart(t *testing.T) {
	s, h := newTestServer(t)
	s.d.Local.FindTools = func(context.Context, map[string]string) []launcher.Tool { return []launcher.Tool{} }
	if s.d.Store.Get().LoggedIn() {
		t.Fatal("fixture unexpectedly logged in")
	}
	id1 := createLocal(t, s, h, "codex", "主力", "private-key-one", "https://provider.test/v1")
	id2 := createLocal(t, s, h, "codex", "备用", "private-key-two", "https://another.test")
	id3 := createLocal(t, s, h, "claude", "Claude", "private-key-three", "https://claude.test")
	w := localRequest(s, h, "GET", "/api/local/accounts", nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "private-key-one") || strings.Contains(w.Body.String(), `"key":`) {
		t.Fatal("unmasked list")
	}
	c := s.d.Store.Get()
	if c.ActiveCodexAccount != id1 || c.ActiveClaudeAccount != id3 || c.LoggedIn() {
		t.Fatal("defaults or remote login changed")
	}
	// Blank secret while editing preserves the stored key; another account's key is untouched.
	w = localRequest(s, h, "POST", "/api/local/accounts", map[string]any{"id": id1, "name": "主力新版", "kind": "codex", "baseUrl": "https://provider.test", "model": "new-model", "key": ""})
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	a, _ := s.d.Store.Get().LocalAccount(id1)
	if a.Key != "private-key-one" || a.Model != "new-model" {
		t.Fatal("edit lost credential")
	}
	w = localRequest(s, h, "POST", "/api/local/accounts/select", map[string]string{"id": id2})
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	if s.d.Store.Get().ActiveCodexAccount != id2 {
		t.Fatal("selection not saved")
	}
	w = localRequest(s, h, "POST", "/api/local/accounts/delete", map[string]string{"id": id2})
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	if s.d.Store.Get().ActiveCodexAccount != id1 || s.d.Store.Get().LoggedIn() {
		t.Fatal("delete changed remote/default state")
	}
	// Reopen the store, as a real application restart would.
	reopened, err := config.Open(s.d.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.Get().LocalAccounts) != 2 || reopened.Get().ActiveCodexAccount != id1 {
		t.Fatal("persisted state differs")
	}
	if w := do(h, "POST", "/api/local/accounts", "127.0.0.1:47831", map[string]string{"Content-Type": "application/json"}, `{}`); w.Code != 401 {
		t.Fatal("local endpoint bypassed auth")
	}
	for _, path := range []string{"/api/local/accounts/delete", "/api/local/accounts/select"} {
		if w := localRequest(s, h, "POST", path, map[string]string{"id": "../escape"}); w.Code != 400 {
			t.Fatal("unsafe ID accepted")
		}
	}
}

func TestLocalAccountRemoteBindingDefaultsToCurrentStationAndCanBeDisabled(t *testing.T) {
	s, h := newTestServer(t)
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.ConnectRemote("https://one.example", "https://one.example/salcara-hub")
		c.ConnectRemote("https://two.example", "https://two.example/salcara-hub")
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	id := createLocal(t, s, h, "api", "current-only", "fixture-key-one", "https://provider.test")
	account, _ := s.d.Store.Get().LocalAccount(id)
	if !account.RemoteHubBindingsSet || !reflect.DeepEqual(account.RemoteHubURLs, []string{"https://two.example/salcara-hub"}) {
		t.Fatalf("new API was not scoped to the current saved station: %+v", account)
	}

	w := localRequest(s, h, "POST", "/api/local/accounts", map[string]any{
		"id": id, "name": "current-only", "kind": "api", "baseUrl": "https://provider.test", "key": "",
		"remoteHubBindingsSet": true, "remoteHubUrls": []string{},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("disable remote binding: %d %s", w.Code, w.Body)
	}
	account, _ = s.d.Store.Get().LocalAccount(id)
	if !account.RemoteHubBindingsSet || len(account.RemoteHubURLs) != 0 || s.d.Store.Get().RemoteHubAllowsAccount("https://two.example/salcara-hub", account) {
		t.Fatal("explicitly disabled API remained available to a Hub")
	}

	w = localRequest(s, h, "POST", "/api/local/accounts", map[string]any{
		"id": id, "name": "current-only", "kind": "api", "baseUrl": "https://provider.test", "key": "",
		"remoteHubBindingsSet": true, "remoteHubUrls": []string{"https://unregistered.example/salcara-hub"},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("accepted an unregistered station binding: %d %s", w.Code, w.Body)
	}
	account, _ = s.d.Store.Get().LocalAccount(id)
	if len(account.RemoteHubURLs) != 0 {
		t.Fatal("rejected binding partially changed the saved API")
	}
}

func TestLocalEditPreservesPoolAndProviderMetadataFromOlderUI(t *testing.T) {
	s, h := newTestServer(t)
	id := createLocal(t, s, h, "api", "反代 GPT", "private-pool-key", "https://proxy.test/v1")
	if err := s.d.Store.Update(func(c *config.Config) error {
		for i := range c.LocalAccounts {
			if c.LocalAccounts[i].ID == id {
				c.LocalAccounts[i].SourceType = "reverse_proxy"
				c.LocalAccounts[i].ProviderID = "provider-gpt"
				c.LocalAccounts[i].PoolEnabled = true
				c.LocalAccounts[i].PoolGroup = "gpt-pool"
				c.LocalAccounts[i].PoolStrategy = "custom"
				c.LocalAccounts[i].PoolBackup = true
				c.LocalAccounts[i].Priority = 7
				c.LocalAccounts[i].ModelAllowlist = []string{"gpt-5"}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// This body matches the older UI and omits all newer pool/card fields.
	w := localRequest(s, h, "POST", "/api/local/accounts", map[string]any{"id": id, "name": "反代 GPT 改名", "kind": "api", "baseUrl": "https://proxy.test", "key": "", "model": "gpt-5"})
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	a, _ := s.d.Store.Get().LocalAccount(id)
	if a.SourceType != "reverse_proxy" || a.ProviderID != "provider-gpt" || !a.PoolEnabled || a.PoolGroup != "gpt-pool" || a.PoolStrategy != "custom" || !a.PoolBackup || a.Priority != 7 || len(a.ModelAllowlist) != 1 {
		t.Fatalf("metadata was cleared by legacy edit: %+v", a)
	}
}

func TestLegacyDefaultApplyCannotBypassGatewayLease(t *testing.T) {
	s, h := newTestServer(t)
	id := createLocal(t, s, h, "codex", "受保护反代", "upstream-secret", "https://proxy.test/v1")
	if err := s.d.Store.Update(func(c *config.Config) error {
		for i := range c.LocalAccounts {
			if c.LocalAccounts[i].ID == id {
				c.LocalAccounts[i].SourceType = "reverse_proxy"
				c.LocalAccounts[i].Model = "gpt-5"
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w := localRequest(s, h, "POST", "/api/local/default", map[string]any{"id": id, "action": "apply"})
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "不会把反代或官方账号凭据直接写入原工具") {
		t.Fatalf("legacy default route bypassed gateway protection: %d %s", w.Code, w.Body)
	}
}

func TestLocalLaunchUsesSelectedKeyAndProfile(t *testing.T) {
	s, h := newTestServer(t)
	exe := filepath.Join(t.TempDir(), "codex.exe")
	if err := os.WriteFile(exe, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	s.d.Local.FindTools = func(context.Context, map[string]string) []launcher.Tool {
		return []launcher.Tool{{ID: "codex", Kind: "codex", Name: "Codex CLI", Available: true, Path: exe}}
	}
	called := 0
	var homes []string
	var keys []string
	s.d.Local.Start = func(ctx context.Context, p launcher.Plan) (int, error) {
		called++
		homes = append(homes, p.ProfileDir)
		for _, kv := range p.Environment {
			if strings.HasPrefix(kv, "OPENAI_API_KEY=") {
				keys = append(keys, strings.TrimPrefix(kv, "OPENAI_API_KEY="))
			}
		}
		return 12345, nil
	}
	id1 := createLocal(t, s, h, "codex", "one", "key-one-private", "https://provider.test")
	id2 := createLocal(t, s, h, "codex", "two", "key-two-private", "https://provider.test")
	id3 := createLocal(t, s, h, "claude", "claude", "claude-private-key", "https://claude.test")
	for _, id := range []string{id1, id2} {
		w := localRequest(s, h, "POST", "/api/local/launch", map[string]string{"id": id, "target": "codex", "workspace": t.TempDir()})
		if w.Code != 200 || strings.Contains(w.Body.String(), "key-one-private") {
			t.Fatalf("launch %d %s", w.Code, w.Body)
		}
	}
	if called != 2 || homes[0] == homes[1] || strings.Join(keys, ",") != "key-one-private,key-two-private" {
		t.Fatal("launch profiles or keys were mixed")
	}
	a, _ := s.d.Store.Get().LocalAccount(id2)
	if a.LastUsedAt == 0 || a.Target != "codex" {
		t.Fatal("last launch not saved")
	}
	w := localRequest(s, h, "POST", "/api/local/launch", map[string]string{"id": id3, "target": "codex"})
	if w.Code != 200 || called != 3 || keys[2] != "claude-private-key" {
		t.Fatal("a legacy label incorrectly restricted the key to one tool")
	}
	w = localRequest(s, h, "POST", "/api/local/launch", map[string]string{"id": id1, "target": "shell-command"})
	if w.Code != 400 {
		t.Fatal("arbitrary target accepted")
	}
	if s.d.Store.Get().LoggedIn() {
		t.Fatal("launch enabled remote login")
	}
}

func TestIndependentLaunchCannotBypassGatewayLease(t *testing.T) {
	for _, source := range []string{"reverse_proxy", "oauth", "official_account"} {
		t.Run(source, func(t *testing.T) {
			s, h := newTestServer(t)
			id := createLocal(t, s, h, "codex", "protected", "synthetic-upstream-secret", "https://proxy.test/v1")
			if err := s.d.Store.Update(func(c *config.Config) error {
				for i := range c.LocalAccounts {
					if c.LocalAccounts[i].ID == id {
						c.LocalAccounts[i].SourceType = source
						c.LocalAccounts[i].Model = "gpt-fixture"
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			s.d.Local.Start = func(context.Context, launcher.Plan) (int, error) {
				t.Fatal("protected source was launched with raw credentials")
				return 0, nil
			}
			w := localRequest(s, h, "POST", "/api/local/launch", map[string]string{"id": id, "target": "codex"})
			if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "独立实例不会直接写入") || strings.Contains(w.Body.String(), "synthetic-upstream-secret") {
				t.Fatalf("independent launch bypassed runtime lease: %d %s", w.Code, w.Body)
			}
			if rows, err := os.ReadDir(filepath.Join(s.d.Local.Dir, "profiles")); err == nil && len(rows) != 0 {
				t.Fatal("rejected launch wrote a profile")
			}
		})
	}
}

func TestLocalModelsDoesNotReplaceSavedModelAndHandlesFailure(t *testing.T) {
	s, h := newTestServer(t)
	fail := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gateway/v1/models" || r.Header.Get("Authorization") != "Bearer model-private-key" {
			t.Error("wrong request")
		}
		if fail {
			w.WriteHeader(401)
			_, _ = w.Write([]byte("model-private-key"))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"coding-model"},{"id":"other-model"}]}`))
	}))
	defer upstream.Close()
	id := createLocal(t, s, h, "codex", "upstream", "model-private-key", upstream.URL+"/gateway/v1")
	w := localRequest(s, h, "POST", "/api/local/models", map[string]string{"id": id})
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	a, _ := s.d.Store.Get().LocalAccount(id)
	if a.Model != "coding-model" || len(a.Models) != 2 {
		t.Fatal("model choice lost")
	}
	fail = true
	w = localRequest(s, h, "POST", "/api/local/models", map[string]string{"id": id})
	if w.Code != 502 || strings.Contains(w.Body.String(), "model-private-key") {
		t.Fatal("unsafe failure response")
	}
	a, _ = s.d.Store.Get().LocalAccount(id)
	if a.Model != "coding-model" || len(a.Models) != 2 {
		t.Fatal("failed probe changed account")
	}
}

func TestLocalModelsProbeAppliesSavedModelPolicy(t *testing.T) {
	s, h := newTestServer(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"allowed-model"},{"id":"blocked-model"},{"id":"unlisted-model"}]}`))
	}))
	defer upstream.Close()
	id := createLocal(t, s, h, "codex", "policy", "model-private-key", upstream.URL+"/gateway/v1")
	if err := s.d.Store.Update(func(c *config.Config) error {
		for i := range c.LocalAccounts {
			if c.LocalAccounts[i].ID == id {
				c.LocalAccounts[i].ModelAllowlist = []string{"allowed-model", "blocked-model"}
				c.LocalAccounts[i].ModelDenylist = []string{"blocked-model"}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w := localRequest(s, h, "POST", "/api/local/models", map[string]string{"id": id})
	if w.Code != 200 {
		t.Fatalf("model probe failed: %d %s", w.Code, w.Body)
	}
	a, _ := s.d.Store.Get().LocalAccount(id)
	if !reflect.DeepEqual(a.Models, []string{"allowed-model"}) {
		t.Fatalf("saved model policy was not applied: %v", a.Models)
	}
}

func TestLocalAccountModelDirectoryCanBeEdited(t *testing.T) {
	s, h := newTestServer(t)
	id := createLocal(t, s, h, "api", "editable", "model-private-key", "https://provider.test/v1")
	w := localRequest(s, h, "POST", "/api/local/accounts", map[string]any{
		"id": id, "name": "editable", "kind": "api", "baseUrl": "https://provider.test/v1",
		"model": "gpt-fixture", "models": []string{"gpt-fixture", "claude-fixture", "gpt-fixture"},
	})
	if w.Code != 200 {
		t.Fatalf("manual model edit failed: %d %s", w.Code, w.Body)
	}
	a, ok := s.d.Store.Get().LocalAccount(id)
	if !ok || !reflect.DeepEqual(a.Models, []string{"gpt-fixture", "claude-fixture"}) {
		t.Fatalf("manual model directory was not persisted: %#v", a.Models)
	}
}

func TestCustomToolPathValidation(t *testing.T) {
	s, h := newTestServer(t)
	for _, paths := range []map[string]string{{"unknown": "x"}, {"codex": "codex --flag"}, {"codex": t.TempDir()}} {
		w := localRequest(s, h, "POST", "/api/local/tools", map[string]any{"paths": paths})
		if w.Code != 400 {
			t.Fatal("invalid tool path accepted")
		}
	}
	exe := filepath.Join(t.TempDir(), "my tool.exe")
	if err := os.WriteFile(exe, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	w := localRequest(s, h, "POST", "/api/local/tools", map[string]any{"paths": map[string]string{"codex": exe}})
	if w.Code != 200 || s.d.Store.Get().LocalToolPaths["codex"] != exe {
		t.Fatal("custom path not saved")
	}
	w = localRequest(s, h, "POST", "/api/local/tools", map[string]any{"paths": map[string]string{"codex": ""}})
	if w.Code != 200 || len(s.d.Store.Get().LocalToolPaths) != 0 {
		t.Fatal("automatic path not restored")
	}
}
