package agents

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func builtinGuardConfig(s Settings) map[string]any {
	return map[string]any{"model_provider": "openai", "openai_base_url": s.codexRoot() + "/v1",
		"cli_auth_credentials_store": "ephemeral", "forced_login_method": "api"}
}

func TestCodexBuiltinProviderMustUseIsolatedEffectiveAuthentication(t *testing.T) {
	s := Settings{CodexRoot: "http://127.0.0.1:12345/gateway/codex", CodexProvider: "openai", CodexKey: "fake-loopback"}
	if !needsCodexProviderGuard(s) {
		t.Fatal("builtin managed worker bypassed provider guard")
	}
	if !codexEffectiveProviderSafe(map[string]any{"config": builtinGuardConfig(s)}, s) {
		t.Fatal("clean isolated builtin config rejected")
	}
	for _, tc := range []struct {
		field string
		value any
	}{
		{"openai_base_url", "https://fixture.invalid/retired/v1"}, {"model_provider", "retired"},
		{"cli_auth_credentials_store", "file"}, {"cli_auth_credentials_store", nil},
		{"forced_login_method", "chatgpt"}, {"forced_login_method", nil},
		{"model_providers", map[string]any{"openai": map[string]any{"http_headers": map[string]any{"Authorization": "synthetic"}}}},
		{"model_providers", map[string]any{"openai": "unverifiable"}},
	} {
		t.Run(tc.field, func(t *testing.T) {
			cfg := builtinGuardConfig(s)
			cfg[tc.field] = tc.value
			if codexEffectiveProviderSafe(map[string]any{"config": cfg}, s) {
				t.Fatal("unsafe builtin configuration accepted")
			}
		})
	}
}

func TestCodexBuiltinAuthenticationFailureCannotReachThread(t *testing.T) {
	for _, mode := range []string{"login-error", "wrong-kind", "no-account", "read-error"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, "auto_all")
			f.mu.Lock()
			f.s = Settings{CodexRoot: "http://127.0.0.1:12345/gateway/codex", CodexProvider: "openai", CodexKey: "fake-loopback", Approval: "auto_all"}
			f.mu.Unlock()
			t.Setenv("SALCARA_FAKE_CODEX_AUTH", mode)
			a := f.m.Get("codex").(*codexAgent)
			if _, err := a.Start(context.Background(), f.workDir, "must not execute", "gpt-new", "auto_all"); err == nil || err.Error() != errCodexRemoteProvider {
				t.Fatal("unverified builtin credential reached a thread")
			}
			for _, row := range readLog(t, f.logDir, "codex-calls.jsonl") {
				if row["method"] == "thread/start" || row["method"] == "turn/start" {
					t.Fatal("authentication refusal happened after thread execution")
				}
			}
			a.mu.Lock()
			retained := a.conn != nil
			a.mu.Unlock()
			if retained {
				t.Fatal("refused authentication was published")
			}
		})
	}
}

func TestCodexBuiltinIsolatedLoginPrecedesThread(t *testing.T) {
	f := newFixture(t, "auto_all")
	for _, key := range []string{"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "OPENAI_FEDERATION_RULE_ID", "OPENAI_IDENTITY_TOKEN_FILE", "OPENAI_WORKLOAD_IDENTITY_CONTEXT"} {
		t.Setenv(key, "synthetic-inherited-not-selected")
	}
	f.mu.Lock()
	f.s = Settings{CodexRoot: "http://127.0.0.1:12345/gateway/codex", CodexProvider: "openai", CodexKey: "fake-loopback", Approval: "auto_all"}
	f.mu.Unlock()
	_, err := f.m.Get("codex").(*codexAgent).Start(context.Background(), f.workDir, "continue", "gpt-new", "auto_all")
	if err != nil {
		t.Fatal(err)
	}
	rows := readLog(t, f.logDir, "codex-calls.jsonl")
	if len(rows) < 5 || rows[0]["method"] != "config/read" || rows[1]["method"] != "account/login/start" || rows[2]["method"] != "account/read" || rows[3]["method"] != "config/read" || rows[4]["method"] != "thread/start" {
		t.Fatal("builtin authentication was not initialized before thread creation")
	}
	if rows[1]["selectedKey"] != true {
		t.Fatal("worker did not receive the exact selected fixture key")
	}
	encoded, _ := json.Marshal(rows)
	if strings.Contains(string(encoded), "fake-loopback") || strings.Contains(string(encoded), "synthetic-inherited-not-selected") {
		t.Fatal("authentication observation exposed a fixture credential")
	}
	a := f.m.Get("codex").(*codexAgent)
	a.mu.Lock()
	child := append([]string(nil), a.conn.cmd.Env...)
	a.mu.Unlock()
	for _, entry := range child {
		if strings.Contains(entry, "synthetic-inherited-not-selected") {
			t.Fatal("managed builtin worker inherited another authentication source")
		}
	}
}
