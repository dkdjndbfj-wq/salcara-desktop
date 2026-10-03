package console

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/launcher"
	"salcara/bridge/internal/toolcfg"
)

func consoleClaude3PFixture(t *testing.T) (*Server, http.Handler, map[string]string, string, *int) {
	t.Helper()
	store, err := config.Open(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	account := config.LocalAccount{ID: "shared-api", Name: "Shared", Kind: "api", BaseURL: "https://fixture.test", Key: "fixture-only-secret", AuthMode: "bearer"}
	if err := store.Update(func(c *config.Config) error { c.LocalAccounts = []config.LocalAccount{account}; return nil }); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	local := launcher.New(t.TempDir())
	exe := filepath.Join(t.TempDir(), "fake-claude.exe")
	if err := os.WriteFile(exe, []byte("fixture-no-executable"), 0o600); err != nil {
		t.Fatal(err)
	}
	local.FindTools = func(context.Context, map[string]string) []launcher.Tool {
		return []launcher.Tool{{ID: "claude-desktop", Name: "Claude Desktop", Kind: "claude", Path: exe, Available: true}}
	}
	calls := new(int)
	local.Stop = func(context.Context, launcher.Plan) error { *calls++; return nil }
	local.Start = func(context.Context, launcher.Plan) (int, error) { *calls++; return 212, nil }
	service := launcher.NewClaudeDesktop3P(local)
	service.Platform = func(context.Context, launcher.Tool) (launcher.Claude3PPlatform, error) {
		return launcher.Claude3PPlatform{Root: root, Version: toolcfg.ClaudeDesktop3PVersion}, nil
	}
	s := New(Deps{Store: store, Local: local, ClaudeDesktop: service, Port: 47831})
	hdr := map[string]string{"Cookie": CookieName + "=" + s.Token(), "Content-Type": "application/json", "Origin": "http://127.0.0.1:47831"}
	return s, s.Handler(), hdr, root, calls
}
func TestClaudeDesktop3PConsoleContractHasNoSecretsAndOptionalModel(t *testing.T) {
	s, h, hdr, _, calls := consoleClaude3PFixture(t)
	host := "127.0.0.1:47831"
	status := do(h, "GET", "/api/local/claude-desktop/status", host, hdr, "")
	if status.Code != 200 || strings.Contains(status.Body.String(), "fixture-only-secret") {
		t.Fatal(status.Code, status.Body.String())
	}
	var fields map[string]any
	_ = json.Unmarshal(status.Body.Bytes(), &fields)
	for _, name := range []string{"available", "supported", "managed", "version", "mode", "requiresModeChange", "recoverable", "message"} {
		if _, ok := fields[name]; !ok {
			t.Fatal("missing status field", name)
		}
	}
	for _, body := range []string{`{"id":"shared-api"}`, `{"id":"shared-api","confirmed":true}`} {
		w := do(h, "POST", "/api/local/claude-desktop/switch", host, hdr, body)
		if w.Code != 400 || *calls != 0 {
			t.Fatal("unrequested mode change/process mutation", w.Code)
		}
	}
	w := do(h, "POST", "/api/local/claude-desktop/switch", host, hdr, `{"id":"shared-api","confirmed":true,"allowModeChange":true}`)
	if w.Code != 200 || strings.Contains(w.Body.String(), "fixture-only-secret") {
		t.Fatal(w.Code, w.Body.String())
	}
	if *calls != 2 {
		t.Fatal("missing safe stop/start")
	}
	applied := s.d.Store.Get().ToolAPIApplied["claude-desktop"]
	if applied.AccountID != "shared-api" || applied.Provider != "gateway" {
		t.Fatal("shared API binding not recorded")
	}
	if _, ok := s.d.Store.Get().ToolAPIApplied["claude"]; ok {
		t.Fatal("Claude CLI binding modified")
	}
	w = do(h, "POST", "/api/local/claude-desktop/restore", host, hdr, `{"confirmed":true}`)
	if w.Code != 200 || *calls != 4 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, ok := s.d.Store.Get().ToolAPIApplied["claude-desktop"]; ok {
		t.Fatal("restored binding not cleared")
	}
}
func TestClaudeDesktop3PConsoleProtectionAndManagedStatus(t *testing.T) {
	s, h, hdr, _, calls := consoleClaude3PFixture(t)
	host := "127.0.0.1:47831"
	if do(h, "GET", "/api/local/claude-desktop/status", host, nil, "").Code != 401 {
		t.Fatal("missing-cookie accepted")
	}
	hdr["Origin"] = "https://foreign.test"
	if do(h, "POST", "/api/local/claude-desktop/switch", host, hdr, `{"id":"shared-api","confirmed":true,"allowModeChange":true}`).Code != 403 || *calls != 0 {
		t.Fatal("cross-origin drive accepted")
	}
	hdr["Origin"] = "http://127.0.0.1:47831"
	s.d.ClaudeDesktop.Platform = func(context.Context, launcher.Tool) (launcher.Claude3PPlatform, error) {
		return launcher.Claude3PPlatform{Version: toolcfg.ClaudeDesktop3PVersion, Managed: true}, nil
	}
	w := do(h, "GET", "/api/local/claude-desktop/status", host, hdr, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"managed":true`) || strings.Contains(w.Body.String(), "fixture-only-secret") {
		t.Fatal(w.Code, w.Body.String())
	}
	w = do(h, "POST", "/api/local/claude-desktop/switch", host, hdr, `{"id":"shared-api","confirmed":true,"allowModeChange":true}`)
	if w.Code != 400 || *calls != 0 {
		t.Fatal("managed mutation accepted")
	}
}

func TestClaudeDesktop3PConsoleMixedCatalogUsesLocalAuthAndPreservesVault(t *testing.T) {
	s, h, hdr, root, calls := consoleClaude3PFixture(t)
	account := config.LocalAccount{ID: "shared-api", Name: "Mixed model API", Kind: "api", BaseURL: "https://upstream-fixture.test/prefix", Key: "upstream-fixture-private-secret", AuthMode: "api-key", Wire: "chat", Model: "old-default-not-in-this-key-catalog", Models: []string{"deepseek/reasoner", "grok-4-fixture", "claude-sonnet-4-5", "https://provider-fixture.test/models/another"}}
	const localKey = "local-gateway-fixture-secret"
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.LocalAccounts = []config.LocalAccount{account}
		c.GatewayKey = localKey
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	request := `{"id":"shared-api","confirmed":true,"allowModeChange":true}`
	var firstCatalog json.RawMessage
	for attempt := 0; attempt < 2; attempt++ {
		w := do(h, "POST", "/api/local/claude-desktop/switch", "127.0.0.1:47831", hdr, request)
		if w.Code != 200 || strings.Contains(w.Body.String(), account.Key) || strings.Contains(w.Body.String(), localKey) {
			t.Fatal("switch failed or returned a credential", w.Code, w.Body.String())
		}
		state, err := toolcfg.InspectClaude3P(root)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(root, "configLibrary", state.ProfileID+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var profile map[string]json.RawMessage
		if err := json.Unmarshal(data, &profile); err != nil {
			t.Fatal(err)
		}
		if string(profile["inferenceGatewayBaseUrl"]) != `"http://127.0.0.1:47831/gateway/claude-desktop"` || string(profile["inferenceGatewayApiKey"]) != `"`+localKey+`"` || string(profile["inferenceGatewayAuthScheme"]) != `"bearer"` || strings.Contains(string(data), account.Key) {
			t.Fatal("native configuration did not isolate upstream credentials behind loopback")
		}
		var models []map[string]any
		if err := json.Unmarshal(profile["inferenceModels"], &models); err != nil {
			t.Fatal(err)
		}
		if len(models) != len(account.Models) || strings.Contains(string(profile["inferenceModels"]), account.Model) {
			t.Fatal("actual catalog incomplete or hidden default inserted")
		}
		for i, original := range account.Models {
			route := config.ClaudeDesktopModelRoute(original)
			if models[i]["name"] != route {
				t.Fatal("model route not stable/current-key scoped", original)
			}
			if route != original && (models[i]["labelOverride"] != original || models[i]["supports1m"] != false || models[i]["prefer1m"] != false) {
				t.Fatal("compatibility route hid its actual model or invented capabilities")
			}
			if route != original {
				if _, exists := models[i]["maxEffort"]; exists {
					t.Fatal("foreign route was assigned an effort capability")
				}
			}
		}
		if attempt == 0 {
			firstCatalog = append(json.RawMessage{}, profile["inferenceModels"]...)
		} else if !reflect.DeepEqual(firstCatalog, profile["inferenceModels"]) {
			t.Fatal("same key catalog aliases/labels changed on reapply")
		}
		current := s.d.Store.Get()
		stored, ok := current.LocalAccount(account.ID)
		if !ok {
			t.Fatal("vault account disappeared")
		}
		stored.Target, stored.LastUsedAt = account.Target, account.LastUsedAt
		if !reflect.DeepEqual(stored, account) {
			t.Fatal("launcher compatibility routes or credentials mutated the shared vault entry")
		}
		applied, ok := current.AppliedToolAccount("claude-desktop")
		if !ok || applied.Key != account.Key || applied.BaseURL != account.BaseURL || !reflect.DeepEqual(applied.Models, account.Models) || !current.ToolCatalogOverride["claude-desktop"] {
			t.Fatal("applied selection recorded loopback instead of actual upstream snapshot")
		}
	}
	if *calls != 4 {
		t.Fatal("reapply bypassed graceful stop/start")
	}
}

func TestClaudeDesktop3PConsoleCatalogOverrideOffKeepsNativeMenu(t *testing.T) {
	s, h, hdr, root, calls := consoleClaude3PFixture(t)
	const id = "11111111-1111-4111-8111-111111111111"
	const originalModels = `[{"name":"claude-original-fixture","labelOverride":"Original menu","supports1m":true,"keep":"metadata"}]`
	files := map[string]string{
		"claude_desktop_config.json":    `{"deploymentMode":"3p","mcpServers":{"keep":{}}}`,
		"configLibrary/_meta.json":      `{"appliedId":"` + id + `","entries":[{"id":"` + id + `","name":"Existing"}]}`,
		"configLibrary/" + id + ".json": `{"inferenceProvider":"gateway","inferenceGatewayBaseUrl":"https://original-fixture.test","deploymentOrganizationUuid":"22222222-2222-4222-8222-222222222222","inferenceModels":` + originalModels + `,"modelDiscoveryEnabled":false,"nativeModelPreference":"claude-original-fixture"}`,
	}
	for rel, data := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.LocalAccounts[0].Models = []string{"deepseek-fixture", "grok-fixture"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := s.d.Store.Get().LocalAccounts[0]
	w := do(h, "POST", "/api/local/claude-desktop/switch", "127.0.0.1:47831", hdr, `{"id":"shared-api","confirmed":true,"allowModeChange":true,"catalogOverride":false}`)
	if w.Code != 200 || *calls != 2 {
		t.Fatal(w.Code, w.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(root, "configLibrary", id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var profile map[string]json.RawMessage
	_ = json.Unmarshal(data, &profile)
	var expected, got any
	_ = json.Unmarshal([]byte(originalModels), &expected)
	_ = json.Unmarshal(profile["inferenceModels"], &got)
	if !reflect.DeepEqual(expected, got) || string(profile["modelDiscoveryEnabled"]) != "false" || string(profile["nativeModelPreference"]) != `"claude-original-fixture"` {
		t.Fatal("disabled override changed native menu/model preference")
	}
	if string(profile["inferenceGatewayBaseUrl"]) != `"http://127.0.0.1:47831/gateway/claude-desktop"` || strings.Contains(string(data), before.Key) {
		t.Fatal("disabled override bypassed local gateway credential isolation")
	}
	for _, rel := range []string{"claude_desktop_config.json", "configLibrary/_meta.json"} {
		data, _ := os.ReadFile(filepath.Join(root, rel))
		if string(data) != files[rel] {
			t.Fatal("existing mode/library identity rewritten")
		}
	}
	current := s.d.Store.Get()
	stored := current.LocalAccounts[0]
	stored.Target, stored.LastUsedAt = before.Target, before.LastUsedAt
	if current.ToolCatalogOverride["claude-desktop"] || !reflect.DeepEqual(stored, before) {
		t.Fatal("disabled choice not recorded or actual catalog mutated")
	}
}
