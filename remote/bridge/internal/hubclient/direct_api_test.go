package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func directFixtureContext(t *testing.T, c *Client) context.Context {
	t.Helper()
	if err := c.o.Store.Update(func(cfg *config.Config) error {
		cfg.DirectRelayEnabled, cfg.DirectPhoneID, cfg.DirectPhoneHash = true, "direct-fixture-phone", strings.Repeat("b", 64)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	frozen := c.o.Store.Get()
	return WithDirectExecutionValidator(context.Background(), strings.Repeat("a", 64), func() error {
		current := c.o.Store.Get() // Also proves validators never run under Store.Update.
		if !current.DirectRelayEnabled || current.DirectPhoneID != frozen.DirectPhoneID || current.DirectPhoneHash != frozen.DirectPhoneHash {
			return errors.New("revoked")
		}
		return nil
	})
}

type directSnapshotAgent struct {
	*modelOptionsAgent
	ctx context.Context
}

func (a *directSnapshotAgent) SendWithOptions(ctx context.Context, id, text string, opts agents.TurnOptions) error {
	a.ctx = ctx
	return a.modelOptionsAgent.SendWithOptions(ctx, id, text, opts)
}

func directAccount(id, root string) config.LocalAccount {
	return config.LocalAccount{ID: id, Name: "Fixture " + strings.ToUpper(id), Kind: "api", SourceType: "api_key", Key: "fixture-key-" + id,
		BaseURL: root, Wire: "chat", AuthMode: "bearer", Models: []string{"model-" + id}, RemoteHubBindingsSet: true}
}

func TestDirectAPISwitchCatalogAndFrozenWorkerAreIndependentOfHubAndDesktop(t *testing.T) {
	var directCalls, otherCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected catalog path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") == "Bearer fixture-key-c" {
			directCalls.Add(1)
		} else {
			otherCalls.Add(1)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"model-c"}]}`))
	}))
	defer server.Close()
	a, b, d := directAccount("a", server.URL), directAccount("b", server.URL), directAccount("c", server.URL)
	c := statusFixture(t, config.Config{ComputerID: "fixture-computer", LocalAccounts: []config.LocalAccount{a, b, d},
		RemoteAPI: map[string]config.RemoteAPI{"codex": {AccountID: b.ID, Model: "model-b"}}, ToolAPISelections: map[string]string{"codex": a.ID}})
	c.o.Store.SetRuntimeGatewayKey("fixture-runtime")
	if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.RecordAppliedAPI("codex", a, ""); return nil }); err != nil {
		t.Fatal(err)
	}
	ctx := directFixtureContext(t, c)
	worker := &directSnapshotAgent{modelOptionsAgent: &modelOptionsAgent{fakeAgent: &fakeAgent{id: "codex"}}}
	c.SetManager(&fakeManager{list: []agents.Agent{worker}})
	before := c.o.Store.Get()
	selected, err := c.Dispatch(ctx, map[string]any{"type": "agents.api.set", "agent": "codex", "accountId": APIHandle(before, d.ID), "model": "model-c"})
	if err != nil {
		t.Fatal(err)
	}
	after := c.o.Store.Get()
	if after.DirectAPI["codex"].AccountID != d.ID || !reflect.DeepEqual(before.RemoteAPI, after.RemoteAPI) ||
		!reflect.DeepEqual(before.ToolAPISelections, after.ToolAPISelections) || !reflect.DeepEqual(before.ToolAPIApplied, after.ToolAPIApplied) {
		t.Fatal("direct selection changed Hub or desktop bindings")
	}
	status := selected.(map[string]any)
	for _, item := range status["agents"].([]agentStatus) {
		if item.ControlSurface != "cli" || item.DesktopLive != nil {
			t.Fatal("direct status exposed a desktop route")
		}
		if item.ID == "codex" && (!item.API.Configured || item.API.Source != "phone" || item.API.AccountID != APIHandle(after, d.ID)) {
			t.Fatalf("incorrect direct API status: %+v", item.API)
		}
	}
	for _, option := range status["apis"].([]remoteAPIOption) {
		if !option.RemoteEligibility.Supported || option.Station != nil || len(option.Stations) != 0 {
			t.Fatal("direct directory required a Hub station")
		}
	}
	encoded, _ := json.Marshal(status)
	if strings.Contains(string(encoded), "fixture-key") || strings.Contains(string(encoded), server.URL) {
		t.Fatal("direct metadata leaked a credential/address")
	}
	result, err := c.Dispatch(ctx, map[string]any{"type": "models.list", "tool": "codex"})
	if err != nil || !reflect.DeepEqual(result.(map[string]any)["models"], []string{"model-c"}) || directCalls.Load() != 1 || otherCalls.Load() != 0 {
		t.Fatalf("direct catalog mismatch: %v", err)
	}
	frozen := c.o.Store.Get()
	if _, err := c.Dispatch(ctx, map[string]any{"type": "session.send", "sessionKey": "codex:original", "text": "fixture", "model": "model-c", "accountId": APIHandle(frozen, d.ID)}); err != nil {
		t.Fatal(err)
	}
	expected := agents.WithDirectConfigSnapshot(ctx, executionConfig(ctx, frozen))
	if !reflect.DeepEqual(worker.ctx, expected) || reflect.DeepEqual(worker.ctx, agents.WithLocalConfigSnapshot(ctx, executionConfig(ctx, frozen))) {
		t.Fatal("worker did not receive the frozen direct settings context")
	}
	if _, err := c.Dispatch(ctx, map[string]any{"type": "session.send", "sessionKey": "codex:original", "text": "wrong source", "model": "model-c", "accountId": APIHandle(frozen, b.ID)}); err == nil || len(worker.sent) != 1 {
		t.Fatal("Hub handle overrode a direct worker source")
	}
	if _, err := c.Dispatch(ctx, map[string]any{"type": "agents.api.set", "agent": "codex", "accountId": ""}); err != nil {
		t.Fatal(err)
	}
	cleared := c.o.Store.Get()
	if len(cleared.DirectAPI) != 0 || !reflect.DeepEqual(before.RemoteAPI, cleared.RemoteAPI) || !reflect.DeepEqual(before.ToolAPIApplied, cleared.ToolAPIApplied) ||
		!reflect.DeepEqual(worker.ctx, expected) {
		t.Fatal("clearing direct choice changed another source or the already frozen context")
	}
	if api := directAPIStatus(cleared, "codex"); api.Source != "computer" || api.AccountID != APIHandle(cleared, a.ID) {
		t.Fatalf("default did not follow applied source: %+v", api)
	}
}

func TestDirectAdmissionRequiresLivePairingAndSelectedCredentialLease(t *testing.T) {
	for _, variant := range []string{"marker", "revoked", "unpaired", "runtime", "deleted", "empty-key", "epoch"} {
		t.Run(variant, func(t *testing.T) {
			a := directAccount("a", "https://fixture.invalid")
			c := statusFixture(t, config.Config{LocalAccounts: []config.LocalAccount{a}, DirectAPI: map[string]config.RemoteAPI{"codex": {AccountID: a.ID}}})
			c.o.Store.SetRuntimeGatewayKey("fixture-runtime")
			ctx := directFixtureContext(t, c)
			switch variant {
			case "marker":
				ctx = WithDirectExecution(context.Background())
			case "revoked":
				ctx = WithDirectExecutionValidator(context.Background(), "fixture", func() error { return errors.New("revoked") })
			case "runtime":
				c.o.Store.SetRuntimeGatewayKey("")
			default:
				if err := c.o.Store.Update(func(cfg *config.Config) error {
					switch variant {
					case "unpaired":
						cfg.DirectPhoneHash = ""
					case "deleted":
						cfg.LocalAccounts = nil
					case "empty-key":
						cfg.LocalAccounts[0].Key = ""
					case "epoch":
						cfg.GatewayAuthEpochs = nil
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			// Epochs are regenerated by committed updates; test its guard directly.
			if variant == "epoch" {
				cfg := c.o.Store.Get()
				cfg.GatewayAuthEpochs = nil
				if c.validateDirectAdmission(ctx, cfg, map[string]any{"type": "models.list", "tool": "codex"}) == nil {
					t.Fatal("missing direct epoch allowed execution")
				}
				return
			}
			worker := &modelOptionsAgent{fakeAgent: &fakeAgent{id: "codex"}}
			c.SetManager(&fakeManager{list: []agents.Agent{worker}})
			if _, err := c.Dispatch(ctx, map[string]any{"type": "models.list", "tool": "codex"}); err == nil {
				t.Fatal("invalid direct source reached models")
			}
			if len(worker.sent) != 0 || len(worker.started) != 0 {
				t.Fatal("invalid source reached worker")
			}
			if variant == "runtime" || variant == "deleted" || variant == "empty-key" {
				if api := directAPIStatus(c.o.Store.Get(), "codex"); api.Configured || api.Source != "phone" {
					t.Fatal("invalid explicit choice silently fell back")
				}
			}
		})
	}
}

func TestDirectAPISwitchBlocksBusyAndPendingApproval(t *testing.T) {
	for _, state := range []string{"running", "waiting_approval", "pending-only"} {
		t.Run(state, func(t *testing.T) {
			a := directAccount("a", "https://fixture.invalid")
			c := statusFixture(t, config.Config{LocalAccounts: []config.LocalAccount{a}})
			c.o.Store.SetRuntimeGatewayKey("fixture-runtime")
			ctx := directFixtureContext(t, c)
			var worker agents.Agent = &fakeAgent{id: "codex", sessions: []protocol.SessionInfo{{SessionKey: "codex:original", Status: state}}}
			if state == "pending-only" {
				worker = newPendingOnlySwitchAgent("codex", "codex")
			}
			c.SetManager(&fakeManager{list: []agents.Agent{worker}})
			if _, err := c.Dispatch(ctx, map[string]any{"type": "agents.api.set", "agent": "codex", "accountId": APIHandle(c.o.Store.Get(), a.ID)}); err == nil {
				t.Fatal("busy/approval source was changed")
			}
			if len(c.o.Store.Get().DirectAPI) != 0 {
				t.Fatal("rejected change persisted a choice")
			}
		})
	}
}

func TestDirectAPISwitchRechecksPairingAfterCatalogAndAvoidsValidatorDeadlock(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		fmt.Fprint(w, `{"data":[{"id":"model-a"}]}`)
	}))
	defer server.Close()
	a := directAccount("a", server.URL)
	c := statusFixture(t, config.Config{LocalAccounts: []config.LocalAccount{a}})
	c.o.Store.SetRuntimeGatewayKey("fixture-runtime")
	ctx := directFixtureContext(t, c)
	c.SetManager(&fakeManager{list: []agents.Agent{&modelOptionsAgent{fakeAgent: &fakeAgent{id: "codex"}}}})
	done := make(chan error, 1)
	go func() {
		_, err := c.Dispatch(ctx, map[string]any{"type": "agents.api.set", "agent": "codex", "accountId": APIHandle(c.o.Store.Get(), a.ID), "model": "model-a"})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("catalog was not reached")
	}
	if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.DirectPhoneHash = "new-phone"; return nil }); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("old pairing committed a direct API")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("direct validator deadlocked the store")
	}
	if len(c.o.Store.Get().DirectAPI) != 0 {
		t.Fatal("revoked choice persisted")
	}
}

func TestDirectExecutionProjectionCannotOverrideHubAuthority(t *testing.T) {
	cfg := config.Config{RemoteAPI: map[string]config.RemoteAPI{"codex": {AccountID: "hub"}}, DirectAPI: map[string]config.RemoteAPI{"codex": {AccountID: "direct"}}}
	ctx := WithDirectExecution(context.Background())
	projection := executionConfig(ctx, cfg)
	if projection.RemoteAPI["codex"].AccountID != "direct" {
		t.Fatal("direct source projection missing")
	}
	projection.RemoteAPI["codex"] = config.RemoteAPI{AccountID: "mutated"}
	if cfg.RemoteAPI["codex"].AccountID != "hub" || cfg.DirectAPI["codex"].AccountID != "direct" {
		t.Fatal("projection shared a mutable selection map")
	}
	for _, guarded := range []context.Context{withCommandGuard(ctx, commandGuard{}), withExecutionSource(ctx, protocol.CommandEnvelope{})} {
		if executionConfig(guarded, cfg).RemoteAPI["codex"].AccountID != "hub" || !reflect.DeepEqual(withWorkerSnapshot(guarded, cfg), agents.WithConfigSnapshot(guarded, cfg)) {
			t.Fatal("Direct marker overrode Hub authority")
		}
	}
}

func TestDirectStatusKeepsItsSourceWhenManagerRetires(t *testing.T) {
	a, b := directAccount("a", "https://fixture.invalid"), directAccount("b", "https://fixture.invalid")
	c := statusFixture(t, config.Config{LocalAccounts: []config.LocalAccount{a, b},
		DirectAPI: map[string]config.RemoteAPI{"codex": {AccountID: a.ID}}, RemoteAPI: map[string]config.RemoteAPI{"codex": {AccountID: b.ID}}})
	c.o.Store.SetRuntimeGatewayKey("fixture-runtime")
	ctx := directFixtureContext(t, c)
	// A manager can retire after the readiness snapshot, before status reads it.
	c.mgrReady.Store(true)
	status, err := c.Dispatch(ctx, map[string]any{"type": "agents.status"})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range status.(map[string]any)["agents"].([]agentStatus) {
		if item.ControlSurface != "cli" || item.ID == "codex" && item.API.AccountID != APIHandle(c.o.Store.Get(), a.ID) {
			t.Fatal("retiring manager bypassed direct source projection")
		}
	}
	for _, option := range status.(map[string]any)["apis"].([]remoteAPIOption) {
		if option.Station != nil || len(option.Stations) != 0 || !option.RemoteEligibility.Supported {
			t.Fatal("retiring manager restored Hub API directory policy")
		}
	}
}
