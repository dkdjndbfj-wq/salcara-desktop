package hubclient

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/protocol"
)

func TestSkillProjectionIsCountedInEventQueueBudget(t *testing.T) {
	e := protocol.Event{Type: "message", Role: "user", SessionKey: "codex:session"}
	before := eventWeight(e)
	for i := 0; i < 8; i++ {
		e.Skills = append(e.Skills, protocol.SkillReference{Name: strings.Repeat("n", 160), Path: strings.Repeat("\\", 4096)})
	}
	raw, err := json.Marshal(e)
	if err != nil || eventWeight(e) <= before || eventWeight(e) < len(raw) {
		t.Fatal("skill metadata bypassed retained/wire event budgets", eventWeight(e), len(raw), err)
	}
}

func TestClaudeSkillReceiptCorrelationReachesStartAndSend(t *testing.T) {
	for _, typ := range []string{"session.start", "session.send"} {
		t.Run(typ, func(t *testing.T) {
			c, base, cwd := aggregationFixture(t, "claude")
			a := &modelOptionsAgent{fakeAgent: base.fakeAgent}
			c.SetManager(&fakeManager{list: []agents.Agent{a}})
			operation := "11111111-2222-4333-8444-555555555555"
			_, err := c.Dispatch(context.Background(), map[string]any{"type": typ, "tool": "claude", "cwd": cwd,
				"sessionKey": "claude:s", "prompt": "fixture", "text": "fixture", "operationId": operation, "controlSurface": "cli"})
			if err != nil {
				t.Fatal(err)
			}
			options := a.sendOptions
			if typ == "session.start" {
				options = a.startOptions
			}
			if len(options) != 1 || options[0].ClientUserMessageID != operation {
				t.Fatal("Claude lost exact phone receipt identity", options)
			}
		})
	}
}

func TestSkillProjectionReceiptCorrelationReachesContinue(t *testing.T) {
	for _, tool := range []string{"codex", "claude"} {
		t.Run(tool, func(t *testing.T) {
			c, base, _ := aggregationFixture(t, tool)
			a := &claudeModeFixture{modelOptionsAgent: &modelOptionsAgent{fakeAgent: base.fakeAgent}}
			c.SetManager(&fakeManager{list: []agents.Agent{a}})
			operation := "11111111-2222-4333-8444-555555555555"
			_, err := c.Dispatch(context.Background(), map[string]any{"type": "session.continue", "sessionKey": tool + ":s",
				"text": "continue", "expectedTurnId": "terminal", "operationId": operation, "controlSurface": "cli"})
			if err != nil {
				t.Fatal(err)
			}
			if len(a.sendOptions) != 1 || a.sendOptions[0].ClientUserMessageID != operation {
				t.Fatal("continuation lost exact phone receipt identity", a.sendOptions)
			}
		})
	}
}
