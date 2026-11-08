package hubclient

import (
	"errors"
	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"strings"
)

func commandPermissionMode(cmd map[string]any, a agents.Agent, cfg config.Config) (string, error) {
	raw, present := cmd["permissionMode"]
	if !present {
		// A validated explicit Claude mode can downgrade an old full-access
		// worker even after consent was revoked. commandClaudeApproval performs
		// its separate enum/capability/consent checks before execution.
		if _, explicit := cmd["approval"]; a.ID() == "claude" && explicit {
			return "", nil
		}
		id := strings.TrimPrefix(str(cmd, "sessionKey"), a.ID()+":")
		if !cfg.AllowPhoneAutoAll && sessionRequiresFullAccessConsent(a, id) {
			return "", errors.New("permission_mode_denied: 电脑已关闭完全访问权限，请先选择请求批准")
		}
		return "", nil
	}
	mode, ok := raw.(string)
	if !ok || (mode != "ask" && mode != "auto_review" && mode != "full_access") {
		return "", errors.New("permission_mode_invalid: 权限模式无效")
	}
	d, err := cliAgentDescriptor(a)
	if err != nil {
		return "", err
	}
	supported := false
	for _, candidate := range publishedRemoteCapabilities(a, d.Capabilities).PermissionModes {
		supported = supported || candidate == mode
	}
	if !supported {
		return "", errors.New("permission_mode_unsupported: 当前 Agent 未提供所选权限模式")
	}
	if mode == "full_access" && !cfg.AllowPhoneAutoAll {
		return "", errors.New("permission_mode_denied: 电脑尚未允许手机开启完全访问权限")
	}
	return mode, nil
}

func (c *Client) permissionConsentGuard(cmd map[string]any, a agents.Agent, startApproval ...string) func() error {
	// Recheck after model/provider/permission RPC awaits. This revokes admission
	// of a new full-access turn without stopping a previously accepted task.
	mode := str(cmd, "permissionMode")
	if a.ID() == "claude" {
		if approval := str(cmd, "approval"); approval != "" {
			if approval == config.ApprovalAutoAll {
				mode = "full_access"
			} else {
				mode = "ask"
			}
		}
	}
	id := strings.TrimPrefix(str(cmd, "sessionKey"), a.ID()+":")
	if mode == "" && len(startApproval) > 0 && startApproval[0] == config.ApprovalAutoAll {
		// The legacy approval path (including an inherited computer default)
		// needs the same live consent fence, even before a session ID exists.
		mode = "full_access"
	}
	if mode == "" {
		// The first resume can discover full access on an original thread that
		// was not loaded at dispatch time. Keep a dynamic guard for adapters
		// that can report this; unrelated adapters retain their no-option send.
		_, known := a.(interface{ KnownSessionPermission(string) string })
		_, observed := a.(interface{ SessionRequiresFullAccessConsent(string) bool })
		if id == "" || (!known && !observed) {
			return nil
		}
	} else if mode != "full_access" {
		return nil
	}
	return func() error {
		if mode == "" && !sessionRequiresFullAccessConsent(a, id) {
			return nil
		}
		if c.o.Store == nil || c.o.Store.Get().AllowPhoneAutoAll {
			return nil
		}
		return errors.New("permission_mode_denied: 电脑已关闭完全访问权限，消息未发送")
	}
}

// Claude's native values are deliberately not aliases for Codex's approval
// enums. Presence of approvalModes is the v1 next-turn switching contract.
func commandClaudeApproval(cmd map[string]any, a agents.Agent, cfg config.Config) (string, error) {
	raw, present := cmd["approval"]
	if !present {
		return "", nil
	}
	value, valid := raw.(string)
	if !valid || !config.ValidApproval(value) {
		return "", errors.New("permission_mode_invalid: Claude 审批方式无效")
	}
	if a.ID() != "claude" || commandSurface(str(cmd, "type"), cmd) != "cli" {
		return "", errors.New("permission_mode_unsupported: 当前执行器不支持 Claude 审批方式")
	}
	d, err := cliAgentDescriptor(a)
	if err != nil {
		return "", err
	}
	supported := false
	for _, candidate := range publishedRemoteCapabilities(a, d.Capabilities).ApprovalModes {
		supported = supported || candidate == value
	}
	if !supported {
		return "", errors.New("permission_mode_unsupported: 当前 Claude 未提供会话内审批切换")
	}
	if value == config.ApprovalAutoAll && !cfg.AllowPhoneAutoAll {
		return "", errors.New("permission_mode_denied: 电脑尚未允许手机使用全部自动")
	}
	return value, nil
}

func sessionRequiresFullAccessConsent(a agents.Agent, id string) bool {
	if observed, ok := a.(interface{ SessionRequiresFullAccessConsent(string) bool }); ok && observed.SessionRequiresFullAccessConsent(id) {
		return true
	}
	known, ok := a.(interface{ KnownSessionPermission(string) string })
	return ok && known.KnownSessionPermission(id) == "full_access"
}
