package hubclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

var errSideToolsInvalid = errors.New("side_tools_invalid: 临时执行参数或结果无效")

type sideQuestionToolProvider interface {
	SideQuestionTools(context.Context, string, string, agents.SideQuestionSnapshot, protocol.SideQuestionToolRequest, func() error) (protocol.SideQuestionToolState, error)
}

func (c *Client) dispatchSideQuestionTools(ctx context.Context, cfg config.Config, cmd map[string]any) (any, error) {
	for field, raw := range cmd {
		switch field {
		case "type", "tool", "sessionKey", "accountId", "controlSurface", "action", "chainId", "expectedRevision", "operationId", "permission", "question", "model", "effort", "collaborationMode", "expectedTurnId", "approvalId", "decision":
			if value, ok := raw.(string); !ok || len(value) > 16<<10 || !utf8.ValidString(value) {
				return nil, errSideToolsInvalid
			}
		case "skills":
			if str(cmd, "action") != "send" || raw == nil {
				return nil, errSideToolsInvalid
			}
			encoded, err := json.Marshal(raw)
			if err != nil || len(encoded) > 24<<10 || len(encoded) == 0 || encoded[0] != '[' {
				return nil, errSideToolsInvalid
			}
			var skills []protocol.SkillReference
			decoder := json.NewDecoder(bytes.NewReader(encoded))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&skills) != nil || agents.ValidateTurnFeatures(agents.TurnOptions{Skills: skills}) != nil {
				return nil, errSideToolsInvalid
			}
		default:
			return nil, errSideToolsInvalid
		}
	}
	if str(cmd, "controlSurface") != "cli" || str(cmd, "tool") != "codex" {
		return nil, errSideToolsInvalid
	}
	var request protocol.SideQuestionToolRequest
	encoded, err := json.Marshal(cmd)
	if err != nil || len(encoded) > 32<<10 || json.Unmarshal(encoded, &request) != nil || !agents.ValidSideQuestionToolRequest(request) {
		return nil, errSideToolsInvalid
	}
	a, id, err := agentFor(c.manager(), str(cmd, "sessionKey"))
	if err != nil {
		return nil, err
	}
	provider, supported := a.(sideQuestionToolProvider)
	reader, metadata := a.(agents.SessionMetadataReader)
	if !supported || !metadata || a.ID() != "codex" {
		return nil, errors.New("side_tools_unavailable: 当前电脑未开放工具型临时聊天")
	}
	if err := requireAgentCapability(a, "sideQuestion"); err != nil {
		return nil, err
	}
	if err := validateAdapterCommand(a, cmd); err != nil {
		return nil, err
	}
	parent, err := reader.SessionMetadata(ctx, id)
	if err != nil || parent.SessionKey != str(cmd, "sessionKey") || parent.ControlSurface == "desktop" || parent.ControlSurface == "read-only" || parent.Archived {
		return nil, errors.New("side_tools_unavailable: 无法确认父会话的项目")
	}
	cwd, err := CheckCwd(parent.Cwd, cfg.Projects)
	if err != nil {
		return nil, err
	}
	directIdentity, directValidate, directGuarded := DirectExecutionValidator(ctx)
	if IsDirectExecution(ctx) && !directGuarded {
		return nil, errors.New("side_tools_unavailable: 当前直连没有临时执行授权租约")
	}
	scope := sideSnapshotScope(ctx, cfg, a, id)
	ownerData, _ := json.Marshal([]any{cfg.ComputerID, config.RemoteIdentity(cfg), cfg.PhoneBindingID, cfg.PhoneHash, directIdentity, isLocalExecution(ctx), scope, parent.SessionKey})
	ownerHash := sha256.Sum256(ownerData)
	owner := hex.EncodeToString(ownerHash[:])
	leaseKey := "side-tools:" + owner + ":" + request.ChainID
	liveCtx := context.WithoutCancel(ctx)
	accountID := str(cmd, "accountId")
	admitted := false
	liveValidate := func() error {
		if directGuarded {
			if err := directValidate(); err != nil {
				return err
			}
		}
		c.taskConfigMu.Lock()
		defer c.taskConfigMu.Unlock()
		if !admitted {
			if err := c.checkUpdateCommand(cmd); err != nil {
				return err
			}
			admitted = true
		}
		if err := c.validateCommandGuard(liveCtx, "session.side-question.tools"); err != nil {
			return err
		}
		c.expireUpdateLocked()
		if c.updatePrepared {
			return errors.New("程序正在更新")
		}
		current := cfg
		if c.o.Store != nil {
			current = executionConfig(liveCtx, c.o.Store.Get())
		}
		if env, ok := executionSourceFromContext(liveCtx); ok {
			if err := c.validateExecutionSourceFor(env, current); err != nil {
				return err
			}
			if err := c.bindExecutionStation(liveCtx, env); err != nil {
				return err
			}
		}
		manager := c.manager()
		if manager == nil || manager.Get(a.ID()) != a || sideSnapshotScope(liveCtx, current, a, id) != scope {
			return errors.New("side_tools_source_changed: 电脑或 API 已变化")
		}
		if err := c.validateExecutionAccountFor(liveCtx, current, a.ID(), parent.SessionKey, accountID); err != nil {
			return err
		}
		if err := requireAgentCapability(a, "sideQuestion"); err != nil {
			return err
		}
		checkCtx, cancel := context.WithTimeout(liveCtx, 3*time.Second)
		defer cancel()
		info, err := reader.SessionMetadata(checkCtx, id)
		if err != nil || info.SessionKey != parent.SessionKey || info.Archived || info.ControlSurface == "desktop" || info.ControlSurface == "read-only" || info.Cwd != parent.Cwd {
			return errors.New("side_tools_source_changed: 父会话项目已变化")
		}
		allowed, err := CheckCwd(info.Cwd, current.Projects)
		if err != nil || allowed != cwd {
			return ErrNotAllowed
		}
		// Private workers reserve their cwd before calling this validator. The
		// same lock also serializes ordinary main-task admissions.
		if request.Action == "send" || request.Action == "approve" && request.Decision == "allow" {
			return c.checkWorkspaceAdmission(checkCtx, manager, cwd, leaseKey)
		}
		return nil
	}
	// Read/stop/approve/close must remain usable when a conflicting external task
	// starts. The worker itself receives the full live validator and retires.
	if err := liveValidate(); err != nil {
		return nil, err
	}
	var snapshot agents.SideQuestionSnapshot
	if request.Action == "open" {
		c.sideQuestions.mu.Lock()
		entry, ok := c.sideQuestions.catalogs[scope+"\x00"+request.ExpectedRevision]
		c.sideQuestions.mu.Unlock()
		if !ok || time.Since(entry.at) > 20*time.Minute || entry.options == nil || entry.options.Tools == nil || entry.options.Tools.Version != 1 {
			return nil, errors.New("side_tools_unavailable: 请刷新临时聊天能力")
		}
		snapshot = cloneSideSnapshot(entry.snapshot)
	}
	if request.Action == "send" {
		if request.Model != "" {
			if _, err := c.validateManagedModel(ctx, cfg, "codex", request.Model); err != nil {
				return nil, err
			}
		}
		if err := validateModelEffort(ctx, cfg, a, request.Model, request.Effort); err != nil {
			return nil, err
		}
	}
	value, err := provider.SideQuestionTools(ctx, id, owner, snapshot, request, liveValidate)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := liveValidate(); err != nil {
		return nil, err
	}
	if value.SessionKey != parent.SessionKey || value.ChainID != request.ChainID || value.Revision != request.ExpectedRevision || value.Permission != "workspace-write" || value.Sequence > 9007199254740991 || len(value.Answer) > 64<<10 || !utf8.ValidString(value.Answer) || len(value.Error) > 4096 || !utf8.ValidString(value.Error) || len(value.Events) > 128 || len(value.Approvals) > 16 {
		return nil, errSideToolsInvalid
	}
	switch value.State {
	case "idle", "starting", "running", "waiting_approval", "completed", "interrupted", "failed", "unconfirmed", "closed":
	default:
		return nil, errSideToolsInvalid
	}
	if value.CanSend && value.State != "idle" && value.State != "completed" && value.State != "interrupted" && value.State != "failed" || value.ExpiresAt < 0 || value.ExpiresAt > 9007199254740991 || value.LastOperationID != "" && !agents.ValidControlOperationID(value.LastOperationID) || value.TurnID != "" && !agents.ValidTurnID(value.TurnID) {
		return nil, errSideToolsInvalid
	}
	if value.Features != nil {
		catalog := cloneFeatureCatalog(*value.Features)
		if len(catalog.CollaborationModes) > 2 || len(catalog.Skills) > protocol.MaxFeatureSkills || catalog.Revision != "" && !agents.ValidTurnID(catalog.Revision) {
			return nil, errSideToolsInvalid
		}
		seenModes, seenSkills := map[string]bool{}, map[string]bool{}
		for _, mode := range catalog.CollaborationModes {
			if mode != "default" && mode != "plan" || seenModes[mode] {
				return nil, errSideToolsInvalid
			}
			seenModes[mode] = true
		}
		for _, skill := range catalog.Skills {
			key := skill.Name + "\x00" + skill.Path
			if !catalog.SkillsSupported || seenSkills[key] || len(skill.Description) > 1000 || !utf8.ValidString(skill.Description) || agents.ValidateTurnFeatures(agents.TurnOptions{Skills: []protocol.SkillReference{{Name: skill.Name, Path: skill.Path, DisplayName: skill.DisplayName, BrandColor: skill.BrandColor, IconSmall: skill.IconSmall}}}) != nil {
				return nil, errSideToolsInvalid
			}
			seenSkills[key] = true
		}
		raw, err := json.Marshal(catalog)
		if err != nil || len(raw) > 161<<10 {
			return nil, errSideToolsInvalid
		}
		value.Features = &catalog
	}
	value.Events = append([]protocol.Event{}, value.Events...)
	value.Approvals = append([]protocol.Event{}, value.Approvals...)
	for _, approval := range value.Approvals {
		if value.State != "waiting_approval" || value.TurnID == "" || approval.Type != "approval.request" || approval.TurnID != value.TurnID || !agents.ValidTurnID(approval.ApprovalID) || len(approval.ApprovalID) > 128 || approval.Kind == "question" || approval.ApprovalOptions == nil || approval.ApprovalOptions.Version != 1 || len(approval.ApprovalOptions.Decisions) == 0 || len(approval.ApprovalOptions.Decisions) > 2 {
			return nil, errSideToolsInvalid
		}
		for _, decision := range approval.ApprovalOptions.Decisions {
			if decision != "allow" && decision != "deny" {
				return nil, errSideToolsInvalid
			}
		}
	}
	for _, events := range [][]protocol.Event{value.Events, value.Approvals} {
		for i := range events {
			event := &events[i]
			if event.SessionKey != parent.SessionKey || event.Tool != "codex" {
				return nil, errSideToolsInvalid
			}
			// Temporary output remains in memory; do not import screenshots into
			// the parent task's attachment cache or broadcast private sessions.
			event.ImageData, event.Attachments, event.Session = nil, nil, nil
			event.ChildSessionKeys, event.MessageOrigin = nil, nil
		}
	}
	encoded, err = json.Marshal(value)
	if err != nil || len(encoded) > 240<<10 {
		return nil, errSideToolsInvalid
	}
	return value, nil
}
