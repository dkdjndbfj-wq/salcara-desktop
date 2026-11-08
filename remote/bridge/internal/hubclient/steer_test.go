package hubclient

import (
	"context"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/protocol"
)

type steerDispatchAgent struct {
	*aggregationFixtureAgent
	steers      int
	beforeGuard func()
}

func (a *steerDispatchAgent) SteerTurn(_ context.Context, id, text, expected, op string, guard func() error) (protocol.SteerResult, error) {
	if a.beforeGuard != nil {
		a.beforeGuard()
	}
	if err := guard(); err != nil {
		return protocol.SteerResult{}, err
	}
	a.steers++
	return protocol.SteerResult{Accepted: true, SessionKey: a.ID() + ":" + id, TurnID: expected, OperationID: op}, nil
}

func TestSteerDispatchIsOptInTextOnlyAndNeverFallsBack(t *testing.T) {
	for _, variant := range []string{"valid", "model", "effort", "approval", "permissionMode", "attachments", "unknown", "desktop", "missing-surface", "bad-operation", "bad-turn", "disabled"} {
		t.Run(variant, func(t *testing.T) {
			c, base, _ := aggregationFixture(t, "codex")
			a := &steerDispatchAgent{aggregationFixtureAgent: base}
			a.descriptor.Capabilities.Steer = true
			c.SetManager(&fakeManager{list: []agents.Agent{a}})
			cmd := map[string]any{"type": "session.steer", "sessionKey": "codex:s", "text": "focus tests", "expectedTurnId": "turn-1", "operationId": "11111111-2222-4333-8444-555555555555", "controlSurface": "cli"}
			switch variant {
			case "valid":
			case "desktop":
				cmd["controlSurface"] = "desktop"
			case "missing-surface":
				delete(cmd, "controlSurface")
			case "bad-operation":
				cmd["operationId"] = "invalid"
			case "bad-turn":
				cmd["expectedTurnId"] = ""
			case "disabled":
				a.descriptor.Capabilities.Steer = false
			default:
				cmd[variant] = ""
			}
			result, err := c.Dispatch(context.Background(), cmd)
			if (err == nil) != (variant == "valid") {
				t.Fatalf("unexpected dispatch result %+v %v", result, err)
			}
			want := 0
			if variant == "valid" {
				want = 1
			}
			if a.steers != want || a.modelCalls != 0 || len(a.sent)+len(a.started)+len(a.opened) != 0 {
				t.Fatal("steer used unrelated adapter entrypoint")
			}
		})
	}
}
