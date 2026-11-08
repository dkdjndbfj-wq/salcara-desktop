package hubclient

import (
	"context"
	"errors"
	"testing"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/protocol"
)

type executionBudgetAgent struct {
	*fakeAgent
	remaining time.Duration
}

func (a *executionBudgetAgent) capture(ctx context.Context) error {
	if deadline, ok := ctx.Deadline(); ok {
		a.remaining = time.Until(deadline)
	}
	return errors.New("fixture stops before reading or mutating the workspace")
}

func (a *executionBudgetAgent) SessionMetadata(ctx context.Context, _ string) (protocol.SessionInfo, error) {
	return protocol.SessionInfo{}, a.capture(ctx)
}

func (a *executionBudgetAgent) Open(ctx context.Context, _ string) (protocol.SessionInfo, []protocol.Event, error) {
	return protocol.SessionInfo{}, nil, a.capture(ctx)
}

func TestRemoteExecutionPreservesCommandBudget(t *testing.T) {
	for _, fixture := range []struct {
		typ  string
		want time.Duration
	}{
		{"workspace.git.read", 90 * time.Second},
		{"workspace.git.mutate", 90 * time.Second},
		{"session.open", 38 * time.Second},
	} {
		t.Run(fixture.typ, func(t *testing.T) {
			hub, server := newFakeHub(t)
			c, store := newTestClient(t, server.URL, hub.key, nil)
			agent := &executionBudgetAgent{fakeAgent: &fakeAgent{id: "codex"}}
			c.SetManager(&fakeManager{list: []agents.Agent{agent}})
			command := map[string]any{"type": fixture.typ, "sessionKey": workspaceTestKey, "controlSurface": "cli"}
			if fixture.typ != "session.open" {
				command["expectedCwd"], command["action"] = t.TempDir(), "status"
			}
			cfg := store.Get()
			c.executeFor(protocol.CommandEnvelope{CommandID: "budget-fixture", DeviceID: cfg.DeviceID, Command: command}, cfg)
			if agent.remaining < fixture.want-time.Second || agent.remaining > fixture.want {
				t.Fatalf("adapter received %s, want nearly %s", agent.remaining, fixture.want)
			}
			select {
			case reply := <-hub.replies:
				if reply.CommandID != "budget-fixture" || reply.OK {
					t.Fatalf("unexpected reply: %+v", reply)
				}
			default:
				t.Fatal("remote execution did not deliver its reply")
			}
		})
	}
}
