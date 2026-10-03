package hubclient

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/launcher"
)

func statusFixture(t *testing.T, cfg config.Config) *Client {
	t.Helper()
	store, err := config.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *config.Config) error { *c = cfg; return nil }); err != nil {
		t.Fatal(err)
	}
	return New(Options{
		Store: store,
		DiscoverTools: func(_ context.Context, _ map[string]string) []launcher.Tool {
			return []launcher.Tool{
				{ID: "codex-desktop", Name: "must-not-leak", Path: "must-not-leak", Available: true},
				{ID: "claude-desktop", Available: true},
				{ID: "codex", Available: true},
				{ID: "claude", Available: false},
				{ID: "malicious-tool", Available: true},
			}
		},
	})
}

func dispatchStatus(t *testing.T, c *Client) []agentStatus {
	t.Helper()
	result, err := c.Dispatch(context.Background(), map[string]any{"type": "agents.status", "key": "must-not-leak", "path": "must-not-leak"})
	if err != nil {
		t.Fatal(err)
	}
	reply, ok := result.(map[string]any)
	if !ok || len(reply) != 3 {
		t.Fatalf("unexpected response envelope: %#v", result)
	}
	list, ok := reply["agents"].([]agentStatus)
	if !ok || len(list) != 4 {
		t.Fatalf("unexpected agents: %#v", reply["agents"])
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "must-not-leak") || strings.Contains(string(encoded), "malicious-tool") {
		t.Fatal("inventory/request metadata escaped the whitelist")
	}
	return list
}

func TestAgentsStatusWithoutStoreOrManager(t *testing.T) {
	c := &Client{o: Options{DiscoverTools: func(context.Context, map[string]string) []launcher.Tool { return nil }}}
	list := dispatchStatus(t, c)
	for _, s := range list {
		if s.Available || s.RemoteSendSupported || s.API != (agentAPIStatus{Source: "tool"}) {
			t.Fatalf("missing configuration invented capability: %+v", s)
		}
	}
}

func TestAgentsStatusDoesNotWaitForManagerOrMutateConfig(t *testing.T) {
	c := statusFixture(t, config.Config{LocalToolPaths: map[string]string{"codex": "fixture-path"}})
	before, err := os.ReadFile(c.o.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := c.o.Store.Get()
	discoveryCount := 0
	c.o.DiscoverTools = func(_ context.Context, paths map[string]string) []launcher.Tool {
		discoveryCount++
		if paths["codex"] != "fixture-path" {
			t.Fatal("configured executable paths not used")
		}
		paths["codex"] = "mutated-copy"
		return nil
	}
	c.mgrMu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := c.Dispatch(context.Background(), map[string]any{"type": "agents.status"})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		c.mgrMu.Unlock()
		t.Fatal("confirmation waited for CLI manager startup")
	}
	c.mgrMu.Unlock()
	after, err := os.ReadFile(c.o.Store.Path())
	if err != nil || string(before) != string(after) || !reflect.DeepEqual(snapshot, c.o.Store.Get()) || discoveryCount != 1 {
		t.Fatal("read-only metadata changed configuration or repeated discovery")
	}
}

func TestAgentsStatusInstallationNeverEnablesDesktopControl(t *testing.T) {
	list := dispatchStatus(t, statusFixture(t, config.Config{}))
	for _, s := range list {
		if s.ID == "claude-desktop" {
			// Claude Desktop's Code sessions continue through the Claude Code CLI.
			continue
		}
		if s.ControlSurface == "desktop" && (!s.Available || s.RemoteSendSupported) {
			t.Fatalf("desktop capability conflated with installation: %+v", s)
		}
		if s.ControlSurface == "cli" && s.RemoteSendSupported != s.Available {
			t.Fatalf("CLI capability conflated with desktop: %+v", s)
		}
	}
}

func TestAgentsStatusUsesAppliedAPIWhileSelectionIsPending(t *testing.T) {
	a := config.LocalAccount{ID: "current-api", Name: "工作 API", Kind: "api", Key: "current-upstream-fixture-secret", BaseURL: "https://current.example", Model: "old-model"}
	b := config.LocalAccount{ID: "next-api", Name: "备用 API", Kind: "api", Key: "next-upstream-fixture-secret", BaseURL: "https://next.example", Model: "new-model"}
	cfg := config.Config{LocalAccounts: []config.LocalAccount{a, b}, ToolAPISelections: map[string]string{"codex-desktop": a.ID}, ToolModels: map[string]string{"codex-desktop": "old-model"}, ToolProtocols: map[string]string{"codex-desktop": "chat"}}
	applied, _ := cfg.SelectedToolAPI("codex-desktop")
	cfg.RecordAppliedAPI("codex-desktop", applied, "fixture-provider")
	cfg.ToolAPISelections["codex-desktop"] = b.ID
	cfg.ToolModels["codex-desktop"] = "new-model"
	cfg.ToolProtocols["codex-desktop"] = "responses"
	api := dispatchStatus(t, statusFixture(t, cfg))[0].API
	if api.Name != a.Name || api.Model != "old-model" || api.Protocol != "chat" || !api.Configured || !api.Pending {
		t.Fatalf("selected draft presented as applied: %+v", api)
	}
	next, _ := cfg.SelectedToolAPI("codex-desktop")
	cfg.RecordAppliedAPI("codex-desktop", next, "next-fixture-provider")
	api = dispatchStatus(t, statusFixture(t, cfg))[0].API
	if api.Name != b.Name || api.Model != "new-model" || api.Protocol != "responses" || !api.Configured || api.Pending {
		t.Fatalf("new applied snapshot did not replace confirmation: %+v", api)
	}
}

func TestAgentsStatusSelectedOnlyIsNotConfigured(t *testing.T) {
	a := config.LocalAccount{ID: "draft-api", Name: "待启用 API", Kind: "api", Key: "fixture-secret", BaseURL: "https://draft.example", Model: "draft-model"}
	cfg := config.Config{LocalAccounts: []config.LocalAccount{a}, ToolAPISelections: map[string]string{"claude": a.ID}}
	api := dispatchStatus(t, statusFixture(t, cfg))[3].API
	if api.Name != a.Name || api.Model != a.Model || api.Configured || !api.Pending {
		t.Fatalf("unapplied selection certified as configured: %+v", api)
	}
}

func TestAgentsStatusInvalidAppliedSnapshotIsNotConfigured(t *testing.T) {
	for _, mode := range []string{"key-edited", "empty-key", "invalid-url", "invalid-protocol"} {
		t.Run(mode, func(t *testing.T) {
			a := config.LocalAccount{ID: "fixture-api", Name: "工作 API", Kind: "api", Key: "fixture-secret", BaseURL: "https://draft.example", Model: "valid-model"}
			cfg := config.Config{LocalAccounts: []config.LocalAccount{a}, ToolAPISelections: map[string]string{"codex": a.ID}}
			if mode == "empty-key" {
				cfg.LocalAccounts[0].Key = ""
			}
			if mode == "invalid-url" {
				cfg.LocalAccounts[0].BaseURL = "not a valid address"
			}
			if mode == "invalid-protocol" {
				cfg.ToolProtocols = map[string]string{"codex": "http://unsafe.invalid"}
			}
			applied, _ := cfg.SelectedToolAPI("codex")
			cfg.RecordAppliedAPI("codex", applied, "fixture-provider")
			if mode == "key-edited" {
				cfg.LocalAccounts[0].Key = "edited-fixture-secret"
			}
			api := dispatchStatus(t, statusFixture(t, cfg))[2].API
			if api.Configured || !api.Pending || strings.Contains(api.Protocol, "http") {
				t.Fatalf("invalid snapshot confirmed: %+v", api)
			}
		})
	}
}

func TestAgentsStatusLabelsCannotExposeCredentialsURLsIDsOrPaths(t *testing.T) {
	secrets := []string{"sk-fixture-private-key", "hub-fixture-secret", "device-fixture-secret", "phone-fixture-secret"}
	for _, raw := range []string{
		"prefix sk-fixture-private-key suffix", "prefix hub-fixture-secret suffix", "device-fixture-secret", "phone-fixture-secret",
		"https://api.example.test/v1", "api.example.test", "127.0.0.1:1234", `C:\private\codex.exe`, "/private/config",
		"vault-fixture-id", "prefix vault-fixture-id suffix", "fixture-fingerprint", "fixture-provider", "row\nprivate", "www.private.test", "Bearer copied-token",
	} {
		t.Run(raw, func(t *testing.T) {
			a := config.LocalAccount{ID: "vault-fixture-id", Name: raw, Kind: "api", Key: secrets[0], BaseURL: "https://api.example.test", Model: raw}
			cfg := config.Config{LocalAccounts: []config.LocalAccount{a}, AccountKey: secrets[1], DeviceSecret: secrets[2], RemoteConnections: []config.RemoteConnection{{DeviceSecret: secrets[3]}}, ToolAPIApplied: map[string]config.AppliedAPI{"codex": {Fingerprint: "fixture-fingerprint", Provider: "fixture-provider"}}, ToolAPISelections: map[string]string{"codex": a.ID}}
			api := dispatchStatus(t, statusFixture(t, cfg))[2].API
			if api.Name != "已命名 API" || api.Model != "" {
				t.Fatalf("private label forwarded: %+v", api)
			}
			encoded, _ := json.Marshal(api)
			for _, private := range append(secrets, raw) {
				if strings.Contains(string(encoded), private) {
					t.Fatal("credential escaped in metadata")
				}
			}
		})
	}
}

func TestAgentsStatusAppliedMetadataContainsOnlyTheFiniteContract(t *testing.T) {
	a := config.LocalAccount{ID: "named-fixture-id", Name: "Salcara 主用", Kind: "api", Key: "fixture-private-upstream-key", BaseURL: "https://api.example.test", Model: "gpt-4.1-mini", Workspace: t.TempDir()}
	cfg := config.Config{LocalAccounts: []config.LocalAccount{a}, ToolAPISelections: map[string]string{"codex": a.ID}, LocalToolPaths: map[string]string{"codex": "fixture-private-path"}}
	applied, _ := cfg.SelectedToolAPI("codex")
	cfg.RecordAppliedAPI("codex", applied, "named-fixture-provider")
	list := dispatchStatus(t, statusFixture(t, cfg))
	api := list[2].API
	if !strings.HasPrefix(api.AccountID, "api_") {
		t.Fatalf("missing opaque API handle: %+v", api)
	}
	api.AccountID = ""
	if api != (agentAPIStatus{Name: a.Name, Model: a.Model, Protocol: "responses", Configured: true, Source: "computer"}) {
		t.Fatalf("incorrect applied metadata: %+v", api)
	}
	encoded, _ := json.Marshal(list)
	for _, private := range []string{a.ID, a.Key, a.BaseURL, a.Workspace, cfg.LocalToolPaths["codex"], cfg.ToolAPIApplied["codex"].Fingerprint, cfg.ToolAPIApplied["codex"].Provider} {
		if strings.Contains(string(encoded), private) {
			t.Fatal("full vault or launcher object was serialized")
		}
	}
	var decoded []map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, obj := range decoded {
		expectedFields := 8
		if string(obj["id"]) == `"claude-desktop"` {
			expectedFields++
			if string(obj["sessionScope"]) != `"code"` {
				t.Fatal("Claude Desktop scope must be explicit Code only")
			}
		}
		if len(obj) != expectedFields {
			t.Fatalf("extra contract fields: %v", obj)
		}
		var supportsConversationSwitch bool
		if err := json.Unmarshal(obj["conversationApiSwitch"], &supportsConversationSwitch); err != nil {
			t.Fatalf("invalid conversation capability: %v", err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(obj["api"], &fields); err != nil || len(fields) < 6 || len(fields) > 7 {
			t.Fatalf("extra API contract fields: %v, %v", fields, err)
		}
	}
}
