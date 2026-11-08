package hubclient

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/protocol"
)

func cliAgentDescriptor(a agents.Agent) (protocol.AgentDescriptor, error) {
	d, valid := agents.CanonicalDescriptor(a)
	if !valid || d.ControlSurface != "cli" {
		return protocol.AgentDescriptor{}, errors.New("agent_capability_unavailable: Agent 身份或控制方式未验证，不会改用其他工具")
	}
	if _, explicit := a.(agents.DescriptorProvider); !explicit && (a.ID() == "codex" || a.ID() == "claude") {
		// Compatibility is limited to reserved built-in test/embedded adapters.
		// An explicit descriptor (including false fields) always wins.
		d.Capabilities = builtinCapabilities(a.ID())
	}
	return d, nil
}

func requireAgentCapability(a agents.Agent, operation string) error {
	d, err := cliAgentDescriptor(a)
	if err != nil {
		return err
	}
	caps := publishedRemoteCapabilities(a, d.Capabilities)
	supported := false
	switch operation {
	case "latestTurnRevert":
		supported = caps.LatestTurnRevert
	case "sideQuestion":
		supported = caps.SideQuestion
	case "sessionWorkflows":
		supported = caps.SessionWorkflows
	case "sessionActions":
		supported = caps.SessionActions
	case "archivedSessions":
		supported = caps.ArchivedSessions
	case "contextReferences":
		supported = caps.ContextReferences
	case "contextAnnotations":
		supported = caps.ContextAnnotations
	case "contextDiffAnnotations":
		supported = caps.ContextDiffAnnotations
	case "context":
		supported = caps.Context
	case "compact":
		supported = caps.Compact
	case "features":
		supported = caps.Features
	case "list":
		supported = caps.List
	case "read":
		supported = caps.Read
	case "models":
		supported = caps.Read || caps.List
	case "send":
		supported = caps.Send
	case "interrupt":
		supported = caps.Interrupt
	case "preciseInterrupt":
		supported = caps.Interrupt && caps.PreciseInterrupt
	case "continue":
		supported = caps.Send && caps.Continue
	case "steer":
		supported = caps.Send && caps.Steer
	case "approval":
		supported = caps.Approval
	case "questions":
		supported = caps.Questions
	case "localApproval":
		// Legacy ID-only answers remain local-console compatible. Remote
		// dispatch never selects these operations and requires session scope.
		supported = d.Capabilities.Approval
	case "localQuestions":
		supported = d.Capabilities.Questions
	case "modelOverride":
		supported = caps.ModelOverride
	case "startModelOverride":
		// Mandatory Start has always carried its model; this does not promise
		// that the same adapter can change it later through SendWithOptions.
		supported = d.Capabilities.ModelOverride || caps.StartModelOverride
	case "attachments":
		supported = caps.Attachments
	case "imageInput":
		supported = caps.Attachments && caps.ImageInput
	}
	if !supported {
		return errors.New("agent_capability_unavailable: 当前 Agent 未提供此能力，操作未执行")
	}
	return nil
}

// explicitModelField preserves the exact wire value for handover receipts.
// In particular, malformed JSON values must not silently become a key-only
// change, and trimming here would change an already persisted payload hash.
func explicitModelField(cmd map[string]any) (string, error) {
	value, present := cmd["model"]
	if !present {
		return "", nil
	}
	model, valid := value.(string)
	if !valid {
		return "", errors.New("模型选项格式无效")
	}
	if err := validateExplicitModel(model); err != nil {
		return "", err
	}
	return model, nil
}

func validateExplicitModel(model string) error {
	if len(model) > 200 || strings.IndexFunc(model, unicode.IsControl) >= 0 {
		return errors.New("模型选项格式无效")
	}
	return nil
}

// requireExplicitModelCapability binds a generation override to the actual
// executor, never to a sibling adapter that merely shares its API family.
// Call before provider probes and again after all awaits, outside Store.Update:
// a vendor Descriptor may read settings and must not re-enter its config lock.
func (c *Client) requireExplicitModelCapability(agentID, model string) error {
	if err := validateExplicitModel(model); err != nil {
		return err
	}
	if strings.TrimSpace(model) == "" {
		return nil
	}
	id := strings.TrimSpace(agentID)
	if id == "codex-desktop" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := c.nativeModelStatus(ctx, ""); err != nil {
			return errors.New("agent_capability_unavailable: Codex 桌面原生连接未验证覆盖模型能力")
		}
		return nil
	}
	if id == "claude-desktop" {
		// This public card intentionally controls the Claude Code worker.
		id = "claude"
	}
	m := c.manager()
	if m == nil {
		// Preserve configuring the known CLI cards before manager startup.
		// Extensions have no legacy contract and must be explicitly registered.
		if id == "codex" || id == "claude" {
			return nil
		}
		return errors.New("agent_capability_unavailable: Agent 尚未提供覆盖模型能力")
	}
	a := m.Get(id)
	if a == nil || a.ID() != id {
		return errors.New("agent_capability_unavailable: Agent 尚未提供覆盖模型能力")
	}
	return requireAgentCapability(a, "modelOverride")
}

func validateAdapterCommand(a agents.Agent, cmd map[string]any) error {
	for _, field := range []string{"agent", "tool"} {
		if raw, present := cmd[field]; present {
			value, valid := raw.(string)
			if !valid {
				return errors.New("Agent 参数格式无效")
			}
			if value != "" && value != a.ID() {
				// These reserved public cards intentionally select the same CLI
				// worker. Extension API families do not create executor aliases.
				if !((a.ID() == "codex" || a.ID() == "claude") && value == a.ID()+"-desktop") {
					return errors.New("Agent 与会话归属不一致")
				}
			}
		}
	}
	return nil
}

func turnFields(cmd map[string]any) (string, string, []string, error) {
	model, effort := "", ""
	for _, field := range []struct {
		key    string
		target *string
	}{{"model", &model}, {"effort", &effort}} {
		key, target := field.key, field.target
		if value, present := cmd[key]; present {
			text, valid := value.(string)
			if !valid || len(text) > 200 || strings.ContainsAny(text, "\r\n\x00") {
				if key == "effort" {
					return "", "", nil, errors.New("无效的推理强度")
				}
				return "", "", nil, errors.New("模型选项格式无效")
			}
			*target = text
			if key == "model" {
				*target = strings.TrimSpace(text)
			}
		}
	}
	ids := []string{}
	if value, present := cmd["attachments"]; present {
		switch list := value.(type) {
		case []string:
			ids = append(ids, list...)
		case []any:
			for _, item := range list {
				id, valid := item.(string)
				if !valid {
					return "", "", nil, errors.New("图片参数格式无效")
				}
				ids = append(ids, id)
			}
		default:
			return "", "", nil, errors.New("图片参数格式无效")
		}
		if len(ids) > maxTurnAttachments {
			return "", "", nil, errors.New("图片数量超过上限")
		}
		for _, id := range ids {
			if !attachmentID.MatchString(id) {
				return "", "", nil, errors.New("图片标识无效")
			}
		}
	}
	return model, effort, ids, nil
}

func requireTurnCapabilities(a agents.Agent, model, effort string, ids []string, start bool) error {
	if err := requireAgentCapability(a, "send"); err != nil {
		return err
	}
	if model != "" || effort != "" {
		operation := "modelOverride"
		if start {
			operation = "startModelOverride"
		}
		if err := requireAgentCapability(a, operation); err != nil {
			return err
		}
	}
	if len(ids) != 0 {
		return requireAgentCapability(a, "imageInput")
	}
	return nil
}
