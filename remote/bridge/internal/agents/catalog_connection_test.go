package agents

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func catalogGroupConfig(family, source, oldModel string) config.Config {
	a := config.LocalAccount{ID: "same-key", Kind: "api", BaseURL: "https://fixture.invalid", Key: "fake-same-key", Model: oldModel, Models: []string{oldModel}, Wire: "auto"}
	c := config.Config{LocalAccounts: []config.LocalAccount{a}, GatewayKey: "fake-loopback", Approval: "auto_all"}
	if source == "phone" {
		_ = c.SetRemoteAPI(family, a.ID, oldModel)
	} else {
		c.RecordAppliedAPI(family, c.APIForTool(a, family), "original-fixture-provider")
	}
	return c
}

func TestRefreshedAppliedAndPhoneCatalogsAdaptWorkersWithoutEditingOriginalConfig(t *testing.T) {
	for _, family := range []string{"codex", "claude"} {
		for _, source := range []string{"phone", "applied"} {
			t.Run(family+"/"+source, func(t *testing.T) {
				oldModel, newModel := "gpt-old", "claude-new"
				if family == "claude" {
					oldModel, newModel = "claude-old", "gpt-new"
				}
				c := catalogGroupConfig(family, source, oldModel)
				before := SettingsWithGateway(c, 12345)
				if source == "applied" && family == "codex" && (!before.UseOriginalCodex || !reflect.DeepEqual(codexArgs("codex.exe", before), []string{"app-server"})) {
					t.Fatal("native-compatible applied API must retain the original provider")
				}
				c.LocalAccounts[0].Models = []string{newModel}
				snapshot := c.Clone()
				after := SettingsWithGateway(c, 12345)
				if !reflect.DeepEqual(snapshot, c) {
					t.Fatal("worker conversion mutated the desktop config or applied binding")
				}
				route := family
				if source == "phone" {
					route = "remote-" + family
				}
				root := "http://127.0.0.1:12345/gateway/" + route
				if (family == "codex" && after.CodexModel != "") || (family == "claude" && after.ClaudeModel != "") {
					t.Fatal("worker inherited a retired upstream default")
				}
				if family == "codex" {
					provider := "salcara"
					if source == "applied" {
						provider = "original-fixture-provider"
					}
					if after.UseOriginalCodex || after.codexProvider() != provider || after.CodexRoot != root || after.CodexKey != c.GatewayKey || codexSig(before) == codexSig(after) {
						t.Fatalf("Codex conversion settings incorrect: %+v", after)
					}
					args := strings.Join(codexArgs("codex.exe", after), " ")
					for _, expected := range []string{"app-server", `model_provider="` + provider + `"`, `base_url="` + root + `/v1"`, `wire_api="responses"`} {
						if !strings.Contains(args, expected) {
							t.Fatalf("backend process still uses original direct provider: missing %q in %s", expected, args)
						}
					}
				} else if after.ClaudeRoot != root || after.ClaudeKey != c.GatewayKey || after.ClaudeAuthMode != "bearer" || claudeSettingsSig(before, "") == claudeSettingsSig(after, "") {
					t.Fatalf("Claude conversion settings incorrect: %+v", after)
				}
				if source == "applied" && c.ToolAPIApplied[family].Catalog {
					t.Fatal("gateway routing changed the user's disabled model-picker override")
				}
			})
		}
	}
}

func TestRefreshedCatalogRespectsExplicitWireAndKeepsCompatibleOriginalProvider(t *testing.T) {
	for _, family := range []string{"codex", "claude"} {
		model, protocol := "claude-any-name", "responses"
		if family == "claude" {
			model, protocol = "gpt-any-name", "anthropic"
		}
		a := config.LocalAccount{ID: "fixed", Kind: "api", Key: "fake-fixed", BaseURL: "https://fixture.invalid", Wire: protocol, Model: model, Models: []string{model}}
		c := config.Config{LocalAccounts: []config.LocalAccount{a}, GatewayKey: "fake-loopback"}
		c.RecordAppliedAPI(family, c.APIForTool(a, family), "fixed-provider")
		s := SettingsWithGateway(c, 12345)
		if family == "codex" && (!s.UseOriginalCodex || s.codexProvider() != "fixed-provider" || s.CodexRoot != a.BaseURL || !reflect.DeepEqual(codexArgs("codex.exe", s), []string{"app-server"})) {
			t.Fatal("model name overrode explicitly configured Responses interface")
		}
		if family == "claude" && s.ClaudeRoot != a.BaseURL {
			t.Fatal("model name overrode explicitly configured Messages interface")
		}
		if c.LocalAccounts[0].Wire != protocol {
			t.Fatal("worker guessed a new fixed interface")
		}
	}
}

func TestRefreshedAppliedCodexCatalogRestartsWorkerAndResumesVerifiedModel(t *testing.T) {
	f := newFixture(t, "auto_all")
	c := catalogGroupConfig("codex", "applied", "gpt-old")
	f.mu.Lock()
	f.s = SettingsWithGateway(c, 12345)
	f.mu.Unlock()
	a := f.m.Get("codex").(*codexAgent)
	id, err := a.Start(context.Background(), f.workDir, "first task", "gpt-old", "auto_all")
	if err != nil {
		t.Fatal(err)
	}
	f.rec.wait(t, "first complete", func(e protocol.Event) bool {
		return e.Type == "turn" && e.Status == "completed" && e.SessionKey == "codex:"+id
	})
	c.LocalAccounts[0].Models = []string{"claude-new"}
	f.mu.Lock()
	f.s = SettingsWithGateway(c, 12345)
	f.mu.Unlock()
	if err := a.SendWithOptions(context.Background(), id, "continue original", TurnOptions{Model: "claude-new"}); err != nil {
		t.Fatal(err)
	}
	f.rec.wait(t, "resumed complete", func(e protocol.Event) bool { return countTurns(f.rec) >= 2 })
	args := readLog(t, f.logDir, "codex-args.jsonl")
	if len(args) != 2 || strings.Join(toStrings(args[0]["args"]), " ") != "app-server" || !strings.Contains(strings.Join(toStrings(args[1]["args"]), " "), "/gateway/codex/v1") {
		t.Fatalf("worker was not restarted onto conversion gateway: %+v", args)
	}
	var resumed, nextTurn bool
	for _, line := range readLog(t, f.logDir, "codex-calls.jsonl") {
		params, _ := line["params"].(map[string]any)
		if line["method"] == "thread/resume" {
			resumed = params["threadId"] == id && params["model"] == "claude-new" && params["modelProvider"] == "original-fixture-provider"
		}
		if line["method"] == "turn/start" && params["model"] == "claude-new" {
			nextTurn = params["threadId"] == id
		}
	}
	if !resumed || !nextTurn {
		t.Fatalf("new group lost thread identity or resumed with the retired model: resume=%v turn=%v", resumed, nextTurn)
	}
}

func TestRefreshedAppliedClaudeCatalogRestartsWorkerAndResumesSameSession(t *testing.T) {
	f := newFixture(t, "auto_all")
	c := catalogGroupConfig("claude", "applied", "claude-old")
	f.mu.Lock()
	f.s = SettingsWithGateway(c, 12345)
	f.mu.Unlock()
	a := f.m.Get("claude").(*claudeAgent)
	id, err := a.Start(context.Background(), f.workDir, "first task", "claude-old", "auto_all")
	if err != nil {
		t.Fatal(err)
	}
	f.rec.wait(t, "first complete", func(e protocol.Event) bool {
		return e.Type == "turn" && e.Status == "completed" && e.SessionKey == "claude:"+id
	})
	c.LocalAccounts[0].Models = []string{"gpt-new"}
	f.mu.Lock()
	f.s = SettingsWithGateway(c, 12345)
	f.mu.Unlock()
	if err := a.SendWithOptions(context.Background(), id, "continue original", TurnOptions{Model: "gpt-new"}); err != nil {
		t.Fatal(err)
	}
	f.rec.wait(t, "resumed complete", func(e protocol.Event) bool { return countTurns(f.rec) >= 2 })
	args := readLog(t, f.logDir, "claude-args.jsonl")
	if len(args) != 2 {
		t.Fatalf("worker did not restart for the changed group: %+v", args)
	}
	last := args[1]
	joined := strings.Join(toStrings(last["args"]), " ")
	if !strings.Contains(joined, "--resume "+id) || !strings.Contains(joined, "--model gpt-new") || strings.Contains(joined, "--session-id") || last["base"] != "http://127.0.0.1:12345/gateway/claude" {
		t.Fatalf("group change did not retain session and gateway: %+v", last)
	}
}

func TestConvertedAppliedCodexPreservesQuotedCustomProviderAndRequestsLoopbackConfig(t *testing.T) {
	for _, provider := range []string{"original-relay", "team.provider", `a"quoted`, "供应商 API"} {
		t.Run(provider, func(t *testing.T) {
			s := Settings{CodexRoot: "http://127.0.0.1:12345/gateway/codex", CodexKey: "fake-loopback", CodexProvider: provider}
			if s.codexProvider() != provider || validateCodexWorkerOverrides("codex.exe", s) != nil {
				t.Fatal("lost the original custom-provider identity")
			}
			args := codexArgs("codex.exe", s)
			var parsed map[string]any
			if len(args) != 5 || toml.Unmarshal([]byte(args[2]+"\n"+args[4]), &parsed) != nil {
				t.Fatalf("custom provider override is not safe TOML: %v", args)
			}
			if parsed["model_provider"] != provider {
				t.Fatalf("quoted provider renamed: %v", parsed["model_provider"])
			}
			providers := parsed["model_providers"].(map[string]any)
			entry := providers[provider].(map[string]any)
			if len(providers) != 1 || entry["env_key"] != "SUB2API_API_KEY" || entry["base_url"] != s.CodexRoot+"/v1" || entry["wire_api"] != "responses" || entry["requires_openai_auth"] != false || len(entry) != 6 {
				t.Fatalf("worker override did not request the expected loopback fields: %+v", entry)
			}
			if strings.Contains(strings.Join(args, " "), s.CodexKey) {
				t.Fatal("loopback credential escaped into process arguments")
			}
			if validateCodexWorkerOverrides("unresolved.cmd", s) == nil || validateCodexWorkerOverrides("unresolved.bat", s) == nil {
				t.Fatal("unsafe custom-provider override passed through a re-parsing batch shim")
			}
		})
	}
}

func TestConvertedAppliedCodexBuiltInProviderUsesSupportedBaseOverride(t *testing.T) {
	for _, provider := range []string{"", "openai"} {
		c := catalogGroupConfig("codex", "applied", "gpt-old")
		binding := c.ToolAPIApplied["codex"]
		binding.Provider = provider
		c.ToolAPIApplied["codex"] = binding
		c.LocalAccounts[0].Models = []string{"claude-new"}
		s := SettingsWithGateway(c, 12345)
		if s.UseOriginalCodex || s.codexProvider() != "openai" {
			t.Fatal("converted built-in provider was replaced by a new provider identity")
		}
		args := strings.Join(codexArgs("codex.exe", s), " ")
		if !strings.Contains(args, `model_provider="openai"`) || !strings.Contains(args, `openai_base_url="http://127.0.0.1:12345/gateway/codex/v1"`) || strings.Contains(args, "model_providers") {
			t.Fatalf("attempted to overwrite a reserved built-in provider: %s", args)
		}
		if validateCodexWorkerOverrides("codex.cmd", s) != nil {
			t.Fatal("safe built-in override rejected")
		}
	}
}

func TestRefreshedBuiltInCodexProviderKeepsIdentityAndUsesLoopbackAuth(t *testing.T) {
	f := newFixture(t, "auto_all")
	c := catalogGroupConfig("codex", "applied", "gpt-old")
	binding := c.ToolAPIApplied["codex"]
	binding.Provider = "" // The original tool's implicit built-in OpenAI provider.
	c.ToolAPIApplied["codex"] = binding
	f.mu.Lock()
	f.s = SettingsWithGateway(c, 12345)
	f.mu.Unlock()
	a := f.m.Get("codex").(*codexAgent)
	id, err := a.Start(context.Background(), f.workDir, "first task", "gpt-old", "auto_all")
	if err != nil {
		t.Fatal(err)
	}
	f.rec.wait(t, "first complete", func(e protocol.Event) bool {
		return e.Type == "turn" && e.Status == "completed" && e.SessionKey == "codex:"+id
	})
	c.LocalAccounts[0].Models = []string{"claude-new"}
	f.mu.Lock()
	f.s = SettingsWithGateway(c, 12345)
	f.mu.Unlock()
	if err := a.SendWithOptions(context.Background(), id, "continue original", TurnOptions{Model: "claude-new"}); err != nil {
		t.Fatal(err)
	}
	f.rec.wait(t, "resumed complete", func(e protocol.Event) bool { return countTurns(f.rec) >= 2 })
	a.mu.Lock()
	procEnv := append([]string(nil), a.conn.cmd.Env...)
	a.mu.Unlock()
	openaiCredential := false
	for _, entry := range procEnv {
		if strings.HasPrefix(entry, "OPENAI_API_KEY=") {
			openaiCredential = entry == "OPENAI_API_KEY=fake-loopback"
		}
		if strings.HasPrefix(entry, "SUB2API_API_KEY=") {
			t.Fatal("built-in worker kept a competing custom-provider credential")
		}
	}
	if !openaiCredential {
		t.Fatal("built-in worker did not get the loopback credential")
	}
	args := readLog(t, f.logDir, "codex-args.jsonl")
	if len(args) != 2 {
		t.Fatal("built-in worker did not restart")
	}
	joined := strings.Join(toStrings(args[1]["args"]), " ")
	if !strings.Contains(joined, `openai_base_url="http://127.0.0.1:12345/gateway/codex/v1"`) || strings.Contains(joined, "model_providers") {
		t.Fatal("built-in worker used the original direct API or reserved-table override")
	}
	resumed := false
	for _, line := range readLog(t, f.logDir, "codex-calls.jsonl") {
		params, _ := line["params"].(map[string]any)
		if line["method"] == "thread/resume" {
			resumed = params["threadId"] == id && params["modelProvider"] == "openai" && params["model"] == "claude-new"
		}
	}
	if !resumed {
		t.Fatal("built-in conversion changed thread/provider identity or resumed a retired model")
	}
}
