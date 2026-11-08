package hubclient

import (
	"context"
	"errors"
	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
)

func (c *Client) dispatchSessionRevert(ctx context.Context, cfg config.Config, cmd map[string]any, recoverOnly ...bool) (any, error) {
	for field, raw := range cmd {
		switch field {
		case "type", "sessionKey", "expectedTurnId", "expectedMessageId", "operationId", "accountId", "controlSurface":
		default:
			return nil, errors.New("latest_turn_revert_invalid: 参数无效")
		}
		value, ok := raw.(string)
		if !ok || len(value) > 1024 {
			return nil, errors.New("latest_turn_revert_invalid: 参数无效")
		}
	}
	if str(cmd, "controlSurface") != "cli" {
		return nil, errors.New("latest_turn_revert_unavailable: 当前控制方式不支持编辑已发送消息")
	}
	a, id, err := agentFor(c.manager(), str(cmd, "sessionKey"))
	if err != nil {
		return nil, err
	}
	if err = validateAdapterCommand(a, cmd); err != nil {
		return nil, err
	}
	if err = requireAgentCapability(a, "latestTurnRevert"); err != nil {
		return nil, err
	}
	if !agents.ValidTurnID(str(cmd, "expectedTurnId")) || !agents.ValidTurnID(str(cmd, "expectedMessageId")) || !agents.ValidControlOperationID(str(cmd, "operationId")) {
		return nil, errors.New("latest_turn_revert_invalid: 目标消息或操作标识无效")
	}
	scope := featureCatalogScope(ctx, cfg, a, str(cmd, "accountId"), "", id)
	guard := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := c.validateCommandGuard(ctx, "session.revert"); err != nil {
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
		if IsDirectExecution(ctx) {
			if err := validateDirectBinding(ctx, current); err != nil {
				return err
			}
		}
		manager := c.manager()
		if manager == nil || manager.Get(a.ID()) != a || scope != featureCatalogScope(ctx, current, a, str(cmd, "accountId"), "", id) {
			return errors.New("latest_turn_revert_source_changed: API 或连接已经变化")
		}
		if err := c.validateExecutionAccountFor(ctx, current, a.ID(), str(cmd, "sessionKey"), str(cmd, "accountId")); err != nil {
			return err
		}
		return requireAgentCapability(a, "latestTurnRevert")
	}
	if err = guard(); err != nil {
		return nil, err
	}
	if len(recoverOnly) > 0 && recoverOnly[0] {
		recoverer, ok := a.(agents.LatestTurnRevertRecoverer)
		if !ok {
			return nil, errors.New("latest_turn_revert_unconfirmed: 请刷新原会话核对结果")
		}
		return recoverer.ReconcileLatestTurnRevert(ctx, id, str(cmd, "expectedTurnId"), str(cmd, "expectedMessageId"), str(cmd, "operationId"), guard)
	}
	return a.(agents.LatestTurnReverter).RevertLatestTurn(ctx, id, str(cmd, "expectedTurnId"), str(cmd, "expectedMessageId"), str(cmd, "operationId"), guard)
}
