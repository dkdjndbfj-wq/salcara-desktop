package hubclient

import (
	"context"
	"errors"
	"strings"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
)

func (c *Client) dispatchSteer(ctx context.Context, cmd map[string]any) (any, error) {
	// No per-turn override is meaningful for an already running turn. Reject
	// even empty/unknown fields rather than silently discard the user's choice.
	for field := range cmd {
		switch field {
		case "type", "sessionKey", "text", "expectedTurnId", "operationId", "accountId", "controlSurface":
		default:
			return nil, errors.New("引导只支持向当前轮次补充文字，不能更换模型、权限或添加图片")
		}
	}
	if str(cmd, "controlSurface") != "cli" {
		return nil, errors.New("agent_capability_unavailable: 当前桌面接口不支持运行中引导")
	}
	a, id, err := agentFor(c.manager(), str(cmd, "sessionKey"))
	if err != nil {
		return nil, err
	}
	if err := validateAdapterCommand(a, cmd); err != nil {
		return nil, err
	}
	if err := requireAgentCapability(a, "steer"); err != nil {
		return nil, err
	}
	expected, operation := str(cmd, "expectedTurnId"), str(cmd, "operationId")
	if !agents.ValidTurnID(expected) {
		return nil, agents.ErrExpectedTurnRequired
	}
	if !agents.ValidControlOperationID(operation) {
		return nil, errors.New("turn_control_operation_required: 操作标识无效")
	}
	text, valid := cmd["text"].(string)
	if !valid || strings.TrimSpace(text) == "" || len(text) > 1<<20 {
		return nil, errors.New("请输入有效的引导内容")
	}
	permission := c.permissionConsentGuard(cmd, a)
	guard := func() error {
		if err := c.validateCommandGuard(ctx, "session.steer"); err != nil {
			return err
		}
		if env, ok := executionSourceFromContext(ctx); ok {
			if err := c.validateExecutionSource(env); err != nil {
				return err
			}
		}
		cfg := config.Config{}
		if c.o.Store != nil {
			cfg = c.o.Store.Get()
		}
		if err := c.validateExecutionAccountFor(ctx, cfg, a.ID(), str(cmd, "sessionKey"), str(cmd, "accountId")); err != nil {
			return err
		}
		if permission != nil {
			return permission()
		}
		return nil
	}
	if err := guard(); err != nil {
		return nil, err
	}
	return a.(agents.ActiveTurnSteerer).SteerTurn(ctx, id, text, expected, operation, guard)
}
