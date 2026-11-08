package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/hubclient"
	"salcara/bridge/internal/launcher"
)

// The same TLS phone that creates a private vault entry selects it through the
// actual Dispatcher. A production gateway request then proves which upstream
// receives inference; status metadata by itself is not sufficient evidence.
func TestDirectAPIPhoneCreateSwitchGatewayAndRevocation(t *testing.T) {
	f := newManagementTransportFixture(t, true)
	f.s.d.Store.SetRuntimeGatewayKey("synthetic-direct-process")
	f.s.d.Hub = hubclient.New(hubclient.Options{Store: f.s.d.Store,
		DiscoverTools: func(context.Context, map[string]string) []launcher.Tool {
			return []launcher.Tool{{ID: "codex", Kind: "codex", Name: "Codex", Available: true}}
		}})
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer direct-synthetic-provider" {
			t.Error("wrong provider credential")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"direct-fixture-model"}]}`))
			return
		}
		if r.URL.Path != "/v1/responses" {
			t.Errorf("wrong upstream route: %s", r.URL.Path)
		}
		calls.Add(1)
		_, _ = w.Write([]byte(`{"id":"response-fixture","object":"response","model":"direct-fixture-model","output":[]}`))
	}))
	defer upstream.Close()
	phone := f.connect(t, f.token, "phone-direct-api", "auth_ok")
	create := directCreateFixture()
	create["baseUrl"], create["key"], create["wire"], create["models"] = upstream.URL+"/v1", "direct-synthetic-provider", "responses", []string{"direct-fixture-model"}
	created, _ := managementTransportRequest(t, phone, "create-direct", create, true)
	handle := created["accountId"].(string)
	before := f.s.d.Store.Get()
	status, raw := managementTransportRequest(t, phone, "choose-direct", map[string]any{"type": "agents.api.set", "agent": "codex", "accountId": handle}, true)
	if status["agents"] == nil {
		t.Fatal("selection did not return agent status")
	}
	for _, secret := range []string{"direct-synthetic-provider", upstream.URL, before.LocalAccounts[0].ID} {
		if strings.Contains(raw, secret) {
			t.Fatal("direct status leaked provider information")
		}
	}
	cfg := f.s.d.Store.Get()
	if cfg.DirectAPI["codex"].AccountID != before.LocalAccounts[0].ID || len(cfg.RemoteAPI) != 0 || len(cfg.ToolAPIApplied) != 0 || len(cfg.ToolAPISelections) != 0 {
		t.Fatal("direct switch did not isolate worker, Hub and native selections")
	}
	worker := agents.DirectSettingsWithGateway(cfg, 47831)
	lease := cfg.DirectGatewayCredential("direct-codex")
	if lease == "" || worker.CodexKey != lease || !strings.Contains(worker.CodexRoot, "/gateway/direct-codex") || worker.UseOriginalCodex {
		t.Fatal("worker is not pinned to the direct gateway")
	}
	request := func(route, key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, route, strings.NewReader(`{"model":"direct-fixture-model","input":"synthetic direct inference"}`))
		r.Host, r.RemoteAddr = "127.0.0.1:47831", "127.0.0.1:12345"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		f.h.ServeHTTP(w, r)
		return w
	}
	if w := request("/gateway/direct-codex/v1/responses", lease); w.Code != 200 || calls.Load() != 1 {
		t.Fatalf("direct inference failed: status=%d body=%s", w.Code, w.Body)
	}
	if w := request("/gateway/remote-codex/v1/responses", lease); w.Code == 200 {
		t.Fatal("direct credential authorized Hub route")
	}
	managementTransportRequest(t, phone, "clear-direct", map[string]any{"type": "agents.api.set", "agent": "codex", "accountId": ""}, true)
	managementTransportRequest(t, phone, "restore-direct", map[string]any{"type": "agents.api.set", "agent": "codex", "accountId": handle}, true)
	if w := request("/gateway/direct-codex/v1/responses", lease); w.Code == 200 || calls.Load() != 1 {
		t.Fatal("old direct worker revived after selection was restored")
	}
	fresh := f.s.d.Store.Get().DirectGatewayCredential("direct-codex")
	if fresh == lease {
		t.Fatal("direct authorization did not rotate")
	}
	if w := request("/gateway/direct-codex/v1/responses", fresh); w.Code != 200 || calls.Load() != 2 {
		t.Fatalf("fresh direct request failed: %d %s", w.Code, w.Body)
	}
	if err := f.relay.Revoke(); err != nil {
		t.Fatal(err)
	}
	if w := request("/gateway/direct-codex/v1/responses", fresh); w.Code == 200 || calls.Load() != 2 {
		t.Fatal("revoked direct phone retained gateway authority")
	}
	encoded, _ := json.Marshal(f.s.d.Store.Get().RemoteAPI)
	if string(encoded) != "null" && string(encoded) != "{}" {
		t.Fatal("direct flow changed Hub selection")
	}
}

func TestDirectAPIAllowlistKeepsDesktopAndUnknownFieldsOut(t *testing.T) {
	base := map[string]any{"type": "agents.api.set", "agent": "codex", "accountId": "api_fixture"}
	if err := directCommandAllowed(base); err != nil {
		t.Fatal(err)
	}
	for _, variant := range []map[string]any{{"agent": "codex-desktop"}, {"controlSurface": "desktop"}, {"command": "arbitrary"}, {"accountId": true}} {
		cmd := map[string]any{}
		for k, v := range base {
			cmd[k] = v
		}
		for k, v := range variant {
			cmd[k] = v
		}
		if err := directCommandAllowed(cmd); err == nil {
			t.Fatalf("accepted forbidden direct API command: %v", variant)
		}
	}
}
