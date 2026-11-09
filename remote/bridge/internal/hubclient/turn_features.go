package hubclient

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func commandTurnFeatures(cmd map[string]any) (agents.TurnOptions, error) {
	var opts agents.TurnOptions
	switch str(cmd, "type") {
	case "session.start", "session.send", "session.continue":
		// Keep room for the authenticated request envelope below the Hub's
		// 256 KiB command-body cap. Reject the whole frozen input; never trim it.
		var encoded bytes.Buffer
		encoder := json.NewEncoder(&encoded)
		encoder.SetEscapeHTML(false)
		if encoder.Encode(cmd) != nil || encoded.Len()-1 > 240<<10 {
			return opts, errors.New("input_too_large: 正文、技能和引用合计过大，请缩短后重试")
		}
	}
	if raw, present := cmd["contextReferences"]; present {
		encoded, err := json.Marshal(raw)
		if err != nil || len(encoded) > 512<<10 || len(encoded) == 0 || encoded[0] != '[' {
			return opts, errors.New("context_reference_invalid: 上下文引用列表格式无效")
		}
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&opts.ContextReferences) != nil || protocol.ValidateContextReferences(opts.ContextReferences) != nil {
			return opts, errors.New("context_reference_invalid: 上下文引用格式无效或超过限制")
		}
		if len(opts.ContextReferences) > 0 {
			if len(str(cmd, "text")) > 64<<10 || len(str(cmd, "prompt")) > 64<<10 {
				return opts, errors.New("context_reference_invalid: 带引用的正文不能超过 64 KB，请缩短后重试")
			}
			switch str(cmd, "type") {
			case "session.start", "session.send", "session.continue":
			default:
				return opts, errors.New("context_reference_unavailable: 上下文引用仅支持 CLI 新回合")
			}
			if surface, present := cmd["controlSurface"]; present && surface != "cli" {
				return opts, errors.New("context_reference_unavailable: 当前控制方式不支持上下文引用")
			}
		}
	}
	if raw, present := cmd["collaborationMode"]; present {
		value, ok := raw.(string)
		if !ok {
			return opts, errors.New("agent_feature_invalid: 计划模式格式无效")
		}
		opts.CollaborationMode = value
	}
	if raw, present := cmd["skills"]; present {
		if raw == nil {
			return opts, errors.New("agent_feature_invalid: 技能列表格式无效")
		}
		encoded, err := json.Marshal(raw)
		if err != nil || len(encoded) > 80<<10 || len(encoded) == 0 || encoded[0] != '[' {
			return opts, errors.New("agent_feature_invalid: 技能列表格式无效")
		}
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&opts.Skills) != nil {
			return opts, errors.New("agent_feature_invalid: 技能列表包含无效字段")
		}
	}
	if (len(opts.ContextReferences) > 0 || len(opts.Skills) > 0) && (len(str(cmd, "text")) > 64<<10 || len(str(cmd, "prompt")) > 64<<10) {
		return opts, errors.New("context_reference_invalid: 带技能或引用的正文不能超过 64 KB，请缩短后重试")
	}
	if err := agents.ValidateTurnFeatures(opts); err != nil {
		return opts, err
	}
	if opts.HasFeatures() {
		switch str(cmd, "type") {
		case "session.start", "session.send", "session.continue":
		case "session.side-question.tools":
			if str(cmd, "action") != "send" || str(cmd, "tool") != "codex" || str(cmd, "controlSurface") != "cli" {
				return opts, errors.New("agent_feature_unavailable: 临时聊天仅在 CLI 新回合中使用计划模式与技能")
			}
		default:
			return opts, errors.New("agent_feature_unavailable: 计划模式与技能仅支持 CLI 新回合")
		}
		if surface, present := cmd["controlSurface"]; present && surface != "cli" {
			return opts, errors.New("agent_feature_unavailable: 当前控制方式不支持计划模式或技能")
		}
	}
	return opts, nil
}

func requireTurnFeatures(a agents.Agent, opts agents.TurnOptions) error {
	if len(opts.ContextReferences) > 0 {
		if err := requireAgentCapability(a, "contextReferences"); err != nil {
			return err
		}
		if protocol.HasContextAnnotations(opts.ContextReferences) {
			if err := requireAgentCapability(a, "contextAnnotations"); err != nil {
				return err
			}
		}
		if protocol.HasDiffAnnotations(opts.ContextReferences) {
			if err := requireAgentCapability(a, "contextDiffAnnotations"); err != nil {
				return err
			}
		}
		for _, ref := range opts.ContextReferences {
			if ref.SessionKey != "" && !strings.HasPrefix(ref.SessionKey, a.ID()+":") {
				return errors.New("context_reference_invalid: 上下文引用属于其他工具，请重新选择")
			}
		}
	}
	if !opts.HasFeatures() {
		return nil
	}
	return requireAgentCapability(a, "features")
}

func (c *Client) dispatchFeatures(ctx context.Context, m agents.Manager, cfg config.Config, cmd map[string]any) (any, error) {
	empty := protocol.AgentFeatures{CollaborationModes: []string{}, Skills: []protocol.Skill{}}
	syncRequest, syncErr := parseFeatureSyncRequest(cmd)
	if syncErr != nil {
		return empty, syncErr
	}
	force := false
	if raw, present := cmd["force"]; present {
		var ok bool
		force, ok = raw.(bool)
		if !ok {
			return empty, errors.New("agent_feature_invalid: 技能刷新参数无效")
		}
	}
	revision := str(cmd, "ifRevision")
	if raw, present := cmd["ifRevision"]; present {
		decoded, err := hex.DecodeString(revision)
		if _, ok := raw.(string); !ok || err != nil || len(decoded) != 32 {
			return empty, errors.New("agent_feature_invalid: 技能目录版本无效")
		}
	}
	if commandSurface("features.list", cmd) != "cli" {
		return empty, errors.New("agent_features_unavailable: 能力目录只支持 CLI 会话")
	}
	var a agents.Agent
	var id, cwd string
	var err error
	if key := str(cmd, "sessionKey"); key != "" {
		a, id, err = agentFor(m, key)
	} else {
		a = m.Get(str(cmd, "tool"))
	}
	if err != nil {
		return empty, err
	}
	if a == nil {
		return empty, errors.New("agent_features_unavailable: 未找到 Agent")
	}
	if err := validateAdapterCommand(a, cmd); err != nil {
		return empty, err
	}
	if err := requireAgentCapability(a, "features"); err != nil {
		return empty, err
	}
	provider, ok := a.(agents.FeatureProvider)
	if !ok {
		return empty, errors.New("agent_features_unavailable: 当前 Agent 未提供能力目录")
	}
	if id != "" {
		var info protocol.SessionInfo
		if reader, ok := a.(agents.SessionMetadataReader); ok {
			info, err = reader.SessionMetadata(ctx, id)
		} else {
			info, _, err = a.Open(ctx, id)
		}
		if err != nil {
			return empty, err
		}
		if !agents.SessionBelongsTo(a, str(cmd, "sessionKey"), info) || info.Cwd == "" {
			return empty, errors.New("agent_features_unavailable: 无法确认会话项目")
		}
		cwd = info.Cwd
	} else {
		if c.isManagedCwd(str(cmd, "cwd")) {
			return empty, ErrNotAllowed
		}
		grant, grantErr := explorerGrantForCwd(cfg, str(cmd, "cwd"))
		if grantErr != nil {
			return empty, grantErr
		}
		if grant {
			cwd, err = workspaceConfiguredRoot(cfg, str(cmd, "cwd"))
		} else {
			cwd, err = CheckCwd(str(cmd, "cwd"), cfg.Projects)
			if errors.Is(err, ErrNotAllowed) {
				cwd, err = CheckCwd(str(cmd, "cwd"), allowedProjects(ctx, m, cfg.Projects))
			}
		}
		if err != nil {
			return empty, err
		}
		if c.isManagedCwd(cwd) {
			return empty, ErrNotAllowed
		}
	}
	if _, err := explorerGrantForCwd(cfg, cwd); err != nil {
		return empty, err
	}
	key := featureCatalogScope(ctx, cfg, a, str(cmd, "accountId"), cwd, id)
	var out protocol.AgentFeatures
	var syncResult any
	if syncRequest.cursor != "" {
		syncResult, err = c.featureCatalog.continueSync(key, syncRequest)
	} else {
		out, err = c.featureCatalog.read(ctx, key, force, func(readCtx context.Context) (protocol.AgentFeatures, error) {
			return provider.Features(readCtx, cwd, id)
		})
		if err == nil && syncRequest.enabled {
			syncResult, err = c.featureCatalog.beginSync(key, out, revision, syncRequest.pageSize)
		}
	}
	if err != nil {
		return empty, err
	}
	if c.o.Store != nil && key != featureCatalogScope(ctx, executionConfig(ctx, c.o.Store.Get()), a, str(cmd, "accountId"), cwd, id) {
		c.featureCatalog.clear()
		return empty, errors.New("API 或连接已切换，请重新读取技能")
	}
	if err := c.validateCommandGuard(ctx, "features.list"); err != nil {
		c.featureCatalog.clear()
		return empty, err
	}
	if syncRequest.enabled {
		return syncResult, nil
	}
	if !force && revision != "" && revision == out.Revision {
		return struct {
			Revision    string `json:"revision"`
			NotModified bool   `json:"notModified"`
		}{out.Revision, true}, nil
	}
	return out, nil
}
