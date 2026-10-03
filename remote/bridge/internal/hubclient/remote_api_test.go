package hubclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
)

func TestPhoneCanPickRemoteAPIByOpaqueHandle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer sk-remote-private" {
			t.Errorf("unexpected catalog request: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5-codex"},{"id":"gpt-5"}]}`))
	}))
	defer server.Close()
	a := config.LocalAccount{ID: "vault-remote-id", Name: "备用中转", Kind: "api", Key: "sk-remote-private", BaseURL: server.URL + "/v1", Models: []string{"gpt-5-codex", "gpt-5"}}
	cfg := config.Config{LocalAccounts: []config.LocalAccount{a}, DeviceSecret: "device-secret", DeviceID: "pc"}
	c := statusFixture(t, cfg)
	result, err := c.Dispatch(context.Background(), map[string]any{"type": "agents.status"})
	if err != nil {
		t.Fatal(err)
	}
	reply := result.(map[string]any)
	apis := reply["apis"].([]remoteAPIOption)
	if len(apis) != 1 || apis[0].Name != "备用中转" || len(apis[0].Models) != 2 || !strings.HasPrefix(apis[0].ID, "api_") {
		t.Fatalf("api options: %+v", apis)
	}
	encoded, _ := json.Marshal(reply)
	for _, private := range []string{a.ID, a.Key, server.URL} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("private vault data escaped: %s", private)
		}
	}
	if _, err := c.Dispatch(context.Background(), map[string]any{"type": "agents.api.set", "agent": "codex", "accountId": "api_bogus"}); err == nil {
		t.Fatal("unknown handle accepted")
	}
	result, err = c.Dispatch(context.Background(), map[string]any{"type": "agents.api.set", "agent": "codex", "accountId": apis[0].ID, "model": "gpt-5"})
	if err != nil {
		t.Fatal(err)
	}
	list := result.(map[string]any)["agents"].([]agentStatus)
	var codex agentStatus
	for _, s := range list {
		if s.ID == "codex" {
			codex = s
		}
	}
	if codex.API.Source != "phone" || codex.API.Name != "备用中转" || codex.API.Model != "gpt-5" || codex.API.AccountID != apis[0].ID {
		t.Fatalf("phone choice not reflected: %+v", codex.API)
	}
	stored := c.o.Store.Get()
	s := agents.SettingsFromConfig(stored)
	if s.UseOriginalCodex || s.CodexKey != a.Key || s.CodexModel != "gpt-5" || s.CodexRoot != server.URL || strings.HasSuffix(s.CodexRoot, "/v1") {
		t.Fatalf("worker settings: %+v", s)
	}
	for _, target := range []string{"codex", "codex-desktop"} {
		if stored.ToolAPISelections[target] != a.ID || stored.ToolModels[target] != "gpt-5" {
			t.Fatalf("phone choice not mirrored to the computer's %s card: %v %v", target, stored.ToolAPISelections, stored.ToolModels)
		}
	}
	if _, err := c.Dispatch(context.Background(), map[string]any{"type": "agents.api.set", "agent": "codex", "accountId": ""}); err != nil {
		t.Fatal(err)
	}
	if _, set := c.o.Store.Get().RemoteAPI["codex"]; set {
		t.Fatal("follow-computer choice not cleared")
	}
}

func TestPhoneClaudeDesktopChoiceDrivesClaudeWorkerAndDesktopCard(t *testing.T) {
	a := config.LocalAccount{ID: "vault-claude", Name: "Claude 中转", Kind: "api", Key: "sk-claude-private", BaseURL: "https://api.claude.test/v1", Models: []string{"claude-sonnet-4-5", "grok-4"}}
	c := statusFixture(t, config.Config{LocalAccounts: []config.LocalAccount{a}, DeviceSecret: "s", DeviceID: "pc"})
	handle := APIHandle(c.o.Store.Get(), a.ID)
	if _, err := c.Dispatch(context.Background(), map[string]any{"type": "agents.api.set", "agent": "claude-desktop", "accountId": handle}); err != nil {
		t.Fatal(err)
	}
	stored := c.o.Store.Get()
	if stored.RemoteAPI["claude"].AccountID != a.ID || stored.ToolAPISelections["claude-desktop"] != a.ID || stored.ToolModels["claude-desktop"] != "" {
		t.Fatalf("remote=%v selections=%v models=%v", stored.RemoteAPI, stored.ToolAPISelections, stored.ToolModels)
	}
	if _, set := stored.ToolAPISelections["claude"]; set {
		t.Fatal("a Claude Desktop choice must not rewrite the Claude Code card")
	}
	if _, set := stored.ToolModels["claude-desktop"]; set {
		t.Fatal("key selection must wait for an explicit model, not choose the first catalog entry")
	}
}
