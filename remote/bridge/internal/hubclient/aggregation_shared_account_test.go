package hubclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

// These fixtures exercise only dispatch and a synthetic model directory. They
// never launch a native tool, read the user's auth files, or call an inference API.
type sharedAccountWorker struct {
	*approvalScopeDispatchAgent
	busy        bool
	sendOptions []agents.TurnOptions
}

func (a *sharedAccountWorker) ActiveRemoteTurns() bool { return a.busy }
func (a *sharedAccountWorker) SendWithOptions(ctx context.Context, id, text string, opts agents.TurnOptions) error {
	a.sendOptions = append(a.sendOptions, opts)
	return a.fakeAgent.Send(ctx, id, text)
}

type sharedAccountExtensionWorker struct {
	*aggregationFixtureAgent
	busy bool
}

func (a *sharedAccountExtensionWorker) ActiveRemoteTurns() bool { return a.busy }

func sharedAccountFixture(t *testing.T) (*Client, config.LocalAccount, config.LocalAccount) {
	t.Helper()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Errorf("unexpected synthetic provider request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"synthetic-model"}]}`))
	}))
	t.Cleanup(provider.Close)
	a := config.LocalAccount{ID: "shared-account", Name: "Synthetic shared API", Kind: "api", SourceType: "api_key",
		BaseURL: provider.URL + "/v1", Key: "synthetic-shared-key", Wire: "responses", Models: []string{"synthetic-model"}}
	b := a
	b.ID, b.Name, b.Key = "other-account", "Synthetic other API", "synthetic-other-key"
	c := statusFixture(t, config.Config{DeviceID: "fixture-computer", DeviceSecret: "synthetic-device-secret",
		PhoneBindingID: strings.Repeat("a", 64), PhoneHash: strings.Repeat("b", 64), RemoteDeviceOnly: true,
		LocalAccounts: []config.LocalAccount{a, b}, RemoteAPI: map[string]config.RemoteAPI{
			"codex": {AccountID: a.ID, Model: "synthetic-model"}, "claude": {AccountID: a.ID, Model: "synthetic-model"}},
		ToolAPISelections: map[string]string{"codex": a.ID, "claude": a.ID},
		ToolModels:        map[string]string{"codex": "synthetic-model", "claude": "synthetic-model"},
		ToolProtocols:     map[string]string{"codex": "responses", "claude": "anthropic"}})
	c.o.Store.SetRuntimeGatewayKey("synthetic-process-lease")
	cfg := c.o.Store.Get()
	base, _ := config.NormalizeStationAPIBase(a.BaseURL)
	if err := c.o.Store.BindRemoteStation(config.RemoteIdentity(cfg), "synthetic-policy-r1", &protocol.StationPolicy{Version: 1, APIBases: []string{base}}, nil); err != nil {
		t.Fatal(err)
	}
	cfg = c.o.Store.Get()
	c.publishPair(PairStatus{DeviceID: cfg.DeviceID, Paired: true}, eventIdentity(cfg))
	return c, a, b
}

func sharedAccountContext(c *Client, cmd map[string]any) context.Context {
	cfg := c.o.Store.Get()
	env := protocol.CommandEnvelope{DeviceID: cfg.DeviceID, BindingID: cfg.PhoneBindingID, PhoneHash: cfg.PhoneHash,
		Phone: true, Command: cmd, Source: &protocol.ExecutionSource{Mode: "open", PolicyRevision: "synthetic-policy-r1"}}
	base, _ := config.NormalizeStationAPIBase(cfg.LocalAccounts[0].BaseURL)
	env.Source.Station = &protocol.StationPolicy{Version: 1, APIBases: []string{base}}
	return approvalScopeDispatchContext(cfg, env)
}

func newSharedAccountWorker(id string, busy bool) *sharedAccountWorker {
	return &sharedAccountWorker{approvalScopeDispatchAgent: &approvalScopeDispatchAgent{
		fakeAgent: &fakeAgent{id: id, sessions: []protocol.SessionInfo{{SessionKey: id + ":same", Tool: id, Status: "idle"}}},
		pending:   map[string]string{"same-approval": id + ":same"}}, busy: busy}
}

func TestAggregationSharedAccountCodexChangeLeavesBusyClaudeUnchanged(t *testing.T) {
	c, a, b := sharedAccountFixture(t)
	codex, claude := newSharedAccountWorker("codex", false), newSharedAccountWorker("claude", true)
	claude.sessions[0].Status = "running"
	c.SetManager(&fakeManager{list: []agents.Agent{codex, claude}})
	before := c.o.Store.Get()
	cx, cxOK := before.RemoteToolAccount("codex")
	cl, clOK := before.RemoteToolAccount("claude")
	if !cxOK || !clOK || cx.ID != a.ID || cl.ID != a.ID || cx.Kind != "codex" || cl.Kind != "claude" {
		t.Fatal("the reusable vault account was assigned to one tool family")
	}
	claudeCredential := before.RemoteGatewayCredential("remote-claude")
	codexCredential := before.RemoteGatewayCredential("remote-codex")
	if claudeCredential == "" || codexCredential == "" || claudeCredential == codexCredential {
		t.Fatal("a shared upstream key produced an interchangeable worker credential")
	}
	cmd := map[string]any{"type": "agents.api.set", "agent": "codex", "accountId": APIHandle(before, b.ID), "sessionKey": "codex:same"}
	if _, err := c.Dispatch(sharedAccountContext(c, cmd), cmd); err != nil {
		t.Fatalf("an unrelated busy Claude family blocked Codex selection: %v", err)
	}
	after := c.o.Store.Get()
	if after.RemoteAPI["codex"].AccountID != b.ID || after.RemoteGatewayCredential("remote-codex") == codexCredential {
		t.Fatal("Codex did not acquire its own new source")
	}
	if after.RemoteAPI["claude"] != before.RemoteAPI["claude"] || after.ToolAPISelections["claude"] != before.ToolAPISelections["claude"] ||
		after.ToolModels["claude"] != before.ToolModels["claude"] || after.ToolProtocols["claude"] != before.ToolProtocols["claude"] ||
		after.RemoteGatewayCredential("remote-claude") != claudeCredential || !reflect.DeepEqual(after.LocalAccounts, before.LocalAccounts) {
		t.Fatal("switching Codex rewrote Claude or mutated the shared vault account")
	}
	if !claude.busy || claude.sessions[0].SessionKey != "claude:same" || claude.sessions[0].Status != "running" ||
		len(claude.started)+len(claude.sent)+len(claude.stopped)+len(claude.opened) != 0 ||
		len(codex.started)+len(codex.sent)+len(codex.stopped)+len(codex.opened) != 0 {
		t.Fatal("selection stopped, replaced or otherwise executed an existing worker")
	}
}

func TestAggregationSharedAccountExplicitFamilyBusySiblingBlocksChange(t *testing.T) {
	c, _, b := sharedAccountFixture(t)
	codex := newSharedAccountWorker("codex", false)
	hermes := &sharedAccountExtensionWorker{aggregationFixtureAgent: &aggregationFixtureAgent{
		fakeAgent: &fakeAgent{id: "hermes"}, descriptor: aggregationDescriptor("hermes")}, busy: true}
	hermes.descriptor.APIFamily = "codex"
	c.SetManager(&fakeManager{list: []agents.Agent{codex, hermes}})
	before := c.o.Store.Get()
	cmd := map[string]any{"type": "agents.api.set", "agent": "codex", "accountId": APIHandle(before, b.ID)}
	if _, err := c.Dispatch(sharedAccountContext(c, cmd), cmd); err == nil {
		t.Fatal("an explicitly shared API family ignored its active sibling")
	}
	if !reflect.DeepEqual(before, c.o.Store.Get()) || len(hermes.sent)+len(hermes.started)+len(hermes.stopped) != 0 {
		t.Fatal("rejected family change mutated configuration or interrupted its sibling")
	}
	hermes.busy = false
	if _, err := c.Dispatch(sharedAccountContext(c, cmd), cmd); err != nil {
		t.Fatalf("idle shared family could not change its source: %v", err)
	}
	if c.o.Store.Get().RemoteAPI["codex"].AccountID != b.ID || len(hermes.sent)+len(hermes.started)+len(hermes.stopped) != 0 {
		t.Fatal("idle family change routed execution into its sibling")
	}
}

func TestAggregationSharedAccountSameRawSessionIDKeepsExactSendNamespace(t *testing.T) {
	c, a, _ := sharedAccountFixture(t)
	codex, claude := newSharedAccountWorker("codex", false), newSharedAccountWorker("claude", false)
	c.SetManager(&fakeManager{list: []agents.Agent{codex, claude}})
	for _, id := range []string{"codex", "claude"} {
		cmd := map[string]any{"type": "session.send", "tool": id, "agent": id, "sessionKey": id + ":same", "controlSurface": "cli",
			"accountId": APIHandle(c.o.Store.Get(), a.ID), "model": "synthetic-model", "text": id + " only"}
		if _, err := c.Dispatch(sharedAccountContext(c, cmd), cmd); err != nil {
			t.Fatalf("%s scoped send failed: %v", id, err)
		}
	}
	if !reflect.DeepEqual(codex.sent, []string{"same|codex only"}) || !reflect.DeepEqual(claude.sent, []string{"same|claude only"}) ||
		len(codex.sendOptions) != 1 || len(claude.sendOptions) != 1 {
		t.Fatal("matching raw IDs cross-routed a message or dropped model options")
	}
	wrong := map[string]any{"type": "session.send", "tool": "codex", "sessionKey": "claude:same", "controlSurface": "cli",
		"accountId": APIHandle(c.o.Store.Get(), a.ID), "model": "synthetic-model", "text": "must not arrive"}
	if _, err := c.Dispatch(sharedAccountContext(c, wrong), wrong); err == nil || len(codex.sent) != 1 || len(claude.sent) != 1 {
		t.Fatal("a shared account bypassed the conflicting executor namespace guard")
	}
}

func TestAggregationSharedAccountSameApprovalIDKeepsExactResponderNamespace(t *testing.T) {
	c, a, _ := sharedAccountFixture(t)
	codex, claude := newSharedAccountWorker("codex", false), newSharedAccountWorker("claude", false)
	c.SetManager(&fakeManager{list: []agents.Agent{codex, claude}})
	cmd := map[string]any{"type": "approval.respond", "tool": "codex", "agent": "codex", "sessionKey": "claude:same", "controlSurface": "cli",
		"accountId": APIHandle(c.o.Store.Get(), a.ID), "approvalId": "same-approval", "decision": "allow"}
	if _, err := c.Dispatch(sharedAccountContext(c, cmd), cmd); err == nil || len(codex.scopedCalls)+len(claude.scopedCalls) != 0 {
		t.Fatal("conflicting approval namespace reached a responder")
	}
	cmd["sessionKey"] = "codex:same"
	if _, err := c.Dispatch(sharedAccountContext(c, cmd), cmd); err != nil {
		t.Fatal(err)
	}
	if len(codex.pending) != 0 || claude.pending["same-approval"] != "claude:same" || len(claude.scopedCalls) != 0 {
		t.Fatal("Codex consumed an identically named Claude approval")
	}
	cmd["tool"], cmd["agent"], cmd["sessionKey"] = "claude", "claude", "claude:same"
	if _, err := c.Dispatch(sharedAccountContext(c, cmd), cmd); err != nil {
		t.Fatal(err)
	}
	if len(claude.pending) != 0 || len(codex.scopedCalls) != 1 || len(claude.scopedCalls) != 1 || codex.legacyCalls+claude.legacyCalls != 0 ||
		codex.scopedCalls[0].sessionKey != "codex:same" || claude.scopedCalls[0].sessionKey != "claude:same" {
		t.Fatal("remote approval did not preserve its exact responder and session")
	}
}
