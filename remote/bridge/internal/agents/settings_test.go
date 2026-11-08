package agents

import (
	"context"
	"os"
	"reflect"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func TestAppliedLocalAPIIsSeparateFromHubLoginAndSelection(t *testing.T) {
	cx := config.LocalAccount{ID: "cx", Kind: "codex", Key: "local-codex", BaseURL: "https://cx.test", Model: "cx-model"}
	cl := config.LocalAccount{ID: "cl", Kind: "claude", Key: "local-claude", BaseURL: "https://cl.test", Model: "cl-model", AuthMode: "api-key"}
	c := config.Config{RelayRoot: "https://hub-login.test", AccountKey: "hub-login-key", LocalAccounts: []config.LocalAccount{cx, cl}, LocalToolPaths: map[string]string{"codex": os.Args[0], "claude": os.Args[0]}}
	legacy := SettingsFromConfig(c)
	if legacy.UseOriginalCodex || legacy.CodexKey != c.AccountKey {
		t.Fatal("changed legacy remote configuration")
	}
	c.RecordAppliedAPI("codex-desktop", cx, "original")
	c.RecordAppliedAPI("claude", cl, "")
	s := SettingsFromConfig(c)
	if !s.UseOriginalCodex || s.codexProvider() != "original" || s.CodexKey != cx.Key || s.ClaudeKey != cl.Key || s.ClaudeAuthMode != "api-key" || s.codexRoot() != cx.BaseURL || s.claudeRoot() != cl.BaseURL || s.RelayRoot != c.RelayRoot {
		t.Fatal("Hub and tool APIs mixed")
	}
	if !reflect.DeepEqual(codexArgs("codex.exe", s), []string{"app-server"}) {
		t.Fatal("changed original provider or history visibility")
	}
	c.ToolAPISelections["codex"] = ""
	if SettingsFromConfig(c).CodexKey != cx.Key {
		t.Fatal("deselection overwrote applied API")
	}
	c.LocalAccounts[0].Key = "changed"
	c.LocalAccounts[1].Key = "changed-claude-key"
	s = SettingsFromConfig(c)
	if s.CodexKey != "" || s.ClaudeKey != "" || s.codexRoot() != "" {
		t.Fatal("invalid binding used edited key or fell back to Hub key")
	}
	if c.AccountKey != "hub-login-key" {
		t.Fatal("mutated Hub login")
	}
}

func TestRemoteExecutionUsesVaultSnapshotInsteadOfNativeConfig(t *testing.T) {
	a := config.LocalAccount{ID: "cx", Kind: "codex", SourceType: "api_key", Key: "vault-key", BaseURL: "https://proxy.example", Model: "gpt-model"}
	c := config.Config{LocalAccounts: []config.LocalAccount{a}, GatewayKey: "old-gateway"}
	c.RecordAppliedAPI("codex", a, "")
	c.RemoteExecutionArmed, c.GatewayRuntimeKey = true, "process-fixture"
	c.RemoteStationAuthorization = &config.StationAuthorization{Identity: config.RemoteIdentity(c), Revision: "r1", Station: protocol.StationPolicy{Version: 1, APIBases: []string{a.BaseURL}}}
	s := SettingsWithGateway(c, 47831)
	if s.UseOriginalCodex || s.CodexProvider != "openai" || s.CodexRoot != "http://127.0.0.1:47831/gateway/remote-codex" || s.CodexKey != c.RemoteGatewayCredential("remote-codex") {
		t.Fatalf("remote worker still trusted native config: %+v", s)
	}
	if got := codexArgs("codex.exe", s); len(got) == 1 {
		t.Fatal("remote worker did not create an explicit provider override")
	}
}

func TestRemoteSettingsSnapshotDoesNotRereadEditedSource(t *testing.T) {
	a := config.LocalAccount{ID: "a", Kind: "api", SourceType: "api_key", BaseURL: "https://a.test", Key: "key-a"}
	c := config.Config{LocalAccounts: []config.LocalAccount{a}, RemoteAPI: map[string]config.RemoteAPI{"codex": {AccountID: a.ID}, "claude": {AccountID: a.ID}}, RemoteExecutionArmed: true, GatewayRuntimeKey: "runtime"}
	c.RemoteStationAuthorization = &config.StationAuthorization{Identity: config.RemoteIdentity(c), Revision: "r1", Station: protocol.StationPolicy{Version: 1, APIBases: []string{a.BaseURL}}}
	ctx := WithConfigSnapshot(context.Background(), c)
	want := SettingsWithGateway(c, 12345)
	c.LocalAccounts[0].Key = "changed-key"
	fallback := func() Settings {
		s := SettingsWithGateway(c, 12345)
		s.LocalPort = 12345
		s.LocalToken = "approval-fixture"
		return s
	}
	for i := 0; i < 3; i++ {
		s := settingsForContext(ctx, fallback)
		if s.CodexKey != want.CodexKey || s.ClaudeKey != want.ClaudeKey || s.LocalToken != "approval-fixture" {
			t.Fatal("authorized snapshot was replaced by current key")
		}
	}
	if got := settingsForContext(context.Background(), fallback); got.CodexKey == want.CodexKey {
		t.Fatal("local callers failed to use current settings")
	}
}

func TestRuntimeCLIPathsFollowWorkbench(t *testing.T) {
	s := Settings{CodexPath: os.Args[0], ClaudePath: os.Args[0]}
	m := NewManagerWithOptions(nil, func() Settings { return s }, Options{StateDir: t.TempDir(), ClaudeHome: t.TempDir()})
	defer m.Close()
	mm := m.(*manager)
	if p, ok := mm.codex.exe(); !ok || p != os.Args[0] {
		t.Fatal("Codex ignored workbench path")
	}
	if p, ok := mm.claude.exe(); !ok || p != os.Args[0] {
		t.Fatal("Claude ignored workbench path")
	}
	s.CodexPath, s.ClaudePath = "missing-custom-tool", "missing-custom-tool"
	if _, ok := mm.codex.exe(); ok {
		t.Fatal("invalid custom Codex path fell back silently")
	}
	if _, ok := mm.claude.exe(); ok {
		t.Fatal("invalid custom Claude path fell back silently")
	}
}

func TestArmedLocalWorkerDoesNotRequireStationAuthorization(t *testing.T) {
	a := config.LocalAccount{ID: "local", Kind: "api", SourceType: "api_key", BaseURL: "https://local.fixture", Key: "synthetic-local", Model: "gpt-fixture"}
	cfg := config.Config{LocalAccounts: []config.LocalAccount{a}, RemoteExecutionArmed: true, GatewayRuntimeKey: "synthetic-runtime"}
	cfg.RecordAppliedAPI("codex", a, "")
	cfg.RecordAppliedAPI("claude", a, "")
	s := LocalSettingsWithGateway(cfg, 47831)
	aCodex := &codexAgent{settings: func() Settings { return s }}
	if _, err := aCodex.requireRelay(context.Background()); err != nil {
		t.Fatalf("explicitly applied local API required a phone station: %v", err)
	}
	if s.CodexKey != cfg.LocalGatewayCredential("codex") || s.ClaudeKey != cfg.LocalGatewayCredential("claude") || s.UseOriginalCodex {
		t.Fatal("local worker did not use its explicit local gateway lease")
	}
}

func TestLocalWorkerSnapshotStartsWithoutStationAndRejectsRemoteReuse(t *testing.T) {
	for _, family := range []string{"codex", "claude"} {
		t.Run(family, func(t *testing.T) {
			f := newFixture(t, "ask")
			local := config.LocalAccount{ID: "local", Kind: "api", SourceType: "api_key", BaseURL: "https://local.fixture", Key: "synthetic-local"}
			phone := config.LocalAccount{ID: "phone", Kind: "api", SourceType: "api_key", BaseURL: "https://phone.fixture", Key: "synthetic-phone"}
			cfg := config.Config{LocalAccounts: []config.LocalAccount{local, phone}, RemoteAPI: map[string]config.RemoteAPI{family: {AccountID: phone.ID}}, RemoteExecutionArmed: true, GatewayRuntimeKey: "synthetic-process", Approval: "ask"}
			cfg.RecordAppliedAPI(family, local, "fixture-provider")
			f.mu.Lock()
			base := f.s
			f.s = LocalSettingsWithGateway(cfg, base.LocalPort)
			f.s.BridgeExe, f.s.LocalPort, f.s.LocalToken = base.BridgeExe, base.LocalPort, base.LocalToken
			f.mu.Unlock()
			localCtx := WithLocalConfigSnapshot(context.Background(), cfg)
			adapter := f.m.Get(family)
			id, err := adapter.Start(localCtx, f.workDir, "hello fixture", "", "ask")
			if err != nil || id == "" {
				t.Fatalf("explicit local start failed without station lease: %v", err)
			}
			key := family + ":" + id
			pending := f.rec.wait(t, "local worker approval", func(e protocol.Event) bool {
				return e.Type == "approval.request" && e.Kind == "command" && e.SessionKey == key
			})
			cfg.RemoteStationAuthorization = &config.StationAuthorization{Identity: config.RemoteIdentity(cfg), Revision: "current", Station: protocol.StationPolicy{Version: 1, APIBases: []string{phone.BaseURL}}}
			remoteCtx := WithConfigSnapshot(localCtx, cfg)
			if err := adapter.Send(remoteCtx, id, "must not reach local worker"); err == nil {
				t.Fatal("phone reused a busy worker with a local gateway lease")
			}
			if RespondForSession(remoteCtx, adapter, key, pending.ApprovalID, ApprovalResponse{Decision: "allow"}) {
				t.Fatal("phone approved the local worker using a different source")
			}
			if !RespondForSession(localCtx, adapter, key, pending.ApprovalID, ApprovalResponse{Decision: "allow"}) {
				t.Fatal("local authorized approval no longer works")
			}
			args := readLog(t, f.logDir, family+"-args.jsonl")
			if len(args) != 1 {
				t.Fatal("source mismatch launched another worker")
			}
			field := "key"
			if family == "claude" {
				field = "token"
			}
			if args[0][field] != cfg.LocalGatewayCredential(family) {
				t.Fatal("worker did not receive the applied local gateway lease")
			}
		})
	}
}
