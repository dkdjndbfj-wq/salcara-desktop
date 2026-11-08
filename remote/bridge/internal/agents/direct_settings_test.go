package agents

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func directWorkerConfig(t *testing.T) config.Config {
	t.Helper()
	s, err := config.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.SetRuntimeGatewayKey("synthetic-worker-runtime")
	if err := s.Update(func(c *config.Config) error {
		c.DirectRelayEnabled, c.DirectPhoneID, c.DirectPhoneHash = true, "phone-worker", "synthetic-phone-hash"
		c.LocalAccounts = []config.LocalAccount{
			{ID: "local", Kind: "api", SourceType: "api_key", BaseURL: "https://local.invalid", Key: "local-key"},
			{ID: "hub", Kind: "api", SourceType: "api_key", BaseURL: "https://hub.invalid", Key: "hub-key"},
			{ID: "direct", Kind: "api", SourceType: "api_key", BaseURL: "https://direct.invalid", Key: "direct-key"},
		}
		for _, family := range []string{"codex", "claude", "deepseek-harness", "hermes"} {
			c.RecordAppliedAPI(family, c.APIForTool(c.LocalAccounts[0], family), "openai")
			if err := c.SetRemoteAPIForAgent(family, "hub", "hub-model"); err != nil {
				return err
			}
			if err := c.SetDirectAPIForAgent(family, "direct", "direct-model"); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return s.Get()
}

func TestDirectSettingsFreezeAllFamiliesAndStaySeparateFromHub(t *testing.T) {
	cfg := directWorkerConfig(t)
	before := cfg.Clone()
	want := DirectSettingsWithGateway(cfg, 47123)
	if want.UseOriginalCodex || want.CodexRoot != "http://127.0.0.1:47123/gateway/direct-codex" || want.ClaudeRoot != "http://127.0.0.1:47123/gateway/direct-claude" {
		t.Fatal("direct workers were not explicitly routed")
	}
	if want.CodexKey != cfg.DirectGatewayCredential("direct-codex") || want.ClaudeKey != cfg.DirectGatewayCredential("direct-claude") {
		t.Fatal("direct workers used another surface's credential")
	}
	for _, family := range []string{"deepseek-harness", "hermes"} {
		x := want.Extensions[family]
		if !strings.HasSuffix(x.Root, "/gateway/"+config.DirectGatewayTarget(family)) || x.Key == "" || x.Key != cfg.DirectGatewayCredential(config.DirectGatewayTarget(family)) || x.Model != "direct-model" {
			t.Fatalf("%s direct worker mismatch", family)
		}
	}
	if !reflect.DeepEqual(cfg, before) {
		t.Fatal("settings construction mutated configuration")
	}
	ctx := WithDirectConfigSnapshot(context.Background(), cfg)
	cfg.DirectAPI["codex"] = config.RemoteAPI{AccountID: "hub", Model: "changed-model"}
	cfg.LocalAccounts[2].Key = "changed-key"
	cfg.GatewayAuthEpochs["direct-claude"] = "changed-epoch"
	fallback := func() Settings {
		s := SettingsWithGateway(cfg, 47123)
		s.LocalPort, s.LocalToken = 47123, "local-control"
		return s
	}
	frozen := settingsForContext(ctx, fallback)
	if frozen.CodexKey != want.CodexKey || frozen.ClaudeKey != want.ClaudeKey || !reflect.DeepEqual(frozen.Extensions, want.Extensions) || frozen.LocalToken != "local-control" {
		t.Fatal("direct snapshot followed current configuration instead of the admitted source")
	}
	if settingsForContext(WithConfigSnapshot(ctx, cfg), fallback).CodexRoot == want.CodexRoot {
		t.Fatal("Hub snapshot inherited the direct context marker")
	}
	if settingsForContext(WithLocalConfigSnapshot(ctx, cfg), fallback).CodexRoot == want.CodexRoot {
		t.Fatal("local snapshot inherited the direct context marker")
	}
}

func TestDirectSettingsFollowAppliedComputerOnlyWithoutAnExplicitChoice(t *testing.T) {
	cfg := directWorkerConfig(t)
	cfg.DirectAPI = nil
	if got, want := DirectSettingsWithGateway(cfg, 47123), LocalSettingsWithGateway(cfg, 47123); !reflect.DeepEqual(got, want) {
		t.Fatal("default direct settings did not follow the computer")
	}
	for _, revoked := range []bool{false, true} {
		cfg := directWorkerConfig(t)
		if revoked {
			cfg.DirectRelayEnabled = false
		} else {
			cfg.LocalAccounts = cfg.LocalAccounts[:2]
		}
		s := DirectSettingsWithGateway(cfg, 47123)
		if s.CodexKey != "" || s.ClaudeKey != "" || s.UseOriginalCodex {
			t.Fatal("invalid explicit direct choice fell back to old/native credentials")
		}
		for _, family := range []string{"deepseek-harness", "hermes"} {
			if s.Extensions[family].Key != "" {
				t.Fatal("invalid direct extension retained a credential")
			}
		}
	}
}

func TestPhoneCodexAPIChoicePreservesAppliedNativeProviderOnBothTransports(t *testing.T) {
	for _, provider := range []string{"", "openai", "original.custom-provider"} {
		for _, binding := range []bool{false, true} {
			cfg := directWorkerConfig(t)
			if binding {
				cfg.RecordAppliedAPI("codex", cfg.APIForTool(cfg.LocalAccounts[0], "codex"), provider)
			} else {
				delete(cfg.ToolAPIApplied, "codex")
			}
			cfg.RemoteStationAuthorization = &config.StationAuthorization{Identity: config.RemoteIdentity(cfg), Revision: "fixture-provider",
				Station: protocol.StationPolicy{Version: 1, APIBases: []string{"https://hub.invalid"}}}
			before := cfg.Clone()
			wantProvider := "openai"
			if binding && provider != "" {
				wantProvider = provider
			}
			for _, surface := range []struct {
				name     string
				settings func(config.Config, int) Settings
				key      func(config.Config, string) string
			}{
				{"remote", SettingsWithGateway, config.Config.RemoteGatewayCredential},
				{"direct", DirectSettingsWithGateway, config.Config.DirectGatewayCredential},
			} {
				active := cfg.Clone()
				active.DirectRelayEnabled = surface.name == "direct"
				if active.DirectRelayEnabled {
					active.ActiveRemoteMode = "direct"
				} else {
					active.ActiveRemoteMode = "hub"
				}
				s := surface.settings(active, 47123)
				if s.UseOriginalCodex || s.codexProvider() != wantProvider || s.CodexRoot != "http://127.0.0.1:47123/gateway/"+surface.name+"-codex" || s.CodexKey == "" || s.CodexKey != surface.key(active, surface.name+"-codex") {
					t.Fatalf("%s changed provider or escaped selected gateway (binding=%v provider=%q)", surface.name, binding, provider)
				}
				if withProvider(map[string]any{}, s)["modelProvider"] != wantProvider || !needsCodexProviderGuard(s) {
					t.Fatal("preserving native provider bypassed managed runtime source checks")
				}
			}
			if !reflect.DeepEqual(before, cfg) {
				t.Fatal("phone settings rewrote the original tool binding")
			}
		}
	}
}

// A stable source identity describes the upstream account, while the key sent
// to the SDK remains a short-lived gateway credential. This does not grant an
// SDK permission to resume across credential changes.
func TestExtensionSettingsKeepSourceSeparateFromGatewayLease(t *testing.T) {
	cfg := directWorkerConfig(t)
	cfg.RemoteStationAuthorization = &config.StationAuthorization{Identity: config.RemoteIdentity(cfg), Revision: "synthetic-source-check",
		Station: protocol.StationPolicy{Version: 1, APIBases: []string{"https://hub.invalid"}}}
	for _, surface := range []struct {
		name     string
		settings func(config.Config, int) Settings
		account  func(config.Config, string) (config.LocalAccount, bool)
	}{
		{"direct", DirectSettingsWithGateway, config.Config.DirectToolAccount},
		{"hub", SettingsWithGateway, config.Config.RemoteToolAccount},
		{"local", LocalSettingsWithGateway, config.Config.AuthorizedAppliedToolAccount},
	} {
		t.Run(surface.name, func(t *testing.T) {
			active := cfg.Clone()
			active.DirectRelayEnabled = surface.name == "direct"
			if active.DirectRelayEnabled {
				active.ActiveRemoteMode = "direct"
			} else {
				active.ActiveRemoteMode = "hub"
			}
			before := surface.settings(active, 47123)
			restarted := active.Clone()
			restarted.GatewayRuntimeKey = "synthetic-next-runtime"
			after := surface.settings(restarted, 47234)
			for _, family := range []string{"deepseek-harness", "hermes"} {
				a, ok := surface.account(active, family)
				first, next := before.Extensions[family], after.Extensions[family]
				if !ok || first.SourceFingerprint == "" || first.SourceFingerprint != config.AccountFingerprint(a) {
					t.Fatalf("%s source identity is not the upstream account", family)
				}
				if first.SourceFingerprint != next.SourceFingerprint || first.Key == "" || first.Key == next.Key || first.Root == next.Root {
					t.Fatalf("%s source identity and runtime lease were not separated", family)
				}
			}
		})
	}
	before := DirectSettingsWithGateway(cfg, 47123)
	cfg.LocalAccounts[2].Key = "different-upstream-credential"
	after := DirectSettingsWithGateway(cfg, 47123)
	for _, family := range []string{"deepseek-harness", "hermes"} {
		if before.Extensions[family].SourceFingerprint == after.Extensions[family].SourceFingerprint {
			t.Fatal("changed upstream retained the same source identity")
		}
	}
	cfg.LocalAccounts = cfg.LocalAccounts[:2]
	for _, settings := range DirectSettingsWithGateway(cfg, 47123).Extensions {
		if settings.SourceFingerprint != "" || settings.Key != "" {
			t.Fatal("missing direct account inherited a source identity or credential")
		}
	}
}
