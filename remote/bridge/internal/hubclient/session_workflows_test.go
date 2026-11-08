package hubclient

import (
	"context"
	"strings"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

type workflowFixture struct {
	*sessionActionsFixture
	bad string
}

func (a *workflowFixture) SessionWorkflows(_ context.Context, id string, validate func() error) (protocol.SessionWorkflows, error) {
	err := a.check(validate)
	value := protocol.SessionWorkflows{SessionKey: "codex:" + id, Revision: "native-1", CanReview: true, GoalSupported: true, GoalActions: []string{"set"}}
	if a.bad == "foreign" {
		value.SessionKey = "codex:other"
	}
	if a.bad == "goal-action" {
		value.GoalActions = []string{"resume"}
	}
	return value, err
}
func (a *workflowFixture) ApplySessionWorkflow(_ context.Context, id string, request protocol.SessionWorkflowRequest, validate func() error) (protocol.SessionWorkflowResult, error) {
	err := a.check(validate)
	value := protocol.SessionWorkflowResult{SessionKey: "codex:" + id, OperationID: request.OperationID, Kind: request.Kind, State: "completed"}
	if request.Kind == "review" {
		value.State, value.TurnID = "requested", "turn-1"
	} else if request.Kind != "goal.clear" {
		value.Goal = &protocol.SessionGoal{ThreadID: id, Objective: request.Objective, Status: "active", TokenBudget: request.TokenBudget}
		if request.Kind != "goal.set" {
			value.Goal.Objective = "Existing goal"
		}
		if request.Kind == "goal.pause" {
			value.Goal.Status = "paused"
		}
	}
	switch a.bad {
	case "foreign":
		value.SessionKey = "codex:other"
	case "wrong-op":
		value.OperationID = "other"
	case "false-complete":
		value.State = "completed"
	case "missing-turn":
		value.TurnID = ""
	case "foreign-goal":
		value.Goal.ThreadID = "other"
	case "false-pause":
		value.Goal.Status = "active"
	case "wrong-budget":
		budget := int64(200)
		value.Goal.TokenBudget = &budget
	}
	return value, err
}
func workflowCommand(kind string) map[string]any {
	cmd := map[string]any{"type": "session.workflows", "tool": "codex", "sessionKey": "codex:s", "controlSurface": "cli"}
	if kind == "" {
		return cmd
	}
	cmd["type"], cmd["kind"], cmd["expectedRevision"], cmd["operationId"] = "session.workflow", kind, "native-1", "11111111-2222-4333-8444-555555555555"
	if kind == "review" {
		cmd["target"] = map[string]any{"type": "uncommittedChanges"}
	}
	if kind == "goal.set" {
		cmd["objective"], cmd["tokenBudget"] = "Finish the task", 100
	}
	return cmd
}
func workflowClient(t *testing.T) (*Client, *workflowFixture) {
	c, base, _ := aggregationFixture(t, "codex")
	a := &workflowFixture{sessionActionsFixture: &sessionActionsFixture{aggregationFixtureAgent: base}}
	a.descriptor.Capabilities.SessionWorkflows = true
	c.SetManager(&fakeManager{list: []agents.Agent{a}})
	return c, a
}
func TestSessionWorkflowDispatchBoundary(t *testing.T) {
	for _, kind := range []string{"", "review", "goal.set", "goal.pause", "goal.resume", "goal.clear"} {
		for _, variant := range []string{"valid", "missing-cap", "missing-interface", "missing-surface", "desktop", "foreign-agent", "unknown-field", "source-before", "source-after", "manager-before", "manager-after", "manager-removed-before", "manager-removed-after", "cancelled"} {
			t.Run(kind+"/"+variant, func(t *testing.T) {
				c, a := workflowClient(t)
				cmd := workflowCommand(kind)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				change := func() {
					if strings.HasPrefix(variant, "manager-removed-") {
						c.SetManager(nil)
						return
					}
					if strings.HasPrefix(variant, "manager-") {
						c.SetManager(&fakeManager{list: []agents.Agent{&workflowFixture{sessionActionsFixture: a.sessionActionsFixture}}})
						return
					}
					if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.CodexModel = "other"; return nil }); err != nil {
						t.Fatal(err)
					}
				}
				switch variant {
				case "missing-cap":
					a.descriptor.Capabilities.SessionWorkflows = false
				case "missing-interface":
					c.SetManager(&fakeManager{list: []agents.Agent{a.aggregationFixtureAgent}})
				case "missing-surface":
					delete(cmd, "controlSurface")
				case "desktop":
					cmd["controlSurface"] = "desktop"
				case "foreign-agent":
					cmd["agent"] = "claude"
				case "unknown-field":
					cmd["config"] = map[string]any{"model_provider": "other"}
				case "source-before", "manager-before", "manager-removed-before":
					a.before = change
				case "source-after", "manager-after", "manager-removed-after":
					a.after = change
				case "cancelled":
					cancel()
				}
				result, err := c.Dispatch(ctx, cmd)
				if (err == nil) != (variant == "valid") {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				if variant != "valid" && !strings.HasSuffix(variant, "-before") && !strings.HasSuffix(variant, "-after") && a.calls != 0 {
					t.Fatal("invalid command reached adapter")
				}
				if len(a.started)+len(a.sent)+len(a.opened)+a.modelCalls != 0 {
					t.Fatal("workflow used ordinary send or history fallback")
				}
			})
		}
	}
}
func TestSessionWorkflowInvalidNativeResult(t *testing.T) {
	for _, tc := range []struct{ kind, bad string }{{"", "foreign"}, {"", "goal-action"}, {"review", "foreign"}, {"review", "wrong-op"}, {"review", "false-complete"}, {"review", "missing-turn"}, {"goal.set", "foreign-goal"}, {"goal.set", "wrong-budget"}, {"goal.pause", "false-pause"}} {
		t.Run(tc.kind+"/"+tc.bad, func(t *testing.T) {
			c, a := workflowClient(t)
			a.bad = tc.bad
			if result, err := c.Dispatch(context.Background(), workflowCommand(tc.kind)); err == nil {
				t.Fatalf("accepted invalid result %+v", result)
			}
		})
	}
}
func TestSessionWorkflowStrictParameters(t *testing.T) {
	for _, variant := range []string{"null-budget", "negative-budget", "fraction-budget", "string-budget", "large-budget", "extra-target", "mixed-target", "invalid-sha", "option-branch", "bidi-objective", "missing-revision", "bad-operation", "read-mutation"} {
		t.Run(variant, func(t *testing.T) {
			c, a := workflowClient(t)
			cmd := workflowCommand("goal.set")
			switch variant {
			case "null-budget":
				cmd["tokenBudget"] = nil
			case "negative-budget":
				cmd["tokenBudget"] = -1
			case "fraction-budget":
				cmd["tokenBudget"] = 1.5
			case "string-budget":
				cmd["tokenBudget"] = "100"
			case "large-budget":
				cmd["tokenBudget"] = 9007199254740992
			case "extra-target":
				cmd = workflowCommand("review")
				cmd["target"] = map[string]any{"type": "uncommittedChanges", "cwd": "C:/other"}
			case "mixed-target":
				cmd = workflowCommand("review")
				cmd["target"] = map[string]any{"type": "custom", "instructions": "Review", "sha": "abcdefa"}
			case "invalid-sha":
				cmd = workflowCommand("review")
				cmd["target"] = map[string]any{"type": "commit", "sha": "HEAD"}
			case "option-branch":
				cmd = workflowCommand("review")
				cmd["target"] = map[string]any{"type": "baseBranch", "branch": "--other"}
			case "bidi-objective":
				cmd["objective"] = "hidden\u202eroute"
			case "missing-revision":
				delete(cmd, "expectedRevision")
			case "bad-operation":
				cmd["operationId"] = "guess"
			case "read-mutation":
				cmd["type"] = "session.workflows"
			}
			if _, err := c.Dispatch(context.Background(), cmd); err == nil || a.calls != 0 {
				t.Fatalf("bad request dispatched err=%v calls=%d", err, a.calls)
			}
		})
	}
}
func TestSessionWorkflowSourceABA(t *testing.T) {
	for _, kind := range []string{"", "review", "goal.set"} {
		t.Run(kind, func(t *testing.T) {
			c, store, _, env := sourceGuardFixture(t, "api_key")
			_, a := workflowClient(t)
			c.SetManager(&fakeManager{list: []agents.Agent{a}})
			env.Command = workflowCommand(kind)
			env.Command["accountId"] = env.Source.CredentialHandle
			env.Source.Model = ""
			a.before = func() {
				for _, key := range []string{"key-b", "key-a"} {
					if err := store.Update(func(cfg *config.Config) error { cfg.LocalAccounts[0].Key = key; return nil }); err != nil {
						t.Fatal(err)
					}
				}
			}
			if value, err := c.Dispatch(withExecutionSource(context.Background(), env), env.Command); err == nil || !strings.Contains(err.Error(), "session_workflow_source_changed") {
				t.Fatalf("ABA result=%+v err=%v", value, err)
			}
		})
	}
}
