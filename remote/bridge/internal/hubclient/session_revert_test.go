package hubclient

import (
	"context"
	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/protocol"
	"testing"
)

type revertDispatchAgent struct {
	*aggregationFixtureAgent
	calls       int
	beforeGuard func()
}

func (a *revertDispatchAgent) RevertLatestTurn(_ context.Context, id, turn, message, op string, guard func() error) (protocol.LatestTurnRevertResult, error) {
	if a.beforeGuard != nil {
		a.beforeGuard()
	}
	if err := guard(); err != nil {
		return protocol.LatestTurnRevertResult{}, err
	}
	a.calls++
	return protocol.LatestTurnRevertResult{Status: "completed", SessionKey: a.ID() + ":" + id, TurnID: turn, MessageID: message, OperationID: op}, nil
}
func TestSessionRevertDispatchRequiresExactOptInContract(t *testing.T) {
	for _, variant := range []string{"valid", "disabled", "missing-contract", "desktop", "missing-surface", "bad-turn", "bad-message", "bad-operation", "extra-text", "source-changed"} {
		t.Run(variant, func(t *testing.T) {
			c, base, _ := aggregationFixture(t, "codex")
			a := &revertDispatchAgent{aggregationFixtureAgent: base}
			base.descriptor.Capabilities.LatestTurnRevert = true
			c.SetManager(&fakeManager{list: []agents.Agent{a}})
			cmd := map[string]any{"type": "session.revert", "sessionKey": "codex:s", "expectedTurnId": "last-turn", "expectedMessageId": "last-user", "operationId": "91000000-0000-4000-8000-000000000001", "controlSurface": "cli"}
			switch variant {
			case "disabled":
				base.descriptor.Capabilities.LatestTurnRevert = false
			case "missing-contract":
				c.SetManager(&fakeManager{list: []agents.Agent{base}})
			case "desktop":
				cmd["controlSurface"] = "desktop"
			case "missing-surface":
				delete(cmd, "controlSurface")
			case "bad-turn":
				cmd["expectedTurnId"] = ""
			case "bad-message":
				cmd["expectedMessageId"] = true
			case "bad-operation":
				cmd["operationId"] = "other"
			case "extra-text":
				cmd["text"] = "replacement"
			case "source-changed":
				a.beforeGuard = func() { base.descriptor.Capabilities.LatestTurnRevert = false }
			}
			_, err := c.Dispatch(context.Background(), cmd)
			if (err == nil) != (variant == "valid") {
				t.Fatalf("unexpected result %v", err)
			}
			want := 0
			if variant == "valid" {
				want = 1
			}
			if a.calls != want || len(base.started)+len(base.sent)+len(base.stopped)+len(base.opened) > 0 {
				t.Fatal("revert fell back to an unrelated operation")
			}
		})
	}
}
func TestSessionRevertUsesDurableExecutionAndSourceGuard(t *testing.T) {
	if !durableCommand("session.revert") || !sourceGuardExecutionType("session.revert") || !sessionManagementCommand("session.revert") {
		t.Fatal("revert bypasses the normal execution/source boundary")
	}
	_, base, _ := aggregationFixture(t, "codex")
	base.descriptor.Capabilities.LatestTurnRevert = true
	if publishedRemoteCapabilities(base, base.descriptor.Capabilities).LatestTurnRevert {
		t.Fatal("declaration without actual method granted revert")
	}
}
