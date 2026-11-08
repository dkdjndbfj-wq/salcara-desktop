package hubclient

import (
	"context"
	"encoding/json"
	"errors"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func sessionManagementCommand(typ string) bool {
	return typ == "session.revert" || typ == "session.actions" || typ == "session.action" || typ == "sessions.archived" || typ == "session.workflows" || typ == "session.workflow"
}

func (c *Client) dispatchSessionActions(ctx context.Context, cfg config.Config, cmd map[string]any) (any, error) {
	typ := str(cmd, "type")
	mutate, archived := typ == "session.action", typ == "sessions.archived"
	for field, raw := range cmd {
		allowed, max := false, 1024
		switch field {
		case "type", "tool", "agent", "accountId", "controlSurface":
			allowed = true
		case "sessionKey":
			allowed = !archived
		case "action", "name", "expectedRevision", "operationId":
			allowed = mutate
		case "cursor":
			allowed, max = archived, 4096
		case "limit":
			if archived {
				continue // pageArguments checks the exact numeric value below.
			}
		}
		value, valid := raw.(string)
		if !allowed || !valid || len(value) > max {
			return nil, errors.New("session_action_invalid: 会话管理参数无效")
		}
	}
	if str(cmd, "controlSurface") != "cli" {
		return nil, errors.New("session_action_unavailable: 当前控制方式不支持会话管理")
	}
	var a agents.Agent
	var id string
	var err error
	capability := "sessionActions"
	if archived {
		if manager := c.manager(); manager != nil {
			a = manager.Get(str(cmd, "tool"))
		}
		capability = "archivedSessions"
	} else {
		a, id, err = agentFor(c.manager(), str(cmd, "sessionKey"))
	}
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, errors.New("session_action_unavailable: 电脑没有这个 Agent")
	}
	if err := validateAdapterCommand(a, cmd); err != nil {
		return nil, err
	}
	if err := requireAgentCapability(a, capability); err != nil {
		return nil, err
	}
	request := protocol.SessionActionRequest{Action: str(cmd, "action"), Name: str(cmd, "name"), ExpectedRevision: str(cmd, "expectedRevision"), OperationID: str(cmd, "operationId")}
	if mutate && !agents.ValidSessionAction(request) {
		return nil, errors.New("session_action_invalid: 会话操作、名称或版本无效")
	}
	scope := featureCatalogScope(ctx, cfg, a, str(cmd, "accountId"), "", id)
	guard := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := c.validateCommandGuard(ctx, typ); err != nil {
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
			return errors.New("session_action_source_changed: API 或连接已经变化")
		}
		if err := c.validateExecutionAccountFor(ctx, current, a.ID(), str(cmd, "sessionKey"), str(cmd, "accountId")); err != nil {
			return err
		}
		return requireAgentCapability(a, capability)
	}
	if err := guard(); err != nil {
		return nil, err
	}
	if archived {
		cursor, limit, err := pageArguments(cmd, maxSessions)
		if err != nil {
			return nil, err
		}
		sessions, next, err := a.(agents.ArchivedSessionProvider).ArchivedSessions(ctx, cursor, limit, guard)
		if err != nil {
			return nil, err
		}
		if err := guard(); err != nil {
			return nil, err
		}
		if len(sessions) > limit || len(next) > 4096 {
			return nil, errors.New("session_action_invalid: 归档目录超过读取上限")
		}
		out := append([]protocol.SessionInfo{}, sessions...)
		for i, info := range out {
			if !agents.SessionBelongsTo(a, info.SessionKey, info) || !info.Archived {
				return nil, errors.New("session_action_invalid: 归档会话身份不一致")
			}
			info.ControlSurface, info.Controllable, info.ActiveTurnID = "cli", false, ""
			out[i] = info
		}
		result := map[string]any{"sessions": out, "nextCursor": next}
		encoded, err := json.Marshal(result)
		if err != nil || len(encoded) > 512<<10 {
			return nil, errors.New("session_action_invalid: 归档目录超过读取上限")
		}
		return result, nil
	}
	controller := a.(agents.SessionActionProvider)
	if !mutate {
		result, err := controller.SessionActions(ctx, id, guard)
		if err != nil {
			return nil, err
		}
		if err := guard(); err != nil {
			return nil, err
		}
		if result.SessionKey != str(cmd, "sessionKey") || len(result.Actions) > 4 || len(result.Title) > 4096 || len(result.UnavailableReason) > 4096 || len(result.Actions) > 0 && !agents.ValidTurnID(result.Revision) {
			return nil, errors.New("session_action_invalid: 会话管理状态无效")
		}
		seen := map[string]bool{}
		for _, action := range result.Actions {
			if seen[action] || action != "rename" && action != "archive" && action != "unarchive" && action != "fork" || result.Archived && action == "archive" || !result.Archived && action == "unarchive" {
				return nil, errors.New("session_action_invalid: 会话管理能力无效")
			}
			seen[action] = true
		}
		if result.Actions == nil {
			result.Actions = []string{}
		}
		return result, nil
	}
	result, err := controller.ApplySessionAction(ctx, id, request, guard)
	if err != nil {
		return nil, err
	}
	if err := guard(); err != nil {
		return nil, err
	}
	if result.SessionKey != str(cmd, "sessionKey") || result.OperationID != request.OperationID || result.Action != request.Action || result.State != "completed" && result.State != "unconfirmed" || len(result.Error) > 4096 {
		return nil, errors.New("session_action_unconfirmed: 会话操作结果未确认")
	}
	if result.Session != nil && (!agents.SessionBelongsTo(a, result.Session.SessionKey, *result.Session) || request.Action == "fork" && result.Session.SessionKey == result.SessionKey || request.Action != "fork" && result.Session.SessionKey != result.SessionKey) {
		return nil, errors.New("session_action_unconfirmed: 会话操作结果归属不一致")
	}
	if result.State == "completed" && request.Action == "fork" && result.Session == nil {
		return nil, errors.New("session_action_unconfirmed: 分支会话尚未确认")
	}
	return result, nil
}
