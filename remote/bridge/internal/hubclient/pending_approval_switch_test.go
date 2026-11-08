package hubclient

import (
	"context"
	"reflect"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/protocol"
)

// The builtin adapter's actual registry/busy behavior is covered separately in
// agents/api_switch_pending_test.go. This dispatch fixture verifies that the
// resulting pending-only admission contract protects exact and shared families
// and the entire station, even when session metadata still reports idle.
type pendingOnlySwitchAgent struct {
	*approvalScopeDispatchAgent
	descriptor protocol.AgentDescriptor
}

func (a *pendingOnlySwitchAgent) Descriptor() protocol.AgentDescriptor { return a.descriptor }
func (a *pendingOnlySwitchAgent) ActiveRemoteTurns() bool              { return len(a.pending) > 0 }

func newPendingOnlySwitchAgent(id, family string) *pendingOnlySwitchAgent {
	descriptor := aggregationDescriptor(id)
	descriptor.APIFamily = family
	return &pendingOnlySwitchAgent{approvalScopeDispatchAgent: &approvalScopeDispatchAgent{
		fakeAgent: &fakeAgent{id: id, sessions: []protocol.SessionInfo{{SessionKey: id + ":idle", Tool: id, Status: "idle"}}},
		pending:   map[string]string{"synthetic-pending": id + ":idle"}}, descriptor: descriptor}
}

func TestPendingApprovalSwitchExactAndSharedAPIFamilyRemainUnchanged(t *testing.T) {
	for _, test := range []struct{ tool, family, selected string }{
		{"codex", "codex", "codex"}, {"claude", "claude", "claude-desktop"}, {"synthetic-helper", "codex", "codex"},
	} {
		t.Run(test.tool, func(t *testing.T) {
			c, _, target := sharedAccountFixture(t)
			pending := newPendingOnlySwitchAgent(test.tool, test.family)
			c.SetManager(&fakeManager{list: []agents.Agent{pending}})
			before := c.o.Store.Get()
			cmd := map[string]any{"type": "agents.api.set", "agent": test.selected, "accountId": APIHandle(before, target.ID)}
			if _, err := c.Dispatch(sharedAccountContext(c, cmd), cmd); err == nil {
				t.Fatal("idle metadata hid the pending request during a family API change")
			}
			if !reflect.DeepEqual(before, c.o.Store.Get()) || len(pending.pending) != 1 || len(pending.stopped) != 0 {
				t.Fatal("rejected API change mutated config, answered the question, or stopped a worker")
			}
			if !pending.RespondForSession(context.Background(), test.tool+":idle", "synthetic-pending", agents.ApprovalResponse{Decision: "deny"}) {
				t.Fatal("synthetic scoped response failed")
			}
			if _, err := c.Dispatch(sharedAccountContext(c, cmd), cmd); err != nil {
				t.Fatalf("answered pending-only request permanently blocked API change: %v", err)
			}
			if c.o.Store.Get().RemoteAPI[test.family].AccountID != target.ID || len(pending.stopped) != 0 {
				t.Fatal("idle API change lost the chosen account or interrupted the original worker")
			}
		})
	}
}

func TestPendingApprovalSwitchWholeStationBlocksUnrelatedAgent(t *testing.T) {
	for _, tool := range []string{"codex", "claude", "synthetic-helper"} {
		for _, release := range []string{"answer", "withdraw"} {
			t.Run(tool+"/"+release, func(t *testing.T) {
				c, source, target := stationSwitchFixture(t, nil, nil)
				pending := newPendingOnlySwitchAgent(tool, tool)
				c.SetManager(&fakeManager{list: []agents.Agent{pending}})
				in := stationSwitchInput(source, target)
				before := c.o.Store.Get()
				if _, err := c.switchStation(context.Background(), in); err == nil {
					t.Fatal("whole-computer station switch ignored an unrelated pending-only Agent")
				}
				if !reflect.DeepEqual(before, c.o.Store.Get()) || len(pending.pending) != 1 || len(pending.stopped) != 0 {
					t.Fatal("rejected station switch changed its receipt/config, consumed a question, or stopped a worker")
				}
				if release == "answer" {
					if !pending.RespondForSession(context.Background(), tool+":idle", "synthetic-pending", agents.ApprovalResponse{Decision: "deny"}) {
						t.Fatal("synthetic scoped response failed")
					}
				} else {
					delete(pending.pending, "synthetic-pending")
				}
				if result, err := c.switchStation(context.Background(), in); err != nil || result["switched"] != true {
					t.Fatalf("released pending-only request permanently blocked station migration: %v", err)
				}
				if c.o.Store.Get().DeviceID != target.DeviceID || len(pending.stopped) != 0 {
					t.Fatal("idle station migration changed or stopped the original worker")
				}
			})
		}
	}
}
