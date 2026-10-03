package toolcfg

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const test3PID = "11111111-1111-4111-8111-111111111111"

func file3P(t *testing.T, root, rel, data string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}
func existing3P(t *testing.T, root string) {
	t.Helper()
	file3P(t, root, "claude_desktop_config.json", `{"deploymentMode":"3p","mcpServers":{"keep":{"command":"fixture"}}}`)
	file3P(t, root, "configLibrary/_meta.json", `{"appliedId":"`+test3PID+`","entries":[{"id":"`+test3PID+`","name":"Original"}],"keep":true}`)
	file3P(t, root, "configLibrary/"+test3PID+".json", `{"inferenceProvider":"gateway","inferenceGatewayBaseUrl":"https://SALCARA.TOP:8443/","inferenceGatewayApiKey":"old-fixture-key","inferenceModels":[{"name":"claude-sonnet-4-5","supports1m":true},"claude-opus-4-1"],"isDesktopExtensionEnabled":true}`)
}
func TestClaude3PExistingPreservesIdentityAndOtherConfiguration(t *testing.T) {
	root := t.TempDir()
	existing3P(t, root)
	state, err := InspectClaude3P(root)
	if err != nil {
		t.Fatal(err)
	}
	p, err := PrepareClaude3P(state, Claude3PGateway{BaseURL: "https://new.test", Key: "new-fixture-key", AuthMode: "api-key", Models: []string{"gpt-test", "claude-opus-4-1", "claude-sonnet-4-5"}, CatalogOverride: true}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if p.ProfileID != test3PID || p.MetaData != nil || p.ModeData != nil {
		t.Fatal("existing identity/mode was rewritten")
	}
	var cfg map[string]json.RawMessage
	if json.Unmarshal(p.ProfileData, &cfg) != nil {
		t.Fatal("invalid profile")
	}
	if string(cfg["deploymentOrganizationUuid"]) != `"173e116f-8ff1-51e3-a2f6-2db491665bf6"` {
		t.Fatal("original effective chat scope was not pinned")
	}
	if string(cfg["inferenceGatewayAuthScheme"]) != `"x-api-key"` || !strings.Contains(string(cfg["inferenceModels"]), "supports1m") || string(cfg["isDesktopExtensionEnabled"]) != "true" {
		t.Fatal("incorrect merge")
	}
	var models []json.RawMessage
	_ = json.Unmarshal(cfg["inferenceModels"], &models)
	var first map[string]any
	_ = json.Unmarshal(models[0], &first)
	if first["name"] != "claude-sonnet-4-5" || len(models) != 3 {
		t.Fatal("picker order was overridden")
	}
}
func TestClaude3PFirstSetupUsesUUIDAndNativeDiscoveryWithoutModel(t *testing.T) {
	root := t.TempDir()
	file3P(t, root, "claude_desktop_config.json", `{"deploymentMode":"1p","mcpServers":{"keep":{}}}`)
	state, err := InspectClaude3P(root)
	if err != nil {
		t.Fatal(err)
	}
	args := Claude3PGateway{BaseURL: "https://fixture.test", Key: "fixture-key", AuthMode: "bearer"}
	if _, err := PrepareClaude3P(state, args, test3PID, false); err == nil {
		t.Fatal("silent mode change")
	}
	p, err := PrepareClaude3P(state, args, test3PID, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(p.ModeData), `"mcpServers"`) || !strings.Contains(string(p.MetaData), test3PID) || strings.Contains(string(p.ProfileData), "inferenceModels") || !strings.Contains(string(p.ProfileData), `"modelDiscoveryEnabled": true`) {
		t.Fatal("first setup did not preserve MCP/use native discovery")
	}
	if err := p.Apply(); err != nil {
		t.Fatal(err)
	}
	got, err := InspectClaude3P(root)
	if err != nil || got.Mode != "third-party" || got.ProfileID != test3PID {
		t.Fatalf("applied: %v %v", got, err)
	}
}
func TestClaude3PGatewayScopeMatchesVerifiedDistributionJS(t *testing.T) {
	for base, want := range map[string]string{"https://salcara.top": "173e116f-8ff1-51e3-a2f6-2db491665bf6", "https://SALCARA.TOP:8443/": "173e116f-8ff1-51e3-a2f6-2db491665bf6", "https://api.example.test/a%20b/": "2a010ad3-5e46-5359-a342-6853ac768f79", "https://api.example.test/v1": "2946ff3f-21cc-5f22-9971-21d3811f388b", "https://[::1]:443/": "03eaebe4-a801-5818-90cf-95037ea043a8"} {
		got, err := gatewayScope3P(base)
		if err != nil || got != want {
			t.Errorf("scope %s != native JS: %s %v", base, got, err)
		}
	}
	if _, err := gatewayScope3P("https://fixture.test/a/../b"); err == nil {
		t.Fatal("ambiguous URL path guessed")
	}
}
func TestClaude3PRejectsManagedOrInvalidLocalLibrary(t *testing.T) {
	for _, kind := range []string{"hybrid", "bootstrap", "invalid-id", "missing-profile", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			existing3P(t, root)
			switch kind {
			case "hybrid":
				file3P(t, root, "configLibrary/_meta.json", `{"hybridPointer":{"bootstrapUrl":"https://fixture.test"}}`)
			case "bootstrap":
				file3P(t, root, "configLibrary/"+test3PID+".json", `{"bootstrapUrl":"https://fixture.test"}`)
			case "invalid-id":
				file3P(t, root, "configLibrary/_meta.json", `{"appliedId":"salcara-bridge-current","entries":[]}`)
			case "missing-profile":
				if err := os.Remove(filepath.Join(root, "configLibrary", test3PID+".json")); err != nil {
					t.Fatal(err)
				}
			case "malformed":
				file3P(t, root, "claude_desktop_config.json", "not-json")
			}
			if _, err := InspectClaude3P(root); err == nil {
				t.Fatal("unsafe library accepted")
			}
		})
	}
}
func TestClaude3PHeaderConflictAndModelBoundary(t *testing.T) {
	root := t.TempDir()
	existing3P(t, root)
	file3P(t, root, "configLibrary/"+test3PID+".json", `{"inferenceProvider":"gateway","inferenceGatewayBaseUrl":"https://fixture.test","inferenceCustomHeaders":{"Authorization":"old-fixture-secret"}}`)
	state, err := InspectClaude3P(root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = PrepareClaude3P(state, Claude3PGateway{BaseURL: "https://new.test", Key: "new-fixture-secret", AuthMode: "bearer"}, "", false)
	if err == nil || strings.Contains(err.Error(), "fixture-secret") {
		t.Fatal("credential conflict not safely rejected")
	}
	for _, model := range []string{"", " ", "claude-sonnet\nsecret", "model\tsecret", "model\u007fsecret", strings.Repeat("x", 201), string([]byte{0xff})} {
		if ValidClaudeDesktopModel(model) {
			t.Fatal("invalid model data accepted")
		}
	}
	for _, model := range []string{"grok-test", "gpt-test", "deepseek/reasoner", "anthropic/claude-sonnet-4-5", "https://provider.test/models/x", "供应商/模型一"} {
		if !ValidClaudeDesktopModel(model) {
			t.Fatalf("valid upstream ID rejected: %q", model)
		}
	}
}

func TestClaude3PCatalogStrictlyReplacesCurrentKeyMenuWithoutDefault(t *testing.T) {
	root := t.TempDir()
	existing3P(t, root)
	state, err := InspectClaude3P(root)
	if err != nil {
		t.Fatal(err)
	}
	args := Claude3PGateway{BaseURL: "https://fixture.test", Key: "fixture-key", AuthMode: "bearer", Model: "old-default-not-granted", Models: []string{"provider/model", "deepseek-chat", "provider/model"}, CatalogOverride: true}
	p, err := PrepareClaude3P(state, args, "", false)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]json.RawMessage
	_ = json.Unmarshal(p.ProfileData, &cfg)
	var models []map[string]string
	if err := json.Unmarshal(cfg["inferenceModels"], &models); err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0]["name"] != "provider/model" || models[1]["name"] != "deepseek-chat" || string(cfg["modelDiscoveryEnabled"]) != "false" {
		t.Fatal("not an exact deduplicated current-key catalog")
	}
	if strings.Contains(string(cfg["inferenceModels"]), "claude-sonnet") || strings.Contains(string(cfg["inferenceModels"]), args.Model) {
		t.Fatal("old key catalog or hidden default survived")
	}
	if err := p.Apply(); err != nil {
		t.Fatal(err)
	}
	got, err := InspectClaude3P(root)
	if err != nil || !got.CatalogOverride || got.ModelDiscovery {
		t.Fatal("catalog status not reflected", err)
	}
	args.Models = nil
	p, err = PrepareClaude3P(got, args, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(p.ProfileData), "inferenceModels") || !strings.Contains(string(p.ProfileData), `"modelDiscoveryEnabled": true`) {
		t.Fatal("empty replacement reused old models instead of discovery")
	}
}

func TestClaude3PCatalogDisabledPreservesNativeMenuAndPreference(t *testing.T) {
	root := t.TempDir()
	existing3P(t, root)
	file3P(t, root, "configLibrary/"+test3PID+".json", `{"inferenceProvider":"gateway","inferenceGatewayBaseUrl":"https://fixture.test","inferenceModels":[{"name":"claude-sonnet-4-5","supports1m":true,"keep":"original"}],"modelDiscoveryEnabled":false,"lastSelectedModel":"claude-sonnet-4-5"}`)
	state, err := InspectClaude3P(root)
	if err != nil {
		t.Fatal(err)
	}
	p, err := PrepareClaude3P(state, Claude3PGateway{BaseURL: "https://new.test", Key: "fixture-key", AuthMode: "bearer", Model: "new-default", Models: []string{"new-route"}}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]json.RawMessage
	_ = json.Unmarshal(p.ProfileData, &cfg)
	for _, field := range []string{"inferenceModels", "modelDiscoveryEnabled", "lastSelectedModel"} {
		var original, written any
		_ = json.Unmarshal(state.profile[field], &original)
		_ = json.Unmarshal(cfg[field], &written)
		originalJSON, _ := json.Marshal(original)
		writtenJSON, _ := json.Marshal(written)
		if string(originalJSON) != string(writtenJSON) {
			t.Fatalf("original field overwritten while override off: %s", field)
		}
	}
}

func TestClaude3PCatalogCompatibleRoutesHaveHonestLabels(t *testing.T) {
	root := t.TempDir()
	existing3P(t, root)
	state, err := InspectClaude3P(root)
	if err != nil {
		t.Fatal(err)
	}
	route := "claude-salcara-v1-12345678901234567890"
	args := Claude3PGateway{BaseURL: "http://127.0.0.1:5000/gateway/claude-desktop", Key: "fixture-key", AuthMode: "bearer", CatalogOverride: true, Models: []string{route}, CatalogEntries: []Claude3PModelEntry{{Name: route, LabelOverride: "deepseek/reasoner"}}}
	p, err := PrepareClaude3P(state, args, "", false)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]json.RawMessage
	_ = json.Unmarshal(p.ProfileData, &cfg)
	var models []map[string]any
	_ = json.Unmarshal(cfg["inferenceModels"], &models)
	if len(models) != 1 || models[0]["name"] != route || models[0]["labelOverride"] != "deepseek/reasoner" || models[0]["supports1m"] != false || models[0]["prefer1m"] != false {
		t.Fatal("route/display distinction or conservative context flags lost")
	}
	if _, exists := models[0]["maxEffort"]; exists {
		t.Fatal("unknown compatibility route was assigned an effort capability")
	}
	args.CatalogEntries[0].Name = "another-key-route"
	if _, err := PrepareClaude3P(state, args, "", false); err == nil {
		t.Fatal("out-of-catalog display entry accepted")
	}
}

func TestClaude3PCatalogInvalidOrOversizedNeverSilentlyTruncates(t *testing.T) {
	state, err := InspectClaude3P(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	args := Claude3PGateway{BaseURL: "https://fixture.test", Key: "fixture-key", AuthMode: "bearer", CatalogOverride: true, Models: []string{"model\nsecret"}}
	if _, err := PrepareClaude3P(state, args, test3PID, true); err == nil {
		t.Fatal("invalid catalog accepted")
	}
	args.Models = nil
	for i := 0; i < ClaudeDesktop3PCatalogLimit+1; i++ {
		args.Models = append(args.Models, fmt.Sprintf("model-%03d", i))
	}
	if _, err := PrepareClaude3P(state, args, test3PID, true); err == nil || !strings.Contains(err.Error(), "1000") {
		t.Fatal("oversized catalog was silently truncated")
	}
	args.Models = args.Models[:ClaudeDesktop3PCatalogLimit]
	p, err := PrepareClaude3P(state, args, test3PID, true)
	if err != nil {
		t.Fatal("within-budget large directory rejected", err)
	}
	var cfg map[string]json.RawMessage
	_ = json.Unmarshal(p.ProfileData, &cfg)
	var models []json.RawMessage
	_ = json.Unmarshal(cfg["inferenceModels"], &models)
	if len(models) != ClaudeDesktop3PCatalogLimit || len(p.ProfileData) > 1<<20 {
		t.Fatal("within-budget directory truncated or exceeded file budget")
	}
}

func TestClaude3PExplicitOrganizationScopeTrimMatchesNative(t *testing.T) {
	root := t.TempDir()
	existing3P(t, root)
	file3P(t, root, "configLibrary/"+test3PID+".json", `{"inferenceProvider":"gateway","inferenceGatewayBaseUrl":"https://old.test","deploymentOrganizationUuid":"  22222222-2222-4222-8222-222222222222  "}`)
	state, err := InspectClaude3P(root)
	if err != nil {
		t.Fatal(err)
	}
	p, err := PrepareClaude3P(state, Claude3PGateway{BaseURL: "https://new.test", Key: "fixture-key", AuthMode: "bearer"}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]string
	_ = json.Unmarshal(p.ProfileData, &cfg)
	if strings.TrimSpace(cfg["deploymentOrganizationUuid"]) != "22222222-2222-4222-8222-222222222222" {
		t.Fatal("native explicit scope was replaced by URL scope")
	}
}
