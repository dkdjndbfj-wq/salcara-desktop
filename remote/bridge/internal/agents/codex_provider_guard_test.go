package agents

import (
	"context"
	"encoding/json"
	"testing"

	"salcara/bridge/internal/protocol"
)

func guardedProviderConfig(s Settings) (map[string]any, map[string]any) {
	entry := map[string]any{
		"base_url": s.codexRoot() + "/v1", "env_key": "SUB2API_API_KEY", "wire_api": "responses",
		"requires_openai_auth": false, "supports_websockets": false,
	}
	cfg := map[string]any{"model_provider": s.codexProvider(), "model_providers": map[string]any{s.codexProvider(): entry}}
	return cfg, entry
}

func TestCodexProviderGuardValidatesEffectiveConfiguration(t *testing.T) {
	s := Settings{CodexRoot: "http://127.0.0.1:12345/gateway/codex", CodexProvider: "original.provider", CodexKey: "fake-loopback"}
	for _, tc := range []struct {
		field string
		value any
	}{
		{"base_url", "https://fixture.invalid/old"}, {"env_key", "SYNTHETIC_OLD_KEY"}, {"wire_api", "anthropic"},
		{"requires_openai_auth", true}, {"requires_openai_auth", nil}, {"supports_websockets", true}, {"supports_websockets", nil},
		{"http_headers", map[string]any{"Authorization": "synthetic-marker"}},
		{"env_http_headers", map[string]any{"Authorization": "SYNTHETIC_ENV_MARKER"}},
		{"query_params", map[string]any{"api-key": "synthetic-marker"}},
		{"experimental_bearer_token", "synthetic-marker"}, {"auth", map[string]any{"command": "synthetic-never-executed"}},
		{"aws", map[string]any{"region": "synthetic-region"}}, {"http_headers", "unknown-shape"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			cfg, entry := guardedProviderConfig(s)
			entry[tc.field] = tc.value
			if codexEffectiveProviderSafe(map[string]any{"config": cfg}, s) {
				t.Fatal("unsafe or unverifiable effective provider passed")
			}
		})
	}
	for _, field := range []string{"http_headers", "env_http_headers", "query_params", "auth", "aws"} {
		cfg, entry := guardedProviderConfig(s)
		entry[field] = map[string]any{}
		if !codexEffectiveProviderSafe(map[string]any{"config": cfg}, s) {
			t.Fatal("empty optional table rejected")
		}
	}
	cfg, _ := guardedProviderConfig(s)
	cfg["model_provider"] = "wrong-provider"
	if codexEffectiveProviderSafe(map[string]any{"config": cfg}, s) || codexEffectiveProviderSafe(map[string]any{}, s) {
		t.Fatal("wrong identity or missing effective configuration passed")
	}
}

func TestCodexCustomProviderGuardRejectsBeforeAnyThreadMutation(t *testing.T) {
	for _, provider := range []string{"", "original.provider"} {
		for _, field := range []string{"http_headers", "env_http_headers", "query_params", "experimental_bearer_token", "auth", "aws", "read-error", "missing-provider"} {
			for _, operation := range []string{"start", "resume"} {
				t.Run(provider+"/"+field+"/"+operation, func(t *testing.T) {
					f := newFixture(t, "auto_all")
					s := Settings{CodexRoot: "http://127.0.0.1:12345/gateway/codex", CodexProvider: provider, CodexKey: "fake-loopback", Approval: "auto_all"}
					f.mu.Lock()
					f.s = s
					f.mu.Unlock()
					cfg, entry := guardedProviderConfig(s)
					entry[field] = map[string]any{"synthetic-private-marker": "never-real"}
					if field == "experimental_bearer_token" {
						entry[field] = "synthetic-private-marker"
					}
					if field == "missing-provider" {
						delete(cfg, "model_providers")
					}
					b, _ := json.Marshal(cfg)
					fixture := string(b)
					if field == "read-error" {
						fixture = "error"
					}
					t.Setenv("SALCARA_FAKE_CODEX_CONFIG", fixture)
					a := f.m.Get("codex").(*codexAgent)
					var err error
					if operation == "start" {
						_, err = a.Start(context.Background(), f.workDir, "must not run", "gpt-new", "auto_all")
					} else {
						err = a.SendWithOptions(context.Background(), "fixture-original-uuid", "must not run", TurnOptions{Model: "gpt-new"})
					}
					if err == nil || err.Error() != errCodexRemoteProvider {
						t.Fatalf("unsafe provider was accepted or exposed its details: %v", err)
					}
					calls := readLog(t, f.logDir, "codex-calls.jsonl")
					if len(calls) != 1 || calls[0]["method"] != "config/read" {
						t.Fatalf("refused worker reached thread mutation: %+v", calls)
					}
					a.mu.Lock()
					retained := a.conn != nil
					a.mu.Unlock()
					if retained {
						t.Fatal("refused process was published as a running connection")
					}
				})
			}
		}
	}
}

func TestCodexCustomProviderGuardCleanConfigurationContinuesOriginalThread(t *testing.T) {
	for _, provider := range []string{"", "original.provider"} {
		t.Run(provider, func(t *testing.T) {
			f := newFixture(t, "auto_all")
			f.mu.Lock()
			f.s = Settings{CodexRoot: "http://127.0.0.1:12345/gateway/codex", CodexProvider: provider, CodexKey: "fake-loopback", Approval: "auto_all"}
			f.mu.Unlock()
			a := f.m.Get("codex").(*codexAgent)
			if err := a.SendWithOptions(context.Background(), "fixture-original-uuid", "continue", TurnOptions{Model: "gpt-new"}); err != nil {
				t.Fatal(err)
			}
			f.rec.wait(t, "clean continued turn", func(e protocol.Event) bool { return e.Type == "turn" && e.Status == "completed" })
			calls := readLog(t, f.logDir, "codex-calls.jsonl")
			if len(calls) != 3 || calls[0]["method"] != "config/read" || calls[1]["method"] != "thread/resume" || calls[2]["method"] != "turn/start" {
				t.Fatalf("clean config did not resume after guard: %+v", calls)
			}
			params, _ := calls[1]["params"].(map[string]any)
			if params["threadId"] != "fixture-original-uuid" || params["modelProvider"] != f.s.codexProvider() {
				t.Fatal("guard changed thread/provider identity")
			}
		})
	}
}

func TestCodexProviderGuardSkipsOriginalLoginAndBuiltInWorkers(t *testing.T) {
	for _, s := range []Settings{{}, {UseOriginalCodex: true, CodexRoot: "https://fixture.invalid", CodexProvider: "original"}, {CodexRoot: "http://127.0.0.1:12345/gateway/codex", CodexProvider: "openai"}} {
		if needsCodexProviderGuard(s) || validateCodexEffectiveProvider(context.Background(), nil, s) != nil {
			t.Fatal("guard affected own-login/native-compatible/built-in path")
		}
	}
}
