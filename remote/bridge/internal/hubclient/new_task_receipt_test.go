package hubclient

import (
	"context"
	"errors"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/protocol"
)

type startReceiptAgent struct {
	*modelOptionsAgent
	partialID string
	startErr  error
}

func (a *startReceiptAgent) StartWithOptions(ctx context.Context, cwd, prompt, approval string, opts agents.TurnOptions) (string, error) {
	a.startOptions = append(a.startOptions, opts)
	return a.partialID, a.startErr
}

func TestNewTaskReceiptCorrelatesFirstUserEcho(t *testing.T) {
	c, original, cwd := effortClient(t)
	a := &startReceiptAgent{modelOptionsAgent: original.modelOptionsAgent, partialID: "s1"}
	c.SetManager(&fakeManager{list: []agents.Agent{a}})
	operation := "11111111-2222-4333-8444-555555555555"
	if _, err := c.Dispatch(context.Background(), map[string]any{"type": "session.start", "tool": "codex", "cwd": cwd, "prompt": "fixture", "operationId": operation}); err != nil {
		t.Fatal(err)
	}
	if len(a.startOptions) != 1 || a.startOptions[0].ClientUserMessageID != operation {
		t.Fatal("first turn lost receipt correlation")
	}
}

func TestNewTaskPartialCreationCannotBecomeDefinitiveRejection(t *testing.T) {
	for _, fixture := range []struct {
		id   string
		err  error
		code string
	}{
		{"s1", errors.New("fixture first turn failed"), "command_delivery_uncertain"},
		{"bad\x00identifier", nil, "command_delivery_uncertain"},
		{"bad\u202fidentifier", nil, "command_delivery_uncertain"},
		{"__salcara_start_v1__:code", nil, "command_delivery_uncertain"},
		{"", errors.New("fixture pre-dispatch failure"), ""},
	} {
		c, original, cwd := effortClient(t)
		a := &startReceiptAgent{modelOptionsAgent: original.modelOptionsAgent, partialID: fixture.id, startErr: fixture.err}
		c.SetManager(&fakeManager{list: []agents.Agent{a}})
		_, err := c.Dispatch(context.Background(), map[string]any{"type": "session.start", "tool": "codex", "cwd": cwd, "prompt": "fixture"})
		if err == nil || protocol.ExecutionErrorCode(err) != fixture.code {
			t.Fatalf("wrong partial-creation boundary: %v", err)
		}
	}
}
