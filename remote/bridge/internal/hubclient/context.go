package hubclient

import (
	"context"
	"errors"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func (c *Client) dispatchContext(ctx context.Context, cfg config.Config, cmd map[string]any) (any, error) {
	compact := str(cmd, "type") == "session.compact"
	for field, raw := range cmd {
		allowed := false
		switch field {
		case "type", "sessionKey", "agent", "tool", "controlSurface", "accountId":
			allowed = true
		case "expectedRevision", "operationId":
			allowed = compact
		}
		value, valid := raw.(string)
		if !allowed || !valid || len(value) > 1024 {
			return nil, errors.New("context_control_invalid: 上下文操作参数无效")
		}
	}
	if str(cmd, "controlSurface") != "cli" {
		return nil, errors.New("context_unavailable: 当前控制方式不支持上下文操作")
	}
	a, id, err := agentFor(c.manager(), str(cmd, "sessionKey"))
	if err != nil {
		return nil, err
	}
	if err := validateAdapterCommand(a, cmd); err != nil {
		return nil, err
	}
	operation := "context"
	if compact {
		operation = "compact"
		if !agents.ValidTurnID(str(cmd, "expectedRevision")) || !agents.ValidControlOperationID(str(cmd, "operationId")) {
			return nil, errors.New("context_control_invalid: 上下文版本或操作标识无效")
		}
	}
	if err := requireAgentCapability(a, operation); err != nil {
		return nil, err
	}
	// Reuse the source fingerprint only; context readings are never put in the
	// feature directory cache. The fingerprint includes the authorization epoch
	// so A -> B -> A cannot revive an operation waiting on a native response.
	scope := featureCatalogScope(ctx, cfg, a, str(cmd, "accountId"), "", id)
	permission := c.permissionConsentGuard(cmd, a)
	// Native completion can arrive after this command's HTTP context is canceled.
	// Detach that deadline only; keep all identity/source/permission revisions
	// live so cancellation cannot either revoke valid completion or hide a later
	// account/phone/consent revocation from the adapter's lifecycle observer.
	sourceContext := context.WithoutCancel(ctx)
	commandType, accountID, sessionKey := str(cmd, "type"), str(cmd, "accountId"), str(cmd, "sessionKey")
	sourceGuard := func() error {
		if err := c.validateCommandGuard(sourceContext, commandType); err != nil {
			return err
		}
		current := cfg
		if c.o.Store != nil {
			current = executionConfig(sourceContext, c.o.Store.Get())
		}
		if isLocalExecution(sourceContext) && IsDirectExecution(sourceContext) {
			if err := c.validateDirectAdmission(sourceContext, current, cmd); err != nil {
				return err
			}
		}
		if env, ok := executionSourceFromContext(sourceContext); ok {
			if err := c.validateExecutionSourceFor(env, current); err != nil {
				return err
			}
		}
		manager := c.manager()
		if manager == nil || manager.Get(a.ID()) != a || scope != featureCatalogScope(sourceContext, current, a, accountID, "", id) {
			return errors.New("context_source_changed: API 或连接已经变化，上下文操作已取消")
		}
		if err := c.validateExecutionAccountFor(sourceContext, current, a.ID(), sessionKey, accountID); err != nil {
			return err
		}
		if err := requireAgentCapability(a, operation); err != nil {
			return err
		}
		if permission != nil {
			return permission()
		}
		return nil
	}
	guard := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return sourceGuard()
	}
	ctx = agents.WithContextLifecycleGuard(ctx, sourceGuard)
	if err := guard(); err != nil {
		return nil, err
	}
	var out protocol.SessionContext
	if compact {
		out, err = a.(agents.ContextCompactor).CompactContext(ctx, id, str(cmd, "expectedRevision"), str(cmd, "operationId"), guard)
	} else {
		out, err = a.(agents.ContextProvider).SessionContext(ctx, id, guard)
	}
	if err != nil {
		return nil, err
	}
	if err := guard(); err != nil {
		return nil, err
	}
	if out.SessionKey != str(cmd, "sessionKey") {
		return nil, errors.New("context_session_mismatch: 上下文会话归属不一致")
	}
	return out, nil
}
