package launcher

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/toolcfg"
)

const cd3PID = "11111111-1111-4111-8111-111111111111"

func fixtureClaude3P(t *testing.T, existing bool) (*ClaudeDesktop3PService, string, config.LocalAccount, *int, *int) {
	t.Helper()
	local := New(t.TempDir())
	tool := stubTool(t, "claude-desktop")
	local.FindTools = func(context.Context, map[string]string) []Tool { return []Tool{tool} }
	starts, stops := new(int), new(int)
	local.Start = func(_ context.Context, p Plan) (int, error) {
		*starts++
		if p.Tool.ID != "claude-desktop" || len(p.Args) != 0 || p.AppData != p.ProfileDir {
			t.Fatal("isolated profile or wrong app")
		}
		for _, env := range p.Environment {
			if strings.HasPrefix(env, "CLAUDE_USER_DATA_DIR=") || strings.Contains(env, "fixture-private-key") {
				t.Fatal("credentials/profile overrides leaked into process")
			}
		}
		return 111, nil
	}
	local.Stop = func(context.Context, Plan) error { *stops++; return nil }
	root := t.TempDir()
	service := NewClaudeDesktop3P(local)
	service.Platform = func(context.Context, Tool) (Claude3PPlatform, error) {
		return Claude3PPlatform{Root: root, Version: toolcfg.ClaudeDesktop3PVersion}, nil
	}
	if existing {
		fixtureFile(t, root, "claude_desktop_config.json", `{"deploymentMode":"3p","mcpServers":{"existing":{}}}`)
		fixtureFile(t, root, "configLibrary/_meta.json", `{"appliedId":"`+cd3PID+`","entries":[{"id":"`+cd3PID+`","name":"Original"}]}`)
		fixtureFile(t, root, "configLibrary/"+cd3PID+".json", `{"inferenceProvider":"gateway","inferenceGatewayBaseUrl":"https://old.test","inferenceGatewayApiKey":"old-fixture-key","inferenceModels":[{"name":"claude-sonnet-4-5","supports1m":true}]}`)
	}
	fixtureFile(t, root, "local-agent-mode-sessions/chat.json", "fixture-conversation-must-not-change")
	a := config.LocalAccount{ID: "shared", Kind: "api", Name: "Fixture API", BaseURL: "https://new.test", Key: "fixture-private-key", AuthMode: "bearer", Models: []string{"claude-sonnet-4-5", "gpt-test"}, CatalogOverride: true}
	return service, root, a, starts, stops
}
func TestClaudeDesktop3PFirstSetupSwitchAndRestore(t *testing.T) {
	s, root, a, starts, stops := fixtureClaude3P(t, false)
	before := fixtureTree(t, root)
	status := s.Status(context.Background(), nil)
	if !status.Supported || !status.RequiresModeChange || status.Recoverable {
		t.Fatal(status)
	}
	if _, err := s.Switch(context.Background(), a, nil, false); err == nil || *starts != 0 || *stops != 0 {
		t.Fatal("implicit first setup occurred")
	}
	if _, err := s.Switch(context.Background(), a, nil, true); err != nil {
		t.Fatal(err)
	}
	if *starts != 1 || *stops != 1 {
		t.Fatal("wrong process sequence")
	}
	if !s.Status(context.Background(), nil).Recoverable {
		t.Fatal("backup unavailable")
	}
	state, err := toolcfg.InspectClaude3P(root)
	if err != nil || state.Mode != "third-party" {
		t.Fatal("invalid official config", err)
	}
	if _, err := s.Restore(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	after := fixtureTree(t, root)
	for path, value := range before {
		if after[path] != value {
			t.Fatalf("original altered: %s", path)
		}
	}
	for path := range after {
		if strings.HasSuffix(path, ".json") && before[path] == "" {
			t.Fatalf("created config not removed: %s", path)
		}
	}
	if *starts != 2 || *stops != 2 || s.Status(context.Background(), nil).Recoverable {
		t.Fatal("restore/restart incorrect")
	}
}
func TestClaudeDesktop3PExistingKeepsChatScopeAcrossAPIChanges(t *testing.T) {
	s, root, a, _, _ := fixtureClaude3P(t, true)
	meta, _ := os.ReadFile(filepath.Join(root, "configLibrary", "_meta.json"))
	mode, _ := os.ReadFile(filepath.Join(root, "claude_desktop_config.json"))
	if _, err := s.Switch(context.Background(), a, nil, false); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(filepath.Join(root, "configLibrary", cd3PID+".json"))
	var cfg map[string]json.RawMessage
	_ = json.Unmarshal(first, &cfg)
	scope := string(cfg["deploymentOrganizationUuid"])
	a.BaseURL, a.Key = "https://another.test", "next-fixture-key"
	if _, err := s.Switch(context.Background(), a, nil, false); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(filepath.Join(root, "configLibrary", cd3PID+".json"))
	_ = json.Unmarshal(second, &cfg)
	if string(cfg["deploymentOrganizationUuid"]) != scope {
		t.Fatal("scope changed")
	}
	gotMeta, _ := os.ReadFile(filepath.Join(root, "configLibrary", "_meta.json"))
	gotMode, _ := os.ReadFile(filepath.Join(root, "claude_desktop_config.json"))
	if !reflect.DeepEqual(meta, gotMeta) || !reflect.DeepEqual(mode, gotMode) {
		t.Fatal("original metadata/MCP rewritten")
	}
	if _, err := s.Restore(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(filepath.Join(root, "configLibrary", cd3PID+".json"))
	if !strings.Contains(string(restored), "old-fixture-key") || strings.Contains(string(restored), "next-fixture-key") {
		t.Fatal("restore was not original configuration")
	}
}
func TestClaudeDesktop3PFailuresRollbackAndProtectExternalChanges(t *testing.T) {
	for _, failure := range []string{"stop", "start", "managed", "version", "protocol", "external"} {
		t.Run(failure, func(t *testing.T) {
			s, root, a, starts, stops := fixtureClaude3P(t, true)
			before := fixtureTree(t, root)
			switch failure {
			case "stop":
				s.Local.Stop = func(context.Context, Plan) error { return errors.New("fixture-private-key") }
			case "start":
				s.Local.Start = func(context.Context, Plan) (int, error) { return 0, errors.New("fixture-private-key") }
			case "managed":
				s.Platform = func(context.Context, Tool) (Claude3PPlatform, error) {
					return Claude3PPlatform{Root: root, Version: toolcfg.ClaudeDesktop3PVersion, Managed: true}, nil
				}
			case "version":
				s.Platform = func(context.Context, Tool) (Claude3PPlatform, error) {
					return Claude3PPlatform{Root: root, Version: "99.0.0"}, nil
				}
			case "protocol":
				a.Wire = "chat"
			case "external":
				if _, err := s.Switch(context.Background(), a, nil, false); err != nil {
					t.Fatal(err)
				}
				fixtureFile(t, root, "configLibrary/"+cd3PID+".json", `{"changedByUser":true}`)
				before = fixtureTree(t, root)
				*starts, *stops = 0, 0
			}
			_, err := s.Switch(context.Background(), a, nil, false)
			if err == nil || strings.Contains(err.Error(), "fixture-private-key") {
				t.Fatal("unsafe or unsanitized failure", err)
			}
			if !reflect.DeepEqual(before, fixtureTree(t, root)) {
				t.Fatal("failure modified app configuration/session files")
			}
			if failure != "stop" && failure != "start" && (*starts != 0 || *stops != 0) {
				t.Fatal("validation failure had process side effect")
			}
		})
	}
}
func TestClaudeDesktop3POptionalModelAndDiscovery(t *testing.T) {
	s, root, a, _, _ := fixtureClaude3P(t, false)
	a.Model = ""
	a.Models = nil
	if _, err := s.Switch(context.Background(), a, nil, true); err != nil {
		t.Fatal(err)
	}
	state, err := toolcfg.InspectClaude3P(root)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(root, "configLibrary", state.ProfileID+".json"))
	if strings.Contains(string(b), "inferenceModels") || !strings.Contains(string(b), `"modelDiscoveryEnabled": true`) {
		t.Fatal("no-model setup did not use native discovery")
	}
}

func TestClaudeDesktop3PCatalogOffRestoresOriginalPickerOnly(t *testing.T) {
	s, root, a, _, _ := fixtureClaude3P(t, true)
	profilePath := filepath.Join(root, "configLibrary", cd3PID+".json")
	fixtureFile(t, root, "configLibrary/"+cd3PID+".json", `{"inferenceProvider":"gateway","inferenceGatewayBaseUrl":"https://old.test","inferenceGatewayApiKey":"old-fixture-key","inferenceModels":[{"name":"claude-original","supports1m":true,"labelOverride":"Original"}],"modelDiscoveryEnabled":false,"lastSelectedModel":"claude-original","userField":{"keep":true}}`)
	readProfile := func() map[string]any {
		t.Helper()
		data, err := os.ReadFile(profilePath)
		if err != nil {
			t.Fatal(err)
		}
		var profile map[string]any
		if err := json.Unmarshal(data, &profile); err != nil {
			t.Fatal(err)
		}
		return profile
	}
	original := readProfile()
	if _, err := s.Switch(context.Background(), a, nil, false); err != nil {
		t.Fatal(err)
	}
	applied := readProfile()
	a.CatalogOverride, a.BaseURL, a.Key = false, "https://next.test", "next-fixture-key"
	if _, err := s.Switch(context.Background(), a, nil, false); err != nil {
		t.Fatal(err)
	}
	got := readProfile()
	for _, field := range []string{"inferenceModels", "modelDiscoveryEnabled", "lastSelectedModel", "userField"} {
		if !reflect.DeepEqual(got[field], original[field]) {
			t.Fatalf("original picker/user field %s was not restored or preserved", field)
		}
	}
	if got["inferenceGatewayBaseUrl"] != a.BaseURL || got["inferenceGatewayApiKey"] != a.Key || got["deploymentOrganizationUuid"] != applied["deploymentOrganizationUuid"] {
		t.Fatal("turning off the menu reverted new credentials or chat identity")
	}
	// The saved applied snapshot must track the restored menu for future switches.
	a.CatalogOverride = true
	if _, err := s.Switch(context.Background(), a, nil, false); err != nil {
		t.Fatal("subsequent override failed", err)
	}
	a.CatalogOverride = false
	if _, err := s.Switch(context.Background(), a, nil, false); err != nil {
		t.Fatal("subsequent disable failed", err)
	}
	if !reflect.DeepEqual(readProfile()["inferenceModels"], original["inferenceModels"]) {
		t.Fatal("original picker was lost after repeated toggles")
	}
}

func TestClaudeDesktop3PCatalogOffAfterFreshSetupUsesDiscovery(t *testing.T) {
	s, root, a, _, _ := fixtureClaude3P(t, false)
	if _, err := s.Switch(context.Background(), a, nil, true); err != nil {
		t.Fatal(err)
	}
	before, err := toolcfg.InspectClaude3P(root)
	if err != nil {
		t.Fatal(err)
	}
	a.CatalogOverride = false
	if _, err := s.Switch(context.Background(), a, nil, false); err != nil {
		t.Fatal(err)
	}
	got, err := toolcfg.InspectClaude3P(root)
	if err != nil || got.CatalogOverride || !got.ModelDiscovery || got.ProfileID != before.ProfileID {
		t.Fatal("fresh profile did not return to native discovery with its original chat identity", err)
	}
}

func TestClaudeDesktop3PCatalogOffProtectsEditsDuringQuit(t *testing.T) {
	s, root, a, starts, _ := fixtureClaude3P(t, true)
	if _, err := s.Switch(context.Background(), a, nil, false); err != nil {
		t.Fatal(err)
	}
	profilePath := filepath.Join(root, "configLibrary", cd3PID+".json")
	data, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	var profile map[string]any
	if err := json.Unmarshal(data, &profile); err != nil {
		t.Fatal(err)
	}
	profile["inferenceModels"] = []string{"external-user-model"}
	external, _ := json.Marshal(profile)
	s.Local.Stop = func(context.Context, Plan) error {
		return os.WriteFile(profilePath, external, 0o600)
	}
	a.CatalogOverride = false
	if _, err := s.Switch(context.Background(), a, nil, false); err == nil {
		t.Fatal("concurrent external menu edit was overwritten")
	}
	got, _ := os.ReadFile(profilePath)
	if !reflect.DeepEqual(got, external) || *starts != 1 {
		t.Fatal("external menu changed or new process started after conflict")
	}
}

func TestClaudeDesktop3PCatalogOffLaunchFailureRollsBackCurrentMenu(t *testing.T) {
	s, root, a, _, _ := fixtureClaude3P(t, true)
	if _, err := s.Switch(context.Background(), a, nil, false); err != nil {
		t.Fatal(err)
	}
	before := fixtureTree(t, root)
	a.CatalogOverride = false
	s.Local.Start = func(context.Context, Plan) (int, error) { return 0, errors.New("fixture failure") }
	if _, err := s.Switch(context.Background(), a, nil, false); err == nil {
		t.Fatal("start failure was ignored")
	}
	if !reflect.DeepEqual(before, fixtureTree(t, root)) {
		t.Fatal("failed disable did not restore the prior applied menu/profile")
	}
}

func TestClaudeDesktop3PLoopbackGatewayCatalogAliases(t *testing.T) {
	s, root, a, _, _ := fixtureClaude3P(t, true)
	a.Wire, a.Protocol = "chat", "chat"
	a.BaseURL = "http://127.0.0.1:5000/gateway/claude-desktop"
	a.Model = "legacy-metadata-not-in-catalog"
	a.Models = []string{"claude-salcara-v1-1234567890"}
	a.DesktopModelLabels = map[string]string{a.Models[0]: "grok-model-fixture"}
	if _, err := s.Switch(context.Background(), a, nil, false); err != nil {
		t.Fatal("explicit loopback conversion rejected", err)
	}
	b, _ := os.ReadFile(filepath.Join(root, "configLibrary", cd3PID+".json"))
	var cfg map[string]json.RawMessage
	_ = json.Unmarshal(b, &cfg)
	var models []map[string]any
	_ = json.Unmarshal(cfg["inferenceModels"], &models)
	if len(models) != 1 || models[0]["name"] != a.Models[0] || models[0]["labelOverride"] != a.DesktopModelLabels[a.Models[0]] || models[0]["supports1m"] != false {
		t.Fatal("native route and honest display label were not passed through")
	}
	if strings.Contains(string(cfg["inferenceModels"]), a.Model) {
		t.Fatal("hidden default was used as a native selection/catalog permission")
	}
	status := s.Status(context.Background(), nil)
	if !status.CatalogOverride || status.ModelDiscovery {
		t.Fatal("native menu status not exposed")
	}
}

func TestClaudeDesktop3PRemoteCannotClaimLoopbackConversion(t *testing.T) {
	for _, base := range []string{"https://remote.test/gateway/claude-desktop", "http://127.0.0.1/gateway/claude-desktop", "http://127.0.0.1:5000/gateway/claude", "http://localhost:5000/gateway/claude-desktop", "http://127.0.0.1:65536/gateway/claude-desktop"} {
		t.Run(base, func(t *testing.T) {
			s, root, a, starts, stops := fixtureClaude3P(t, true)
			before := fixtureTree(t, root)
			a.Wire, a.Protocol, a.BaseURL = "chat", "chat", base
			if _, err := s.Switch(context.Background(), a, nil, false); err == nil {
				t.Fatal("non-dedicated conversion route accepted")
			}
			if *starts != 0 || *stops != 0 || !reflect.DeepEqual(before, fixtureTree(t, root)) {
				t.Fatal("protocol validation changed app state")
			}
		})
	}
}

func TestClaudeDesktop3PFirstLaunchFailureCanRetrySameProfile(t *testing.T) {
	s, root, a, _, _ := fixtureClaude3P(t, false)
	start := s.Local.Start
	s.Local.Start = func(context.Context, Plan) (int, error) { return 0, errors.New("fixture failure") }
	if _, err := s.Switch(context.Background(), a, nil, true); err == nil {
		t.Fatal("first launch unexpectedly succeeded")
	}
	backup, err := s.readBackup(root)
	if err != nil {
		t.Fatal(err)
	}
	s.Local.Start = start
	if _, err := s.Switch(context.Background(), a, nil, true); err != nil {
		t.Fatal("first setup retry blocked", err)
	}
	state, err := toolcfg.InspectClaude3P(root)
	if err != nil || state.ProfileID != backup.ProfileID {
		t.Fatal("retry created another profile", err)
	}
}

func TestClaudeDesktop3PLargeBackupRemainsRecoverable(t *testing.T) {
	s, root, a, _, _ := fixtureClaude3P(t, true)
	fixtureFile(t, root, "configLibrary/"+cd3PID+".json", `{"inferenceProvider":"gateway","inferenceGatewayBaseUrl":"https://old.test","unrelated":"`+strings.Repeat("x", 400<<10)+`"}`)
	if _, err := s.Switch(context.Background(), a, nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.readBackup(root); err != nil {
		t.Fatal("valid source configuration yielded unreadable backup", err)
	}
	if _, err := s.Restore(context.Background(), nil); err != nil {
		t.Fatal("large backup cannot be restored", err)
	}
}
func TestClaudeDesktop3PRestoreRejectsTamperedBackup(t *testing.T) {
	s, _, a, _, _ := fixtureClaude3P(t, true)
	if _, err := s.Switch(context.Background(), a, nil, false); err != nil {
		t.Fatal(err)
	}
	b, err := s.readBackup(s.StatusRootFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	b.Original[0].Path = filepath.Join(t.TempDir(), "do-not-write.json")
	if err := s.saveBackup(b); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Restore(context.Background(), nil); err == nil {
		t.Fatal("tampered backup followed")
	}
}
func (s *ClaudeDesktop3PService) StatusRootFixture(t *testing.T) string {
	t.Helper()
	p, err := s.Platform(context.Background(), Tool{})
	if err != nil {
		t.Fatal(err)
	}
	return p.Root
}
func TestDistributionClaudeVersionReadsOnlyArchiveMetadata(t *testing.T) {
	pkg := []byte(`{"name":"@ant/desktop","productName":"Claude","version":"2.16120.0"}`)
	header, _ := json.Marshal(map[string]any{"files": map[string]any{"package.json": map[string]any{"size": len(pkg), "offset": "0"}}})
	prefix := make([]byte, 16)
	binary.LittleEndian.PutUint32(prefix[4:], uint32(len(header)+8))
	binary.LittleEndian.PutUint32(prefix[12:], uint32(len(header)))
	archive := append(prefix, header...)
	archive = append(archive, pkg...)
	path := filepath.Join(t.TempDir(), "app.asar")
	if err := os.WriteFile(path, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	version, err := distributionClaudeVersion(path)
	if err != nil || version != toolcfg.ClaudeDesktop3PVersion {
		t.Fatal(version, err)
	}
}
