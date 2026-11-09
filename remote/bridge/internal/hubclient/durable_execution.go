package hubclient

import (
	"context"
	"errors"
	"strings"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
)

func durableCommand(typ string) bool {
	switch typ {
	case "session.start", "session.send", "session.continue", "session.steer", "session.revert", "desktop.session.send", "approval.respond", "desktop.approval.respond", "session.compact":
		return true
	}
	return false
}

func durableRemoteContext(ctx context.Context) bool {
	if IsDirectExecution(ctx) {
		_, _, valid := DirectExecutionValidator(ctx)
		return valid
	}
	_, guarded := commandGuardFromContext(ctx)
	_, sourced := executionSourceFromContext(ctx)
	return guarded || sourced
}

// Called only after the normal Dispatch authorization and source checks, while
// taskConfigMu is held. A replay never bypasses current binding/account checks.
func (c *Client) beginDurableExecution(ctx context.Context, cfg config.Config, cmd map[string]any) (func(any, error) error, bool, any, error) {
	id := str(cmd, "operationId")
	if !durableCommand(str(cmd, "type")) || id == "" || c.o.Store == nil || !durableRemoteContext(ctx) {
		return nil, false, nil, nil
	}
	if !stationSwitchOperationID(id) {
		return nil, false, nil, errors.New("执行请求编号无效")
	}
	scope := "hub:" + eventIdentity(cfg)
	if c.eventScopeRequired(cfg) {
		frozenScope := c.eventScopeFromContext(ctx, cfg)
		if !IsDirectExecution(ctx) && !isLocalExecution(ctx) && !c.commandEventScopeValid(cfg, frozenScope) {
			return nil, false, nil, errors.New("手机绑定代次已改变，指令未继续执行")
		}
		scope += ":" + frozenScope
	}
	if IsDirectExecution(ctx) {
		_, validate, ok := DirectExecutionValidator(ctx)
		if !ok {
			return nil, false, nil, errors.New("直连授权已失效")
		}
		if err := validate(); err != nil {
			return nil, false, nil, err
		}
		scope = "direct:" + cfg.ComputerID + ":" + cfg.DirectPhoneID + ":" + cfg.DirectPhoneHash
	} else if isLocalExecution(ctx) {
		scope = "local:" + cfg.ComputerID
	}
	if !c.durableReady() {
		return nil, false, nil, errors.New("执行记录暂时无法保存，请检查电脑存储后重试")
	}
	family := config.NormalizeAgentFamily(str(cmd, "tool"))
	if family == "" {
		family, _, _ = strings.Cut(str(cmd, "sessionKey"), ":")
	}
	// Ignore process-local launch leases and timestamps: reconnecting the same
	// source must recover its receipt, while a credential/API change must not.
	accountID := str(cmd, "accountId")
	if accountID == "" {
		accountID = cfg.RemoteAPI[family].AccountID
	}
	if resolved, found := AccountForHandle(cfg, accountID); found {
		accountID = resolved
	}
	account, ok := cfg.LocalAccount(accountID)
	if !ok {
		account, _ = cfg.SelectedToolAccount(family)
	}
	var descriptor any
	if manager := c.manager(); manager != nil {
		if agent := manager.Get(family); agent != nil {
			descriptor = agents.Describe(agent)
		}
	}
	source := durableHash(map[string]any{"descriptor": descriptor, "remote": cfg.RemoteAPI[family], "selection": cfg.ToolAPISelections[family], "model": cfg.ToolModels[family], "protocol": cfg.ToolProtocols[family], "path": cfg.LocalToolPaths[family], "account": config.AccountFingerprint(account), "credentialEpoch": account.CredentialEpoch, "headers": account.ExtraHeaders, "codex": cfg.ActiveCodexAccount, "claude": cfg.ActiveClaudeAccount})

	scope = durableHash(scope)
	replayed, result, err := c.durable.claim(scope, id, durableHash(cmd), source)
	if err != nil && replayed && cmd["projectless"] == true {
		// A verified native acceptance may repair this exact crashed claim; it
		// never authorizes another execution. The SQL update rechecks both the
		// immutable payload and source because claim intentionally uses the same
		// uncertain error for a crash and for a fingerprint mismatch.
		if known, ok := c.knownProjectlessReceipt(ctx, cmd); ok {
			if repaired, repairErr := c.durable.completeKnownClaim(scope, id, durableHash(cmd), source, known); repairErr == nil && repaired {
				return nil, true, known, nil
			}
		}
	}
	if err != nil || replayed {
		return nil, replayed, result, err
	}
	return func(result any, runErr error) error { return c.durable.finish(scope, id, result, runErr) }, false, nil, nil
}
