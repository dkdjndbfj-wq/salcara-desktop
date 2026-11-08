package hubclient

import (
	"context"
	"encoding/json"
	"errors"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func parseSessionWorkflowCommand(cmd map[string]any) (protocol.SessionWorkflowRequest, error) {
	var request protocol.SessionWorkflowRequest
	mutate := str(cmd, "type") == "session.workflow"
	invalid := errors.New("session_workflow_invalid: 会话任务参数无效")
	for field, raw := range cmd {
		switch field {
		case "type", "tool", "agent", "sessionKey", "accountId", "controlSurface":
			if value, ok := raw.(string); !ok || len(value) > 1024 {
				return request, invalid
			}
		case "kind", "expectedRevision", "operationId", "objective":
			value, ok := raw.(string)
			if !mutate || !ok || len(value) > 16000 {
				return request, invalid
			}
		case "tokenBudget":
			if !mutate || raw == nil {
				return request, invalid
			}
		case "target":
			target, ok := raw.(map[string]any)
			if !mutate || !ok {
				return request, invalid
			}
			for key, value := range target {
				if key != "type" && key != "branch" && key != "sha" && key != "instructions" {
					return request, invalid
				}
				if text, ok := value.(string); !ok || len(text) > 16000 {
					return request, invalid
				}
			}
		default:
			return request, invalid
		}
	}
	encoded, err := json.Marshal(cmd)
	if err != nil || len(encoded) > 32768 {
		return request, invalid
	}
	if err := json.Unmarshal(encoded, &request); err != nil {
		return request, invalid
	}
	if mutate && !agents.ValidSessionWorkflow(request) {
		return request, invalid
	}
	return request, nil
}

func validWorkflowGoal(goal *protocol.SessionGoal, id string) bool {
	if goal == nil {
		return true
	}
	if goal.ThreadID != id || len(goal.Objective) == 0 || len(goal.Objective) > 64000 || goal.TokensUsed < 0 || goal.TokensUsed > 9007199254740991 || goal.TimeUsedSeconds < 0 || goal.TimeUsedSeconds > 9007199254740991 || goal.CreatedAt < 0 || goal.CreatedAt > 9007199254740991 || goal.UpdatedAt < 0 || goal.UpdatedAt > 9007199254740991 {
		return false
	}
	if goal.TokenBudget != nil && (*goal.TokenBudget <= 0 || *goal.TokenBudget > 9007199254740991) {
		return false
	}
	switch goal.Status {
	case "active", "paused", "blocked", "usageLimited", "budgetLimited", "complete":
		return true
	}
	return false
}

func (c *Client) dispatchSessionWorkflows(ctx context.Context, cfg config.Config, cmd map[string]any) (any, error) {
	request, err := parseSessionWorkflowCommand(cmd)
	if err != nil {
		return nil, err
	}
	if str(cmd, "controlSurface") != "cli" {
		return nil, errors.New("session_workflow_unavailable: 当前控制方式不支持会话任务")
	}
	a, id, err := agentFor(c.manager(), str(cmd, "sessionKey"))
	if err != nil {
		return nil, err
	}
	if err := validateAdapterCommand(a, cmd); err != nil {
		return nil, err
	}
	if err := requireAgentCapability(a, "sessionWorkflows"); err != nil {
		return nil, err
	}
	scope := featureCatalogScope(ctx, cfg, a, str(cmd, "accountId"), "", id)
	guard := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := c.validateCommandGuard(ctx, str(cmd, "type")); err != nil {
			return err
		}
		current := cfg
		if c.o.Store != nil {
			current = executionConfig(ctx, c.o.Store.Get())
		}
		if env, ok := executionSourceFromContext(ctx); ok {
			if err := c.validateExecutionSourceFor(env, current); err != nil {
				return err
			}
		}
		manager := c.manager()
		if manager == nil || manager.Get(a.ID()) != a || scope != featureCatalogScope(ctx, current, a, str(cmd, "accountId"), "", id) {
			return errors.New("session_workflow_source_changed: API 或连接已经变化")
		}
		if err := c.validateExecutionAccountFor(ctx, current, a.ID(), str(cmd, "sessionKey"), str(cmd, "accountId")); err != nil {
			return err
		}
		return requireAgentCapability(a, "sessionWorkflows")
	}
	if err := guard(); err != nil {
		return nil, err
	}
	provider := a.(agents.SessionWorkflowProvider)
	if str(cmd, "type") == "session.workflows" {
		value, err := provider.SessionWorkflows(ctx, id, guard)
		if err != nil {
			return nil, err
		}
		if err := guard(); err != nil {
			return nil, err
		}
		if value.SessionKey != str(cmd, "sessionKey") || len(value.UnavailableReason) > 4096 || len(value.GoalActions) > 4 || !validWorkflowGoal(value.Goal, id) || !value.GoalSupported && (value.Goal != nil || len(value.GoalActions) > 0) || (value.CanReview || len(value.GoalActions) > 0) && !agents.ValidTurnID(value.Revision) {
			return nil, errors.New("session_workflow_invalid: 会话任务状态无效")
		}
		seen := map[string]bool{}
		for _, action := range value.GoalActions {
			if seen[action] || action != "set" && action != "pause" && action != "resume" && action != "clear" || value.Goal == nil && action != "set" {
				return nil, errors.New("session_workflow_invalid: 目标操作无效")
			}
			seen[action] = true
		}
		if value.GoalActions == nil {
			value.GoalActions = []string{}
		}
		return value, nil
	}
	value, err := provider.ApplySessionWorkflow(ctx, id, request, guard)
	if err != nil {
		return nil, err
	}
	if err := guard(); err != nil {
		return nil, err
	}
	valid := value.SessionKey == str(cmd, "sessionKey") && value.OperationID == request.OperationID && value.Kind == request.Kind && len(value.Error) <= 4096 && validWorkflowGoal(value.Goal, id)
	if value.State != "unconfirmed" {
		if request.Kind == "review" {
			valid = valid && value.State == "requested" && agents.ValidTurnID(value.TurnID) && value.Goal == nil
		} else {
			valid = valid && value.State == "completed" && value.TurnID == ""
			switch request.Kind {
			case "goal.clear":
				valid = valid && value.Goal == nil
			case "goal.set":
				valid = valid && value.Goal != nil && value.Goal.Objective == request.Objective && (request.TokenBudget == nil || value.Goal.TokenBudget != nil && *request.TokenBudget == *value.Goal.TokenBudget)
			case "goal.pause":
				valid = valid && value.Goal != nil && value.Goal.Status == "paused"
			case "goal.resume":
				valid = valid && value.Goal != nil && value.Goal.Status == "active"
			}
		}
	} else {
		valid = valid && value.TurnID == ""
	}
	if !valid {
		return nil, errors.New("session_workflow_unconfirmed: 会话任务结果未确认")
	}
	return value, nil
}
