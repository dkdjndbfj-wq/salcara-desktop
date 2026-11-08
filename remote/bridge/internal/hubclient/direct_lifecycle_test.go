package hubclient

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
)

func TestDirectApprovalUsesExactSessionAndFrozenDirectSource(t *testing.T) {
	a, b := directAccount("a", "https://fixture.invalid"), directAccount("b", "https://fixture.invalid")
	c := statusFixture(t, config.Config{LocalAccounts: []config.LocalAccount{a, b},
		DirectAPI: map[string]config.RemoteAPI{"codex": {AccountID: a.ID}}, RemoteAPI: map[string]config.RemoteAPI{"codex": {AccountID: b.ID}}})
	c.o.Store.SetRuntimeGatewayKey("fixture-runtime")
	ctx := directFixtureContext(t, c)
	worker := &approvalScopeDispatchAgent{fakeAgent: &fakeAgent{id: "codex"}, pending: map[string]string{"fixture-approval": "codex:original"}}
	c.SetManager(&fakeManager{list: []agents.Agent{worker}})
	cfg := c.o.Store.Get()
	cmd := map[string]any{"type": "approval.respond", "tool": "codex", "controlSurface": "cli", "sessionKey": "codex:sibling",
		"approvalId": "fixture-approval", "decision": "allow", "accountId": APIHandle(cfg, a.ID)}
	if _, err := c.Dispatch(ctx, cmd); err == nil || worker.legacyCalls != 0 || len(worker.pending) != 1 {
		t.Fatal("direct approval answered another session through the legacy responder")
	}
	delete(cmd, "sessionKey")
	if _, err := c.Dispatch(ctx, cmd); err == nil || len(worker.scopedCalls) != 1 {
		t.Fatal("direct approval without exact session reached responder")
	}
	cmd["sessionKey"], cmd["accountId"] = "codex:original", APIHandle(cfg, b.ID)
	if _, err := c.Dispatch(ctx, cmd); err == nil || len(worker.scopedCalls) != 1 {
		t.Fatal("Hub source answered a direct approval")
	}
	cmd["accountId"] = APIHandle(cfg, a.ID)
	if _, err := c.Dispatch(ctx, cmd); err != nil || len(worker.pending) != 0 || len(worker.scopedCalls) != 2 || worker.legacyCalls != 0 {
		t.Fatalf("valid direct approval failed: %v", err)
	}
	if !reflect.DeepEqual(worker.scopedCalls[1].ctx, agents.WithDirectConfigSnapshot(ctx, executionConfig(ctx, cfg))) {
		t.Fatal("scoped direct approval lost its frozen worker source")
	}
	legacy := &approvalIDOnlyDispatchAgent{fakeAgent: &fakeAgent{id: "codex"}, pending: true}
	c.SetManager(&fakeManager{list: []agents.Agent{legacy}})
	if _, err := c.Dispatch(ctx, cmd); err == nil || legacy.calls != 0 || !legacy.pending {
		t.Fatal("direct approval silently fell back to an ID-only adapter")
	}
}

func TestDirectContextLifecycleKeepsItsPairingAndSourceAfterCommandCancellation(t *testing.T) {
	for _, change := range []string{"cancel-only", "hub-only", "relay-revoked", "phone-replaced", "source-aba", "source-cleared", "runtime-revoked"} {
		t.Run(change, func(t *testing.T) {
			a, b := directAccount("a", "https://fixture.invalid"), directAccount("b", "https://fixture.invalid")
			c := statusFixture(t, config.Config{LocalAccounts: []config.LocalAccount{a, b},
				DirectAPI: map[string]config.RemoteAPI{"codex": {AccountID: a.ID}}, RemoteAPI: map[string]config.RemoteAPI{"codex": {AccountID: b.ID}}})
			c.o.Store.SetRuntimeGatewayKey("fixture-runtime")
			paired := directFixtureContext(t, c)
			identity, pairing, _ := DirectExecutionValidator(paired)
			relayRevoked := false
			paired = WithDirectExecutionValidator(context.Background(), identity, func() error {
				if relayRevoked {
					return errors.New("relay closed")
				}
				return pairing()
			})
			ctx, cancel := context.WithCancel(paired)
			defer cancel()
			_, base, _ := aggregationFixture(t, "codex")
			worker := &contextDispatchAgent{aggregationFixtureAgent: base}
			worker.descriptor.Capabilities.Context, worker.descriptor.Capabilities.Compact = true, true
			c.SetManager(&fakeManager{list: []agents.Agent{worker}})
			cmd := contextCommand(true)
			cmd["accountId"] = APIHandle(c.o.Store.Get(), a.ID)
			if _, err := c.Dispatch(ctx, cmd); err != nil {
				t.Fatal(err)
			}
			cancel()
			if worker.lifecycle == nil || worker.admission == nil || !errors.Is(worker.admission(), context.Canceled) || worker.lifecycle() != nil {
				t.Fatal("command cancellation lost the admitted direct lifecycle")
			}
			switch change {
			case "relay-revoked":
				relayRevoked = true
			case "runtime-revoked":
				c.o.Store.SetRuntimeGatewayKey("")
			case "source-aba":
				for _, key := range []string{"replacement", a.Key} {
					if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.LocalAccounts[0].Key = key; return nil }); err != nil {
						t.Fatal(err)
					}
				}
			default:
				if err := c.o.Store.Update(func(cfg *config.Config) error {
					switch change {
					case "hub-only":
						cfg.RemoteAPI["codex"] = config.RemoteAPI{AccountID: a.ID}
					case "phone-replaced":
						cfg.DirectPhoneHash = "replacement"
					case "source-cleared":
						cfg.DirectAPI = nil
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := worker.lifecycle(); (err == nil) != (change == "cancel-only" || change == "hub-only") {
				t.Fatalf("direct lifecycle %s: %v", change, err)
			}
		})
	}
}
