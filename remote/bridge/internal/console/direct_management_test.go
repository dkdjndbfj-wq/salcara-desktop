package console

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/hubclient"
)

func directManagementFixture(t *testing.T) (*Server, context.Context) {
	t.Helper()
	s, _ := newTestServer(t)
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.DirectRelayEnabled, c.DirectPhoneID, c.DirectPhoneHash = true, "phone-management", strings.Repeat("a", 64)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return s, hubclient.WithDirectExecutionValidator(context.Background(), "direct-owner", func() error { return nil })
}

func directCreateFixture() map[string]any {
	return map[string]any{"type": "computer.api.create", "controlSurface": "cli", "requestId": "create-request-0001",
		"name": "手机新增测试 API", "baseUrl": "https://api.example.invalid/v1", "key": "synthetic-private-key",
		"wire": "chat", "authMode": "bearer", "models": []string{"fixture-model", "fixture-model"}}
}

func TestDirectManagementRequiresValidatedDirectBinding(t *testing.T) {
	s, valid := directManagementFixture(t)
	for _, ctx := range []context.Context{context.Background(), hubclient.WithLocalExecution(context.Background()), hubclient.WithDirectExecution(context.Background()),
		hubclient.WithDirectExecutionValidator(context.Background(), "revoked", func() error { return errors.New("revoked") })} {
		if _, err := s.dispatchDirectManagement(ctx, directCreateFixture()); err == nil {
			t.Fatal("unvalidated management accepted")
		}
	}
	if len(s.d.Store.Get().LocalAccounts) != 0 {
		t.Fatal("unauthorized request wrote account")
	}
	if _, err := s.dispatchDirectManagement(valid, directCreateFixture()); err != nil {
		t.Fatal(err)
	}
}

func TestDirectManagementCreateIsPrivateAndIdempotent(t *testing.T) {
	s, ctx := directManagementFixture(t)
	cmd := directCreateFixture()
	first, err := s.dispatchDirectManagement(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := cmd["key"]; present {
		t.Fatal("credential payload was retained")
	}
	second, err := s.dispatchDirectManagement(ctx, directCreateFixture())
	if err != nil {
		t.Fatal(err)
	}
	a, b := first.(map[string]any), second.(map[string]any)
	if a["accountId"] != b["accountId"] || !strings.HasPrefix(a["accountId"].(string), "api_") {
		t.Fatal("unstable public account handle")
	}
	cfg := s.d.Store.Get()
	if len(cfg.LocalAccounts) != 1 {
		t.Fatal("retry duplicated account")
	}
	account := cfg.LocalAccounts[0]
	if !account.RemoteHubBindingsSet || len(account.RemoteHubURLs) != 0 {
		t.Fatal("created credential acquired Hub authorization")
	}
	if len(account.Models) != 1 || !account.ModelsManual || len(cfg.RemoteAPI) != 0 || len(cfg.ToolAPISelections) != 0 {
		t.Fatal("create changed tool bindings or lost manual models")
	}
	data, _ := json.Marshal(first)
	if strings.Contains(string(data), "synthetic-private-key") || strings.Contains(string(data), account.ID) || strings.Contains(string(data), account.BaseURL) {
		t.Fatal("create response exposed credential metadata")
	}
	conflict := directCreateFixture()
	conflict["key"] = "different-key"
	if _, err := s.dispatchDirectManagement(ctx, conflict); err == nil {
		t.Fatal("reused request changed credential")
	}
	if s.d.Store.Get().LocalAccounts[0].Key != account.Key {
		t.Fatal("retry overwrote key")
	}
}

func TestDirectManagementRechecksRevokeBeforeWrite(t *testing.T) {
	s, _ := directManagementFixture(t)
	calls := 0
	ctx := hubclient.WithDirectExecutionValidator(context.Background(), "direct-owner", func() error {
		calls++
		if calls >= 2 {
			return errors.New("revoked while queued")
		}
		return nil
	})
	if _, err := s.dispatchDirectManagement(ctx, directCreateFixture()); err == nil {
		t.Fatal("revoked write accepted")
	}
	if len(s.d.Store.Get().LocalAccounts) != 0 {
		t.Fatal("revoked operation wrote account")
	}
}

func TestDirectManagementRejectsExtraFieldsAndMalformedValues(t *testing.T) {
	s, ctx := directManagementFixture(t)
	for _, field := range []string{"cwd", "headers", "source", "credentialRef", "sessionKey", "Key", "command", "url"} {
		cmd := directCreateFixture()
		cmd[field] = "not-allowed"
		if _, err := s.dispatchDirectManagement(ctx, cmd); err == nil {
			t.Fatalf("accepted %s", field)
		}
	}
	for _, change := range []map[string]any{{"controlSurface": "desktop"}, {"key": "bad\nkey"}, {"baseUrl": "not a URL"}, {"wire": "shell"}, {"requestId": "short"}, {"models": []string{strings.Repeat("x", 201)}}} {
		cmd := directCreateFixture()
		for k, v := range change {
			cmd[k] = v
		}
		if _, err := s.dispatchDirectManagement(ctx, cmd); err == nil {
			t.Fatalf("accepted malformed request: %v", change)
		}
	}
	if len(s.d.Store.Get().LocalAccounts) != 0 {
		t.Fatal("invalid request persisted")
	}
}

func TestDirectManagementInstallNeverAcceptsCustomCommands(t *testing.T) {
	s, ctx := directManagementFixture(t)
	for _, cmd := range []map[string]any{
		{"type": "computer.agent.install", "controlSurface": "cli", "tool": "codex", "confirmed": false},
		{"type": "computer.agent.install", "controlSurface": "cli", "tool": "powershell arbitrary", "confirmed": true},
		{"type": "computer.agent.install", "controlSurface": "cli", "tool": "codex-desktop", "confirmed": true},
	} {
		if _, err := s.dispatchDirectManagement(ctx, cmd); err == nil {
			t.Fatal("unsafe install accepted")
		}
	}
	if _, _, err := s.startSetupInstall("codex", "owner", "install-request-0001", func() error { return errors.New("revoked") }); err == nil {
		t.Fatal("revoked install accepted")
	}
}

func TestDirectManagementInstallReceiptIsSanitizedAndNotReplayed(t *testing.T) {
	s, ctx := directManagementFixture(t)
	setupState.mu.Lock()
	previous := setupState.job
	setupState.job = &setupJob{ID: "job-id", Owner: "direct-owner", RequestID: "install-request-0001", Tool: "codex", State: "failed", Error: "private local path and environment", Log: "secret-log"}
	setupState.mu.Unlock()
	t.Cleanup(func() { setupState.mu.Lock(); setupState.job = previous; setupState.mu.Unlock() })
	result, err := s.dispatchDirectManagement(ctx, map[string]any{"type": "computer.agent.install", "controlSurface": "cli", "tool": "codex", "confirmed": true, "requestId": "install-request-0001"})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(result)
	if strings.Contains(string(data), "private") || strings.Contains(string(data), "secret") || strings.Contains(string(data), "direct-owner") {
		t.Fatal("installer response exposed local details")
	}
	if result.(map[string]any)["job"].(map[string]any)["id"] != "job-id" {
		t.Fatal("installer was replayed")
	}
}

func TestDirectManagementInstallRetainsReceiptAfterAnotherJob(t *testing.T) {
	s, ctx := directManagementFixture(t)
	setupState.mu.Lock()
	previous, receipts := setupState.job, setupState.receipts
	setupState.job = &setupJob{ID: "later-local-job", Tool: "claude", State: "done"}
	setupState.receipts = map[string]*setupJob{"direct-owner\x00install-request-0001": {ID: "original-job", Owner: "direct-owner", RequestID: "install-request-0001", Tool: "codex", State: "done"}}
	setupState.mu.Unlock()
	t.Cleanup(func() {
		setupState.mu.Lock()
		setupState.job, setupState.receipts = previous, receipts
		setupState.mu.Unlock()
	})
	result, err := s.dispatchDirectManagement(ctx, map[string]any{"type": "computer.agent.install", "controlSurface": "cli", "tool": "codex", "confirmed": true, "requestId": "install-request-0001"})
	if err != nil {
		t.Fatal(err)
	}
	if result.(map[string]any)["job"].(map[string]any)["id"] != "original-job" {
		t.Fatal("old request started a new installer")
	}
	setupState.mu.Lock()
	defer setupState.mu.Unlock()
	if setupState.job.ID != "later-local-job" {
		t.Fatal("replay replaced current job")
	}
}

func TestDirectManagementAllowlistAndHubSeparation(t *testing.T) {
	s, _ := directManagementFixture(t)
	cmd := directCreateFixture()
	if err := directCommandAllowed(cmd); err != nil {
		t.Fatal(err)
	}
	hub := hubclient.New(hubclient.Options{Store: s.d.Store})
	if _, err := hub.Dispatch(context.Background(), cmd); err == nil {
		t.Fatal("Hub accepted direct management command")
	}
	if len(s.d.Store.Get().LocalAccounts) != 0 {
		t.Fatal("Hub request mutated credentials")
	}
}
