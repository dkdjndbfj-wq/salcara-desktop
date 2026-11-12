package hubclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/desktopcompanion"
	"salcara/bridge/internal/desktoplink"
	"salcara/bridge/internal/protocol"
)

const (
	maxSessions   = 100
	maxOpenEvents = 400
)

type commandGuardKey struct{}
type executionSourceKey struct{}

// commandGuard freezes the remote station/phone identity at SSE admission.
// Dispatch may wait on taskConfigMu while another command changes the active
// station; rechecking after that wait prevents an old A command from mutating
// workers that are already owned by B.
type commandGuard struct {
	identity   string
	deviceID   string
	bindingID  string
	phoneHash  string
	eventScope string
}

func withCommandGuard(ctx context.Context, guard commandGuard) context.Context {
	return context.WithValue(ctx, commandGuardKey{}, guard)
}

func withExecutionSource(ctx context.Context, env protocol.CommandEnvelope) context.Context {
	return context.WithValue(ctx, executionSourceKey{}, env)
}

func executionSourceFromContext(ctx context.Context) (protocol.CommandEnvelope, bool) {
	env, ok := ctx.Value(executionSourceKey{}).(protocol.CommandEnvelope)
	return env, ok
}

func commandGuardFromContext(ctx context.Context) (commandGuard, bool) {
	// This helper is intentionally only a presence check. The full identity is
	// still revalidated by validateCommandGuard while taskConfigMu is held.
	guard, ok := ctx.Value(commandGuardKey{}).(commandGuard)
	return guard, ok
}

func (c *Client) validateCommandGuard(ctx context.Context, typ string) error {
	guard, ok := ctx.Value(commandGuardKey{}).(commandGuard)
	if !ok || c.o.Store == nil {
		return nil
	}
	cfg := c.o.Store.Get()
	if cfg.DeviceID != guard.deviceID || config.RemoteIdentity(cfg) != guard.identity {
		return errors.New("连接已变化，请刷新后重试")
	}
	if typ != "device.ping" && (!c.commandPhoneMatches(cfg, guard.bindingID, guard.phoneHash, guard.eventScope) || !c.canUpload(cfg)) {
		return errors.New("手机绑定已失效，请重新扫码")
	}
	if typ != "device.ping" && !c.commandEventScopeValid(cfg, guard.eventScope) {
		return errors.New("手机绑定代次已改变，指令未继续执行")
	}
	return nil
}

// Dispatch executes one Command (PROTOCOL.md §3) and returns its result. The local console uses it too.
func (c *Client) Dispatch(ctx context.Context, cmd map[string]any) (result any, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if isLocalExecution(ctx) && IsDirectExecution(ctx) {
		if c.o.Store == nil {
			return nil, errors.New("直连授权已失效，请重新连接电脑")
		}
		if err := validateDirectBinding(ctx, c.o.Store.Get()); err != nil {
			return nil, err
		}
	}
	if err := validateProjectlessFields(cmd); err != nil {
		return nil, err
	}
	if handled, result, err := c.dispatchClaudeDesktopHistory(ctx, cmd); handled {
		return result, err
	}
	typ := str(cmd, "type")
	if c.o.Store != nil {
		if err := validateRemoteSettingsEpoch(ctx, typ, c.o.Store.Get()); err != nil {
			return nil, err
		}
	}
	if err := c.checkUpdateCommand(cmd); err != nil {
		return nil, err
	}
	if err := validateCooperationFields(cmd); err != nil {
		return nil, err
	}
	featureOpts, featureErr := commandTurnFeatures(cmd)
	if featureErr != nil {
		return nil, featureErr
	}
	if typ == "source.grant.acquire" {
		return c.acquireSourceGrant(ctx, str(cmd, "challengeId"))
	}
	if typ == "remote.connections.sync" {
		if len(cmd) != 1 {
			return nil, errors.New("连接同步参数无效")
		}
		return c.syncPhoneConnections(ctx)
	}
	taskLocked := false
	if typ == cooperationPrepareCommand || sessionManagementCommand(typ) || typ == "agents.api.set" || typ == "remote.station.switch" || typ == "remote.station.receipt" || typ == "session.start" || typ == "session.send" || typ == "session.continue" || typ == "session.steer" || typ == "session.interrupt" || typ == "desktop.session.send" || typ == "approval.respond" || typ == "desktop.approval.respond" || typ == "desktop.navigate" || typ == "attachment.put" || typ == "models.list" || typ == "features.list" || typ == "session.context" || typ == "session.compact" {
		c.taskConfigMu.Lock()
		taskLocked = true
		defer func() {
			if taskLocked {
				c.taskConfigMu.Unlock()
			}
		}()
		// Waiting for another task or API change does not extend this command's
		// deadline. Reject it before binding authority or touching a worker.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := c.validateCommandGuard(ctx, typ); err != nil {
			return nil, err
		}
		c.expireUpdateLocked()
		if err := c.checkUpdateCommand(cmd); err != nil {
			return nil, err
		}
		if c.updatePrepared {
			return nil, errors.New("程序正在更新，请稍后重试")
		}
		// The Hub performs an early policy check, but the source envelope must be
		// checked again after this mutex is acquired. A phone can race an API
		// change between SSE admission and dispatch; the second check binds the
		// actual command to the current vault snapshot.
		if env, ok := executionSourceFromContext(ctx); ok {
			if err := c.validateExecutionSource(env); err != nil {
				return nil, err
			}
			if err := c.bindExecutionStation(ctx, env); err != nil {
				return nil, err
			}
		}
	}
	cfg := config.Config{}
	if c.o.Store != nil {
		cfg = c.o.Store.Get()
	}
	if err := validateRemoteSettingsEpoch(ctx, typ, cfg); err != nil {
		return nil, err
	}
	if isLocalExecution(ctx) && IsDirectExecution(ctx) {
		if err := c.validateDirectAdmission(ctx, cfg, cmd); err != nil {
			return nil, err
		}
	}
	if env, ok := executionSourceFromContext(ctx); ok {
		if err := c.validateExecutionSourceFor(env, cfg); err != nil {
			return nil, err
		}
	}
	// Only the authenticated console may use the local applied source. Retain
	// the same immutable snapshot for model validation, execution and approvals.
	cfg = executionConfig(ctx, cfg)
	if isLocalExecution(ctx) {
		ctx = withWorkerSnapshot(ctx, cfg)
	} else if cfg.RemoteExecutionArmed && sourceGuardExecutionType(typ) {
		if _, guarded := commandGuardFromContext(ctx); guarded {
			ctx = withWorkerSnapshot(ctx, cfg)
		} else if _, sourced := executionSourceFromContext(ctx); sourced {
			ctx = withWorkerSnapshot(ctx, cfg)
		}
	}
	// A paired phone command must identify the API source that the Bridge will
	// actually execute with. Do not let a saved Codex/Claude config, a legacy
	// relay key, or a third-party reverse proxy become an implicit remote
	// credential merely because the tool happens to be installed. This check is
	// deliberately before any manager/desktop dispatch and also covers retries.
	if typ == cooperationPrepareCommand || sessionManagementCommand(typ) || sideQuestionCommand(typ) || typ == "session.start" || typ == "session.send" || typ == "session.continue" || typ == "session.steer" || typ == "desktop.session.send" || typ == "approval.respond" || typ == "desktop.approval.respond" || typ == "desktop.navigate" || typ == "features.list" || typ == "session.context" || typ == "session.compact" {
		var accountErr error
		if typ == "desktop.session.send" || typ == "desktop.approval.respond" || typ == "session.send" && str(cmd, "controlSurface") == "desktop" {
			accountErr = c.validateNativeExecutionAccountFor(ctx, cfg, cmd)
		} else {
			accountErr = c.validateExecutionAccountFor(ctx, cfg, str(cmd, "tool"), str(cmd, "sessionKey"), str(cmd, "accountId"))
		}
		if accountErr != nil {
			return nil, accountErr
		}
	}
	if err := c.validateManagedSession(ctx, cfg, str(cmd, "sessionKey")); err != nil {
		return nil, err
	}
	if err := c.validateExplorerSession(ctx, cfg, str(cmd, "sessionKey"), commandSurface(typ, cmd)); err != nil {
		return nil, err
	}
	finish, replayed, previous, ledgerErr := c.beginDurableExecution(ctx, cfg, cmd)
	if ledgerErr != nil || replayed {
		if ledgerErr == nil && replayed && typ == "session.revert" {
			encoded, _ := json.Marshal(previous)
			var saved protocol.LatestTurnRevertResult
			if json.Unmarshal(encoded, &saved) == nil && saved.Status == "unconfirmed" && saved.OperationID == str(cmd, "operationId") {
				return c.dispatchSessionRevert(ctx, cfg, cmd, true)
			}
		}
		return previous, ledgerErr
	}
	if finish != nil {
		defer func() {
			if err := finish(result, resultErr); err != nil {
				result = nil
				resultErr = commandDeliveryUncertainError{}
			}
		}()
	}
	attachments := c.attachmentsForConfig(ctx, cfg)
	if typ == "remote.station.switch" {
		sourceOnly, valid := stationSourceOnly(cmd)
		if !valid {
			return nil, errors.New("中转站切换参数无效")
		}
		model, err := explicitModelField(cmd)
		if err != nil {
			return nil, err
		}
		// Handover owns its two lock phases and repeats admission checks. Its
		// target network probe must not delay stop/approval on the source.
		c.taskConfigMu.Unlock()
		taskLocked = false
		return c.switchStation(ctx, stationSwitchCommand{
			TargetHubURL: str(cmd, "targetHubUrl"), TargetDeviceID: str(cmd, "targetDeviceId"), TargetComputerID: str(cmd, "targetComputerId"),
			Agent: str(cmd, "agent"), AccountID: str(cmd, "accountId"), Model: model, SessionKey: str(cmd, "sessionKey"), OperationID: str(cmd, "operationId"),
			SourceOnly: sourceOnly,
		})
	}
	if typ == "remote.station.receipt" {
		operationID := str(cmd, "operationId")
		if !stationSwitchOperationID(operationID) {
			return nil, errors.New("切换请求编号无效")
		}
		cfg := c.o.Store.Get()
		return map[string]any{
			"committed":   cfg.RemoteHandoverOperation == operationID,
			"operationId": cfg.RemoteHandoverOperation,
			"payloadHash": cfg.RemoteHandoverPayloadHash,
			"hubUrl":      config.NormalizeHubURL(cfg.EffectiveHubURL()),
			"deviceId":    cfg.DeviceID,
		}, nil
	}
	if typ == "device.ping" {
		// A transport probe must work before agent startup and must never open
		// config/files, inspect sessions, run a model, or expose anything else.
		receivedAt := nowMS()
		nonce, ok := cmd["nonce"].(string)
		if !ok || len(nonce) < 1 || len(nonce) > 64 {
			return nil, errors.New("无效的延迟探测 nonce，必须是 1 至 64 位字母、数字、下划线或连字符")
		}
		for _, ch := range []byte(nonce) {
			if !((ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '-') {
				return nil, errors.New("无效的延迟探测 nonce，必须是 1 至 64 位字母、数字、下划线或连字符")
			}
		}
		// receivedAt is diagnostic only. RTT is measured by the caller's local
		// monotonic clock, not by subtracting these two computers' wall clocks.
		return map[string]any{"nonce": nonce, "receivedAt": receivedAt}, nil
	}
	if typ == "attachment.put" {
		if raw, present := cmd["controlSurface"]; present && raw != "cli" {
			return nil, errors.New("agent_capability_unavailable: 当前控制方式不支持图片上传")
		}
		if raw, present := cmd["agent"]; present {
			if _, valid := raw.(string); !valid {
				return nil, errors.New("图片上传的 Agent 参数格式无效")
			}
		}
		if agentID := str(cmd, "agent"); agentID != "" {
			// The public Claude Desktop Code card deliberately selects Claude
			// Code. Chat/Cowork history remains read-only; shared API families
			// and native Codex do not create executor aliases here.
			if agentID == "claude-desktop" {
				agentID = "claude"
			}
			m := c.manager()
			if m == nil {
				return nil, errors.New("程序还在启动，请稍后再试")
			}
			a := m.Get(agentID)
			if err := requireAgentCapability(a, "imageInput"); err != nil {
				return nil, err
			}
			if err := validateAdapterCommand(a, cmd); err != nil {
				return nil, err
			}
			d, _ := cliAgentDescriptor(a)
			if !publishedRemoteCapabilities(a, d.Capabilities).ImageInput {
				return nil, errors.New("agent_capability_unavailable: 当前 Agent 未提供图片选项传输")
			}
		} else if _, remote := commandGuardFromContext(ctx); remote {
			return nil, errors.New("图片上传缺少 Agent 归属")
		}
		index, _ := cmd["index"].(float64)
		total, _ := cmd["total"].(float64)
		done, err := attachments.put(str(cmd, "id"), str(cmd, "mime"), int(index), int(total), str(cmd, "data"))
		if err != nil {
			return nil, err
		}
		return map[string]any{"complete": done}, nil
	}
	if typ == "attachment.read" {
		offset, _ := cmd["offset"].(float64)
		length, _ := cmd["length"].(float64)
		return attachments.read(str(cmd, "id"), str(cmd, "sessionKey"), int(offset), int(length))
	}
	if typ == "agents.api.set" {
		if isLocalExecution(ctx) && IsDirectExecution(ctx) {
			return c.setDirectAPI(ctx, cmd)
		}
		// Picks the API Bridge-run workers use for remote tasks. Keys stay here;
		// the original desktop tool configuration is not touched or restarted.
		agentID, account := str(cmd, "agent"), str(cmd, "accountId")
		model, err := explicitModelField(cmd)
		if err != nil {
			return nil, err
		}
		route, routeErr := c.routeForAgent(agentID)
		if routeErr != nil {
			return nil, routeErr
		}
		if err := c.requireExplicitModelCapability(agentID, model); err != nil {
			return nil, err
		}
		family, mirror := route.Family, agentID
		if c.o.Store == nil {
			return nil, errors.New("程序还在启动，请稍后再试")
		}
		stationEpoch := c.o.Store.Get().RemoteConnectionEpoch
		if err := c.checkRemoteAPIChange(ctx, family, str(cmd, "sessionKey")); err != nil {
			return nil, err
		}
		// Selecting a key needs no provider call. Ordinary APIs require catalogue
		// membership; official IDs use explicit policy, not picker entitlement.
		var selected config.LocalAccount
		if account != "" {
			snapshot := c.o.Store.Get()
			id, ok := AccountForHandle(snapshot, account)
			if !ok {
				return nil, errors.New("电脑上没有这个 API，请刷新后重选")
			}
			selected, _ = snapshot.LocalAccount(id)
			if !snapshot.RemoteHubAllowsAccount(snapshot.EffectiveHubURL(), selected) {
				return nil, errors.New("这个 API 尚未加入当前中转站，请在电脑端 API 设置中开启")
			}
			if strings.TrimSpace(model) != "" {
				if isOfficialModelAccount(selected) {
					if err := validateOfficialModel(selected, model); err != nil {
						return nil, err
					}
				} else {
					models, err := c.verifiedAPIModels(ctx, selected, false)
					if err != nil {
						return nil, err
					}
					if !containsKey(models, strings.TrimSpace(model)) {
						return nil, errors.New("这个 API 不支持该模型，请刷新后重选模型")
					}
				}
			}
		}
		// A provider directory can take seconds. Adapter notifications and local
		// tasks do not share taskConfigMu, so they can become busy during that
		// wait even though new Bridge commands are serialized.
		if err := c.checkRemoteAPIChange(ctx, family, str(cmd, "sessionKey")); err != nil {
			return nil, err
		}
		if err := c.requireExplicitModelCapability(agentID, model); err != nil {
			return nil, err
		}
		if c.o.Store.Get().RemoteConnectionEpoch != stationEpoch {
			return nil, errors.New("连接方式或中转站已变化，请刷新后重选 API")
		}
		if err := c.o.Store.Update(func(cfg *config.Config) error {
			if err := validateRemoteSettingsEpoch(ctx, typ, *cfg); err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if cfg.RemoteConnectionEpoch != stationEpoch {
				return errors.New("连接方式或中转站已变化，请刷新后重选 API")
			}
			if env, ok := executionSourceFromContext(ctx); ok {
				if err := c.validateExecutionSourceFor(env, *cfg); err != nil {
					return err
				}
			}
			id := ""
			if account != "" {
				var ok bool
				if id, ok = AccountForHandle(*cfg, account); !ok {
					return errors.New("电脑上没有这个 API，请刷新后重选")
				}
				current, exists := cfg.LocalAccount(id)
				if !exists || id != selected.ID || catalogAccountFingerprint(current) != catalogAccountFingerprint(selected) {
					return errors.New("API 已更换，请刷新后重选")
				}
				if !cfg.RemoteHubAllowsAccount(cfg.EffectiveHubURL(), current) {
					return errors.New("这个 API 尚未加入当前中转站，请在电脑端 API 设置中开启")
				}
			}
			if err := cfg.SetRemoteAPIForAgent(family, id, model); err != nil {
				return err
			}
			if id != "" {
				// The phone's choice is also the computer's choice: the 模型 page shows
				// it on the matching tool cards, and the next「打开」uses the same key.
				mirrorRemoteChoice(cfg, mirror, id, model)
			}
			return nil
		}); err != nil {
			return nil, err
		}
		if selected.ID != "" && strings.TrimSpace(model) == "" {
			// Switching back to a previously used account still needs a fresh
			// catalog. Persistent Models are picker hints, not verified access.
			c.invalidateAPIModels(selected.ID)
		}
		return c.agentStatus(ctx), nil
	}
	if typ == "agents.status" {
		// Confirmation metadata must be available before a CLI manager starts.
		// This does not select/apply an API, start a tool, or certify upstream health.
		status := c.agentStatus(ctx)
		if c.o.Store != nil {
			if err := validateRemoteSettingsEpoch(ctx, typ, c.o.Store.Get()); err != nil {
				return nil, err
			}
		}
		return status, nil
	}
	if typ == "workspace.projects" {
		return c.dispatchWorkspaceProjects(ctx, cmd)
	}
	if typ == "workspace.explorer" {
		return c.dispatchWorkspaceExplorer(ctx, cmd)
	}
	if typ == "workspace.project.preview" {
		return c.dispatchWorkspaceProjectPreview(ctx, cmd)
	}
	if typ == "workspace.project.files.list" || typ == "workspace.project.files.read" {
		return c.dispatchWorkspaceProjectFiles(ctx, cmd)
	}
	if typ == "workspace.artifacts" {
		return c.dispatchWorkspaceArtifacts(ctx, cmd)
	}
	if typ == "workspace.git.read" || typ == "workspace.git.mutate" {
		return c.dispatchWorkspaceGit(ctx, cmd)
	}
	if typ == "workspace.files.mutate" {
		return c.dispatchWorkspaceFileMutation(ctx, cmd)
	}
	if typ == "workspace.files.write" {
		return c.dispatchWorkspaceFileWrite(ctx, cmd)
	}
	if strings.HasPrefix(typ, "workspace.terminal.") {
		return c.dispatchWorkspaceTerminal(ctx, cmd)
	}
	if strings.HasPrefix(typ, "workspace.preview.") {
		return c.dispatchWorkspacePreview(ctx, cmd)
	}
	if typ == "workspace.files.list" || typ == "workspace.files.read" {
		if str(cmd, "controlSurface") == "cli" {
			m := c.manager()
			if m == nil {
				return nil, errors.New("程序还在启动，请稍后再试")
			}
			a, _, err := agentFor(m, str(cmd, "sessionKey"))
			if err != nil {
				return nil, err
			}
			if err := requireAgentCapability(a, "read"); err != nil {
				return nil, err
			}
		}
		return c.dispatchWorkspaceFiles(ctx, cmd)
	}
	if strings.HasPrefix(typ, "desktop.") && typ != "desktop.navigate" {
		return c.dispatchDesktop(ctx, cmd)
	}
	if surface, present := cmd["controlSurface"]; present {
		s, ok := surface.(string)
		if ok && s == "desktop" && typ == "session.send" {
			return c.dispatchNativeSend(ctx, cmd)
		}
		if ok && s == "desktop" && typ == "models.list" {
			return c.dispatchNativeModels(ctx, cmd)
		}
		want := "cli"
		if typ == "desktop.navigate" {
			want = "desktop"
		}
		if !ok || s != want {
			return nil, errors.New("请求的控制方式不受支持；不会自动改用 CLI")
		}
	}
	m := c.manager()
	if m == nil && typ != "projects.list" {
		return nil, errors.New("程序还在启动，请稍后再试")
	}
	switch typ {
	case "session.side-question.tools":
		return c.dispatchSideQuestionTools(ctx, cfg, cmd)
	case "session.side-question.catalog", "session.side-question.ask", "session.side-question.open", "session.side-question.read", "session.side-question.send", "session.side-question.delete":
		return c.dispatchSideQuestion(ctx, cfg, cmd)
	case "session.workflows", "session.workflow":
		return c.dispatchSessionWorkflows(ctx, cfg, cmd)
	case "session.actions", "session.action", "sessions.archived":
		return c.dispatchSessionActions(ctx, cfg, cmd)
	case "session.context", "session.compact":
		return c.dispatchContext(ctx, cfg, cmd)
	case "features.list":
		return c.dispatchFeatures(ctx, m, cfg, cmd)
	case "sessions.list":
		page, err := dispatchSessionsPage(ctx, m, cmd)
		if err == nil {
			page = c.projectProjectlessResult(ctx, page)
		}
		return page, err

	case "desktop.navigate":
		key := str(cmd, "sessionKey")
		if _, err := desktoplink.CodexURL(key); err != nil && !desktoplink.ClaudeKey(key) {
			return nil, err
		}
		if c.o.NavigateDesktop == nil {
			return nil, errors.New("此 Bridge 未启用桌面会话定位，请升级电脑端")
		}
		a, id, err := agentFor(m, key)
		if err != nil {
			return nil, err
		}
		// Verify an existing chat before dispatching its fixed deep link. Never create/resume/send.
		info, _, err := a.Open(ctx, id)
		if err != nil {
			return nil, err
		}
		if info.SessionKey != key {
			return nil, errors.New("会话身份不一致，已取消桌面定位")
		}
		navCtx := ctx
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if desktoplink.ClaudeKey(key) {
			starter, ok := a.(interface{ ClaimDesktopImport(string) bool })
			if !ok || !starter.ClaimDesktopImport(id) {
				return nil, desktoplink.ErrClaudeSessionNavigationUnavailable
			}
			// Claude Desktop only lists sessions it created itself; a session the
			// phone started gets at most one import attempt through claude://resume.
			navCtx = context.WithValue(ctx, importSessionKey{}, true)
		}
		if err := c.o.NavigateDesktop(navCtx, key); err != nil {
			return nil, err
		}
		return map[string]any{"requested": true, "controlSurface": "desktop", "action": "navigate", "sessionKey": key}, nil

	case "session.open":
		ticket := c.beginSideQuestionHistory(str(cmd, "sessionKey"))
		result, err := dispatchHistoryPageWithAttachments(ctx, m, cmd, attachments)
		if err == nil {
			result = c.projectProjectlessResult(ctx, result)
			c.rememberSideQuestionHistory(ctx, cfg, cmd, result, ticket)
		}
		return result, err

	case "session.start":
		tool := str(cmd, "tool")
		a := m.Get(tool)
		if a == nil {
			return nil, fmt.Errorf("不支持的工具：%s", tool)
		}
		if err := validateAdapterCommand(a, cmd); err != nil {
			return nil, err
		}
		workspaceMode, err := commandWorkspaceMode(cmd)
		if err != nil {
			return nil, err
		}
		if err := requireTurnFeatures(a, featureOpts); err != nil {
			return nil, err
		}
		model, effort, attachmentIDs, err := turnFields(cmd)
		if err != nil {
			return nil, err
		}
		if err := requireTurnCapabilities(a, model, effort, attachmentIDs, true); err != nil {
			return nil, err
		}
		_, optionsSupported := a.(turnOptionsStarter)
		if !optionsSupported && (effort != "" || len(attachmentIDs) != 0) {
			return nil, errors.New("agent_capability_unavailable: 当前 Agent 不能保留推理或图片选项，任务未启动")
		}
		rawPrompt, hasPrompt := cmd["prompt"]
		prompt, validPrompt := rawPrompt.(string)
		prompt = strings.TrimSpace(prompt)
		if hasPrompt && !validPrompt || prompt == "" && len(attachmentIDs) == 0 && len(featureOpts.ContextReferences) == 0 {
			return nil, errors.New("请输入任务内容")
		}
		if !protocol.ValidModelEffort(effort) {
			return nil, errors.New("无效的推理强度")
		}
		// A saved project needs no history lookup or startup of unrelated tools.
		projectless := cmd["projectless"] == true
		cwd := ""
		if !projectless {
			explorerGrant, grantErr := explorerGrantForCwd(cfg, str(cmd, "cwd"))
			if grantErr != nil {
				return nil, grantErr
			}
			if c.isManagedCwd(str(cmd, "cwd")) {
				return nil, ErrNotAllowed
			}
			if explorerGrant {
				cwd, err = workspaceConfiguredRoot(cfg, str(cmd, "cwd"))
			} else {
				cwd, err = CheckCwd(str(cmd, "cwd"), cfg.Projects)
				if errors.Is(err, ErrNotAllowed) {
					cwd, err = CheckCwd(str(cmd, "cwd"), allowedProjects(ctx, m, cfg.Projects))
				}
			}
			if err != nil {
				return nil, err
			}
			if c.isManagedCwd(cwd) {
				return nil, ErrNotAllowed
			}
			if _, err := explorerGrantForCwd(cfg, cwd); err != nil {
				return nil, err
			}
		}
		if !projectless && workspaceGuardedAgent(a) {
			if err := c.checkWorkspaceAdmission(ctx, m, cwd, ""); err != nil {
				return nil, err
			}
		}
		permissionMode, err := commandPermissionMode(cmd, a, cfg)
		if err != nil {
			return nil, err
		}
		if permissionMode != "" && !optionsSupported {
			return nil, errors.New("permission_mode_unsupported: Agent 未提供权限选项传输")
		}
		approval := cfg.Approval
		if a.ID() == "claude" && approval == config.ApprovalAutoAll {
			approval = config.ApprovalAsk
		}
		if raw, present := cmd["approval"]; present {
			value, valid := raw.(string)
			if !valid || !config.ValidApproval(value) {
				return nil, errors.New("permission_mode_invalid: 审批方式无效")
			}
			approval = value
		}
		if !config.ValidApproval(approval) {
			approval = config.ApprovalAsk
		}
		if permissionMode != "" {
			approval = config.ApprovalAsk
		}
		if approval == config.ApprovalAutoAll && !cfg.AllowPhoneAutoAll {
			return nil, errors.New("电脑端没有允许手机使用「全部自动」，请在电脑的「设置」里打开，或改用其他审批方式")
		}
		managed, err := c.validateManagedModel(ctx, cfg, c.apiFamilyForAgent(a.ID()), model)
		if err != nil {
			return nil, err
		}
		if model == "" && !managed && requireAgentCapability(a, "modelOverride") == nil {
			if a.ID() == "codex" {
				model = cfg.CodexModel
			} else if a.ID() == "claude" {
				model = cfg.ClaudeModel
			}
		}
		if err := validateModelEffort(ctx, cfg, a, model, effort); err != nil {
			return nil, err
		}
		if durableRemoteContext(ctx) {
			if err := c.ensureCommandAttachments(ctx, cfg, attachments, cmd); err != nil {
				return nil, err
			}
		}
		releaseImages, err := attachments.lease(attachmentIDs)
		if err != nil {
			return nil, err
		}
		defer releaseImages()
		images, err := attachments.resolve(attachmentIDs)
		if err != nil {
			return nil, err
		}
		if err := validateModelImages(ctx, cfg, a, model, "", len(images)); err != nil {
			return nil, err
		}
		managedKey := ""
		if projectless {
			var accepted string
			cwd, managedKey, accepted, err = c.prepareProjectless(ctx, cmd)
			if err != nil {
				return nil, err
			}
			if accepted != "" {
				return map[string]any{"sessionKey": accepted}, nil
			}
			if workspaceGuardedAgent(a) {
				if err := c.checkWorkspaceAdmission(ctx, m, cwd, ""); err != nil {
					return nil, err
				}
			}
		}
		var id string
		opts := agents.TurnOptions{WorkspaceMode: workspaceMode, CollaborationMode: featureOpts.CollaborationMode, Skills: featureOpts.Skills, ContextReferences: featureOpts.ContextReferences, PermissionMode: permissionMode, Model: model, Effort: effort, Images: images, Attachments: attachments.refs(attachmentIDs),
			Desktop: a.ID() == "claude" && str(cmd, "entrypoint") == "claude-desktop"}
		opts.ValidatePermission = c.permissionConsentGuard(cmd, a, approval)
		if (a.ID() == "codex" || a.ID() == "claude") && desktopcompanion.ValidNativeOperationID(str(cmd, "operationId")) {
			opts.ClientUserMessageID = str(cmd, "operationId")
		}
		if opts.ValidatePermission != nil {
			if err := opts.ValidatePermission(); err != nil {
				return nil, err
			}
		}
		finishWorkspaceStart := c.beginWorkspaceStart()
		defer finishWorkspaceStart("", "")
		if projectless {
			if err := c.markProjectlessStarting(ctx, managedKey); err != nil {
				return nil, err
			}
		} else if c.o.Store != nil {
			if _, err := explorerGrantForCwd(c.o.Store.Get(), cwd); err != nil {
				return nil, err
			}
		}
		if starter, ok := a.(turnOptionsStarter); ok {
			id, err = starter.StartWithOptions(ctx, cwd, prompt, approval, opts)
		} else {
			id, err = a.Start(ctx, cwd, prompt, model, approval)
		}
		if err != nil {
			if id != "" || projectless { // A native thread may exist; a new UUID can duplicate it.
				if id != "" {
					finishWorkspaceStart(a.ID()+":"+id, cwd)
				}
				return nil, newTaskUncertainError{}
			}
			return nil, err
		}
		sessionKey := a.ID() + ":" + id
		finishWorkspaceStart(sessionKey, cwd)
		if !agents.SessionBelongsTo(a, sessionKey, protocol.SessionInfo{SessionKey: sessionKey}) {
			return nil, newTaskUncertainError{}
		}
		if projectless {
			if err := c.bindProjectless(managedKey, sessionKey); err != nil {
				return nil, newTaskUncertainError{}
			}
		}
		if modes, ok := m.(interface {
			RememberWorkspaceMode(string, protocol.WorkspaceMode) error
		}); ok {
			if err := modes.RememberWorkspaceMode(sessionKey, workspaceMode); err != nil {
				// The worker already accepted this turn. Do not turn a metadata
				// write failure into a retryable session.start failure.
			}
		}
		if err := attachments.bind(attachmentIDs, sessionKey); err != nil {
			return nil, newTaskUncertainError{}
		}
		if projectless && !c.projectlessStillOwned(ctx, managedKey) {
			return nil, newTaskUncertainError{}
		}
		return map[string]any{"sessionKey": sessionKey}, nil

	case cooperationPrepareCommand:
		return c.prepareCooperation(ctx, cfg, cmd)

	case "session.send":
		a, id, err := agentFor(m, str(cmd, "sessionKey"))
		if err != nil {
			return nil, err
		}
		if err := validateAdapterCommand(a, cmd); err != nil {
			return nil, err
		}
		if workspaceGuardedAgent(a) {
			if err := c.checkSessionWorkspace(ctx, m, a, id, str(cmd, "sessionKey")); err != nil {
				return nil, err
			}
		}
		if err := requireTurnFeatures(a, featureOpts); err != nil {
			return nil, err
		}
		claudeApproval, err := commandClaudeApproval(cmd, a, cfg)
		if err != nil {
			return nil, err
		}
		permissionMode, err := commandPermissionMode(cmd, a, cfg)
		if err != nil {
			return nil, err
		}
		model, effort, attachmentIDs, err := turnFields(cmd)
		if err != nil {
			return nil, err
		}
		if err := requireTurnCapabilities(a, model, effort, attachmentIDs, false); err != nil {
			return nil, err
		}
		_, optionsSupported := a.(turnOptionsSender)
		if !optionsSupported && (permissionMode != "" || model != "" || effort != "" || len(attachmentIDs) != 0) {
			return nil, errors.New("agent_capability_unavailable: 当前 Agent 不能保留模型、推理或图片选项，消息未发送")
		}
		rawText, hasText := cmd["text"]
		text, validText := rawText.(string)
		text = strings.TrimSpace(text)
		if hasText && !validText || text == "" && len(attachmentIDs) == 0 && len(featureOpts.ContextReferences) == 0 {
			return nil, errors.New("请输入内容")
		}
		// The caller freezes its chosen executor with the delivery receipt.
		// Explicit CLI (and older missing-surface requests) never switch to a
		// desktop lease that happens to be available when a retry arrives.
		if durableRemoteContext(ctx) {
			if err := c.ensureCommandAttachments(ctx, cfg, attachments, cmd); err != nil {
				return nil, err
			}
		}
		releaseImages, err := attachments.lease(attachmentIDs)
		if err != nil {
			return nil, err
		}
		defer releaseImages()
		images, err := attachments.resolve(attachmentIDs)
		if err != nil {
			return nil, err
		}
		opts := agents.TurnOptions{CollaborationMode: featureOpts.CollaborationMode, Skills: featureOpts.Skills, ContextReferences: featureOpts.ContextReferences, PermissionMode: permissionMode, ClaudeApproval: claudeApproval, Model: model, Effort: effort, Images: images, Attachments: attachments.refs(attachmentIDs)}
		opts.ValidatePermission = c.permissionConsentGuard(cmd, a)
		if err := c.admitCooperation(ctx, cfg, cmd, &opts); err != nil {
			return nil, err
		}
		// Codex reports clientId; Claude's Bridge-owned echo and exact skill
		// receipt retain it. Equal-text messages cannot consume a pending echo.
		// This is correlation, never sender provenance.
		if (a.ID() == "codex" || a.ID() == "claude") && desktopcompanion.ValidNativeOperationID(str(cmd, "operationId")) {
			opts.ClientUserMessageID = str(cmd, "operationId")
		}
		if len(opts.Model) > 200 || strings.ContainsAny(opts.Model, "\r\n\x00") {
			return nil, errors.New("模型名称无效")
		}
		if _, err := c.validateManagedModel(ctx, cfg, c.apiFamilyForAgent(a.ID()), opts.Model); err != nil {
			return nil, err
		}
		if err := validateModelEffort(ctx, cfg, a, opts.Model, opts.Effort); err != nil {
			return nil, err
		}
		if err := validateModelImages(ctx, cfg, a, opts.Model, id, len(opts.Images)); err != nil {
			return nil, err
		}
		if err := attachments.bind(attachmentIDs, str(cmd, "sessionKey")); err != nil {
			return nil, err
		}
		if sender, ok := a.(turnOptionsSender); ok && (opts.HasFeatures() || len(opts.ContextReferences) > 0 || opts.ValidatePermission != nil || opts.PermissionMode != "" || opts.ClaudeApproval != "" || opts.Model != "" || opts.Effort != "" || len(opts.Images) > 0 || opts.ClientUserMessageID != "") {
			err = sender.SendWithOptions(ctx, id, text, opts)
		} else if len(opts.Images) > 0 {
			return nil, errors.New("这个工具暂不支持图片")
		} else {
			err = a.Send(ctx, id, text)
		}
		if err != nil {
			return nil, err
		}
		if opts.CrossTaskOrigin != nil {
			return map[string]any{"via": "background", "receiptId": opts.CrossTaskOrigin.ReceiptID}, nil
		}
		return map[string]any{"via": "background"}, nil

	case "session.revert":
		return c.dispatchSessionRevert(ctx, cfg, cmd)
	case "session.steer":
		return c.dispatchSteer(ctx, cmd)

	case "session.continue":
		// Continue is a new turn in the same conversation. It is not a Goal
		// resume and it cannot be used to restart a running/unknown turn.
		a, id, err := agentFor(m, str(cmd, "sessionKey"))
		if err != nil {
			return nil, err
		}
		if err := validateAdapterCommand(a, cmd); err != nil {
			return nil, err
		}
		if workspaceGuardedAgent(a) {
			if err := c.checkSessionWorkspace(ctx, m, a, id, str(cmd, "sessionKey")); err != nil {
				return nil, err
			}
		}
		if err := requireTurnFeatures(a, featureOpts); err != nil {
			return nil, err
		}
		if surface := commandSurface(typ, cmd); surface != "cli" {
			return nil, errors.New("继续操作只支持 CLI 会话；不会自动改用桌面接口")
		}
		if err := requireAgentCapability(a, "continue"); err != nil {
			return nil, err
		}
		expected := str(cmd, "expectedTurnId")
		operation := str(cmd, "operationId")
		if !agents.ValidTurnID(expected) {
			return nil, agents.ErrExpectedTurnRequired
		}
		if !agents.ValidControlOperationID(operation) {
			return nil, errors.New("turn_control_operation_required: 操作标识无效")
		}
		text, ok := cmd["text"].(string)
		if !ok || strings.TrimSpace(text) == "" || len(text) > 1<<20 {
			return nil, errors.New("请输入要继续的内容")
		}
		model, effort, attachmentIDs, err := turnFields(cmd)
		if err != nil {
			return nil, err
		}
		// A continuation is intentionally text-only. A phone image starts a
		// normal new turn so the terminal CAS is never mixed with upload state.
		if len(attachmentIDs) != 0 {
			return nil, errors.New("继续操作暂不支持图片；请发送一条新消息")
		}
		if model != "" || effort != "" {
			if err := requireAgentCapability(a, "modelOverride"); err != nil {
				return nil, err
			}
		}
		if err := c.requireExplicitModelCapability(a.ID(), model); err != nil {
			return nil, err
		}
		permissionMode, err := commandPermissionMode(cmd, a, cfg)
		if err != nil {
			return nil, err
		}
		claudeApproval, err := commandClaudeApproval(cmd, a, cfg)
		if err != nil {
			return nil, err
		}
		if _, err := c.validateManagedModel(ctx, cfg, c.apiFamilyForAgent(a.ID()), model); err != nil {
			return nil, err
		}
		if err := validateModelEffort(ctx, cfg, a, model, effort); err != nil {
			return nil, err
		}
		continuer, ok := a.(agents.TerminalContinuer)
		if !ok {
			return nil, errors.New("agent_capability_unavailable: 当前 Agent 未提供安全的继续操作")
		}
		opts := agents.TurnOptions{CollaborationMode: featureOpts.CollaborationMode, Skills: featureOpts.Skills, ContextReferences: featureOpts.ContextReferences, PermissionMode: permissionMode, ClaudeApproval: claudeApproval, Model: model, Effort: effort}
		if a.ID() == "codex" || a.ID() == "claude" {
			opts.ClientUserMessageID = operation
		}
		opts.ValidatePermission = c.permissionConsentGuard(cmd, a)
		result, err := continuer.ContinueWithOptions(ctx, id, text, expected, operation, opts)
		if err != nil {
			return nil, err
		}
		return result, nil

	case "session.interrupt":
		a, id, err := agentFor(m, str(cmd, "sessionKey"))
		if err != nil {
			return nil, err
		}
		if err := validateAdapterCommand(a, cmd); err != nil {
			return nil, err
		}
		if err := requireAgentCapability(a, "preciseInterrupt"); err != nil {
			return nil, err
		}
		expected := str(cmd, "expectedTurnId")
		operation := str(cmd, "operationId")
		if !agents.ValidTurnID(expected) {
			return nil, agents.ErrExpectedTurnRequired
		}
		if !agents.ValidControlOperationID(operation) {
			return nil, errors.New("turn_control_operation_required: 操作标识无效")
		}
		if surface := commandSurface(typ, cmd); surface != "cli" {
			return nil, errors.New("停止操作只支持 CLI 会话；不会自动改用桌面接口")
		}
		interrupter, ok := a.(agents.PreciseInterrupter)
		if !ok {
			return nil, errors.New("agent_capability_unavailable: 当前 Agent 未提供安全的精确停止操作")
		}
		result, err := interrupter.InterruptTurn(ctx, id, expected, operation)
		if err != nil {
			return nil, err
		}
		return result, nil

	case "approval.respond":
		aid, decision := str(cmd, "approvalId"), str(cmd, "decision")
		if decision != "allow" && decision != "allow_session" && decision != "deny" {
			return nil, errors.New("无效的审批结果")
		}
		var answers map[string][]string
		if raw, present := cmd["answers"]; present {
			data, err := json.Marshal(raw)
			if err != nil || len(data) > 64<<10 {
				return nil, errors.New("回答格式无效或过长")
			}
			var fields map[string]json.RawMessage
			if json.Unmarshal(data, &fields) != nil || fields == nil {
				return nil, errors.New("回答格式无效或过长")
			}
			answers = make(map[string][]string, len(fields))
			for id, field := range fields {
				var values []json.RawMessage
				if json.Unmarshal(field, &values) != nil || values == nil {
					return nil, errors.New("回答格式无效或过长")
				}
				answers[id] = make([]string, len(values))
				for i, value := range values {
					if len(value) == 0 || value[0] != '"' || json.Unmarshal(value, &answers[id][i]) != nil {
						return nil, errors.New("回答格式无效或过长")
					}
				}
			}
		}
		response := agents.ApprovalResponse{Decision: decision, Message: str(cmd, "message"), Answers: answers}
		directApproval := isLocalExecution(ctx) && IsDirectExecution(ctx)
		capability := "approval"
		if answers != nil {
			capability = "questions"
		}
		if _, remote := commandGuardFromContext(ctx); !remote || !cfg.RemoteExecutionArmed {
			if capability == "questions" {
				capability = "localQuestions"
			} else {
				capability = "localApproval"
			}
		}
		if sessionKey := str(cmd, "sessionKey"); sessionKey != "" {
			a, _, err := agentFor(m, sessionKey)
			if err != nil {
				return nil, err
			}
			if err := validateAdapterCommand(a, cmd); err != nil {
				return nil, err
			}
			if err := requireAgentCapability(a, capability); err != nil {
				return nil, err
			}
			answered := false
			if _, remote := commandGuardFromContext(ctx); directApproval || remote && cfg.RemoteExecutionArmed {
				if _, typed := a.(agents.TypedApprovalResponder); typed {
					response.OperationID = str(cmd, "operationId")
					response.By = "phone"
					if isLocalExecution(ctx) && !IsDirectExecution(ctx) {
						response.By = "desktop"
					}
				}
				answered, err = agents.RespondForSessionResult(ctx, a, sessionKey, aid, response)
				if err != nil {
					return nil, err
				}
			} else {
				if _, typed := a.(agents.TypedApprovalResponder); typed {
					response.OperationID = str(cmd, "operationId")
					response.By = "desktop"
					answered, err = agents.RespondForSessionResult(ctx, a, sessionKey, aid, response)
					if err != nil {
						return nil, err
					}
				} else {
					answered = agents.RespondWithAnswers(a, aid, response)
				}
			}
			if answered {
				return map[string]any{}, nil
			}
			return nil, errors.New("请求已处理、过期或不属于这个会话，请检查后重试")
		}
		// Older local-console callers may not have a session key. A paired phone
		// command is required to include one by the source guard, so this fallback
		// cannot cross-route a remote approval.
		if _, remote := commandGuardFromContext(ctx); directApproval || remote && c.o.Store != nil && c.o.Store.Get().RemoteExecutionArmed {
			return nil, errors.New("远程审批缺少会话归属")
		}
		for _, a := range m.Agents() {
			if requireAgentCapability(a, capability) != nil {
				continue
			}
			if typed, ok := a.(interface {
				RespondWithAnswersResult(context.Context, string, agents.ApprovalResponse) (bool, error)
			}); ok {
				response.OperationID, response.By = str(cmd, "operationId"), "desktop"
				answered, err := typed.RespondWithAnswersResult(ctx, aid, response)
				if err != nil {
					return nil, err
				}
				if answered {
					return map[string]any{}, nil
				}
				continue
			}
			if agents.RespondWithAnswers(a, aid, response) {
				return map[string]any{}, nil
			}
		}
		return nil, errors.New("请求已处理、过期或回答格式不正确，请检查后重试")

	case "projects.list":
		// Allowed folders first, then folders of existing sessions (newest first),
		// like the workspace picker in Codex.
		var p []protocol.Project
		if m != nil {
			p = allowedProjects(ctx, m, cfg.Projects)
		} else {
			p = append(p, cfg.Projects...)
		}
		visible := p[:0]
		for _, project := range p {
			if !c.isManagedCwd(project.Path) {
				visible = append(visible, project)
			}
		}
		p = visible
		if p == nil {
			p = []protocol.Project{}
		}
		return map[string]any{"projects": p, "capabilities": c.Device(ctx).Capabilities}, nil

	case "models.list":
		if err := requireAgentCapability(m.Get(str(cmd, "tool")), "models"); err != nil {
			return nil, err
		}
		// The selected API's upstream catalog: the phone can switch to any of these
		// per message (other families are converted locally by the gateway).
		if c.o.Store != nil {
			cfg := executionConfig(ctx, c.o.Store.Get())
			family := c.apiFamilyForAgent(str(cmd, "tool"))
			if _, _, err := managedModelAccount(cfg, family); err != nil {
				return nil, err
			}
			a, ok := remoteModelAccount(cfg, family)
			if ok && a.ID != "" {
				vault, exists := cfg.LocalAccount(a.ID)
				if !exists {
					return nil, errors.New("API 已删除，请重新选择 API")
				}
				refresh, _ := cmd["refresh"].(bool)
				var models []string
				var capabilities map[string]protocol.ModelCapability
				nativeSource := ""
				var err error
				if isOfficialModelAccount(vault) {
					var catalog protocol.ModelCatalog
					catalog, err = c.OfficialModelCatalog(ctx, vault, refresh)
					models, capabilities = catalog.Models, catalog.ModelCapabilities
					for _, capability := range capabilities {
						nativeSource = capability.Source
						break
					}
					if nativeSource == "" {
						nativeSource = OfficialNativeCatalogSource
					}
				} else {
					models, err = c.verifiedAPIModels(ctx, vault, refresh)
					capabilities = protocol.UnknownModelCapabilities(models, "relay-model-list")
				}
				if err != nil {
					return nil, err
				}
				a.Models = models
				currentCfg := executionConfig(ctx, c.o.Store.Get())
				current, exists := remoteModelAccount(currentCfg, family)
				if !exists || current.ID != a.ID || config.AccountFingerprint(current) != config.AccountFingerprint(a) {
					return nil, errors.New("API 已更换，请重新读取模型")
				}
				result := map[string]any{"models": apiModels(a), "modelCapabilities": capabilities, "api": safeConfirmationLabel(current.Name, "已命名 API", currentCfg)}
				if nativeSource != "" {
					result["nativeSource"] = nativeSource
				}
				return result, nil
			}
		}
		models := []string{}
		if a := m.Get(str(cmd, "tool")); a != nil {
			if reader, ok := a.(agents.ModelCatalogReader); ok {
				catalog, err := reader.ModelCatalog(ctx)
				if err != nil {
					return nil, errors.New("读取模型列表失败，请刷新后重试")
				}
				return catalog, nil
			}
			if ms := a.Models(ctx); ms != nil {
				models = ms
			}
		}
		return map[string]any{"models": models, "modelCapabilities": protocol.UnknownModelCapabilities(models, "unknown")}, nil
	}
	return nil, fmt.Errorf("不支持的命令：%s", typ)
}

// validateExecutionAccount binds remote execution to an explicit Salcara
// account selection. A running Bridge never falls back to whatever another
// program wrote into a native tool config; a phone command must resolve its
// handle to a local Salcara API card.
func (c *Client) validateExecutionAccount(ctx context.Context, agent, sessionKey, handle string) error {
	if c.o.Store == nil {
		// Some embedded desktop-only callers construct a Client without a
		// config store. They do not have a remote account source to validate;
		// production Bridge instances always provide the store and runtime lease.
		return nil
	}
	return c.validateExecutionAccountFor(ctx, c.o.Store.Get(), agent, sessionKey, handle)
}

func (c *Client) validateExecutionAccountFor(ctx context.Context, cfg config.Config, agent, sessionKey, handle string) error {
	if !cfg.RemoteExecutionArmed {
		// Unit/legacy callers that have not started a Bridge process do not have
		// a runtime lease to prove source ownership. Production startup arms this
		// flag before accepting Hub commands, so real remote execution remains
		// fail-closed while old in-process adapters keep their compatibility tests.
		return nil
	}
	// Console calls Dispatch directly. They do not carry the paired-phone
	// command guard and therefore must continue using the local tool session,
	// not be mistaken for a remote execution request.
	if _, remote := commandGuardFromContext(ctx); !remote {
		return nil
	}
	family := c.apiFamilyForAgent(strings.TrimSpace(agent))
	if strings.Contains(sessionKey, ":") {
		sessionFamily := c.apiFamilyForAgent(strings.SplitN(sessionKey, ":", 2)[0])
		if family != "" && family != sessionFamily {
			return errors.New("远程任务的 Agent 与会话不一致")
		}
		family = sessionFamily
	}
	if strings.TrimSpace(handle) == "" {
		return errors.New("远程任务缺少 API 来源，请刷新后重新选择")
	}
	accountID, ok := AccountForHandle(cfg, handle)
	if !ok {
		// A handle can only resolve to an API card saved in Salcara. This is the
		// fail-closed boundary that prevents adopting a third-party native login
		// or reverse proxy when the vault is empty/unmanaged.
		return errors.New("当前 Agent 没有由 Salcara 明确启用的 API；请先在电脑端应用 API，不能接管外部反代配置")
	}
	selected, ok := cfg.LocalAccount(accountID)
	if !ok {
		return errors.New("远程任务的 API 来源已删除，请重新选择")
	}
	if handle != APIHandle(cfg, selected.ID) {
		return errors.New("远程任务的 API 来源已变化，请重新选择；未使用其他 Key")
	}
	effective, managed, err := managedModelAccount(cfg, family)
	if err != nil {
		return err
	}
	if !managed || effective.ID != selected.ID {
		return errors.New("远程任务的 API 与当前执行来源不一致，请重新选择；未使用其他 Key")
	}
	return nil
}

// Native commands route before manager lookup. Do not start/resume/open/send a
// CLI process, even when a native tool returns an error or no current history.
func (c *Client) dispatchNativeSend(ctx context.Context, cmd map[string]any) (any, error) {
	ctx, finishAdmission := c.nativeAdmissionContext(ctx, cmd)
	defer finishAdmission()
	if _, present := cmd["approval"]; present {
		return nil, errors.New("permission_mode_unsupported: 原生桌面接口不支持 Claude 审批方式")
	}
	if _, present := cmd["permissionMode"]; present {
		return nil, errors.New("permission_mode_unsupported: 原生桌面接口不支持手机覆盖权限，请在电脑端设置")
	}
	if c.o.Desktop == nil {
		return nil, desktopcompanion.ErrActivationRequired
	}
	key := str(cmd, "sessionKey")
	if _, err := desktoplink.CodexURL(key); err != nil {
		return nil, fmt.Errorf("%w；消息未发送，不会自动改用 CLI", desktopcompanion.ErrDesktopScope)
	}
	if err := validateNativeIdentity(cmd); err != nil {
		return nil, err
	}
	options, optionsErr := nativeOptionsFromCommand(cmd)
	if optionsErr != nil {
		return nil, optionsErr
	}
	if raw, present := cmd["attachments"]; present {
		// Even malformed attachment metadata cannot bypass the native-only
		// request by being silently decoded as an empty string list.
		empty := false
		switch list := raw.(type) {
		case []any:
			empty = len(list) == 0
		case []string:
			empty = len(list) == 0
		}
		if !empty {
			return nil, errors.New("桌面接口暂不支持手机图片；消息未发送，不会自动改用 CLI")
		}
	}
	text, ok := cmd["text"].(string)
	if !ok || strings.TrimSpace(text) == "" || len(text) > 128000 {
		return nil, errors.New("桌面消息内容无效；消息未发送，不会自动改用 CLI")
	}
	st, err := c.o.Desktop.NativeStatus(ctx)
	if err != nil {
		return nil, err
	}
	if !desktopcompanion.ValidateNativeConnection(st, nowMS()) {
		return nil, desktopcompanion.ErrActivationRequired
	}
	if !containsKey(st.SessionKeys, key) {
		return nil, fmt.Errorf("%w；消息未发送，不会自动改用 CLI", desktopcompanion.ErrDesktopScope)
	}
	if !st.Capabilities.Send {
		return nil, desktopcompanion.ErrDesktopCapability
	}
	var optionsSender nativeOptionsSender
	if options.Model != "" || options.Thinking != "" {
		var available bool
		optionsSender, available = c.o.Desktop.(nativeOptionsSender)
		if !available || !desktopcompanion.NativeOptionsSupported(st, options) {
			return nil, errors.New("desktop_capability_unavailable: 当前桌面接口不支持手机指定模型或推理强度；消息未发送，不会自动改用 CLI")
		}
		guard, err := c.validateNativeModelSource(ctx, cmd, options.Model)
		if err != nil {
			return nil, err
		}
		latest, err := c.nativeModelStatus(ctx, key)
		if err != nil {
			return nil, err
		}
		if latest.ExpiresAt != st.ExpiresAt || !desktopcompanion.NativeOptionsSupported(latest, options) {
			return nil, desktopcompanion.ErrDesktopCapability
		}
		if err := guard(); err != nil {
			return nil, err
		}
		options.ValidateSource = guard
	}
	op := str(cmd, "operationId")
	if op == "" {
		op = config.NewUUID()
	}
	if !desktopcompanion.ValidNativeOperationID(op) {
		return nil, errors.New("桌面操作标识无效；消息未发送，不会自动改用 CLI")
	}
	if err := desktopcompanion.CheckRemoteAdmission(ctx); err != nil {
		return nil, err
	}
	if optionsSender != nil {
		err = optionsSender.NativeSendWithOptions(ctx, key, text, op, options)
	} else {
		err = c.o.Desktop.NativeSend(ctx, key, text, op)
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"via": "desktop", "sessionKey": key, "controlSurface": "desktop", "accepted": true}, nil
}

func (c *Client) dispatchDesktop(ctx context.Context, cmd map[string]any) (any, error) {
	if str(cmd, "controlSurface") != "desktop" {
		return nil, errors.New("桌面命令必须明确指定 controlSurface=desktop，不会改用 CLI")
	}
	if c.o.Desktop == nil {
		if str(cmd, "type") == "desktop.status" {
			return map[string]any{"nativeConnection": desktopcompanion.NativeConnection{SessionKeys: []string{}}, "activationRequired": true}, nil
		}
		return nil, desktopcompanion.ErrActivationRequired
	}
	switch str(cmd, "type") {
	case "desktop.status":
		st, err := c.o.Desktop.NativeStatus(ctx)
		if err != nil || !desktopcompanion.ValidateNativeConnection(st, nowMS()) {
			return map[string]any{"nativeConnection": desktopcompanion.NativeConnection{SessionKeys: []string{}}, "activationRequired": true}, nil
		}
		return map[string]any{"nativeConnection": st, "activationRequired": false}, nil
	case "desktop.sessions.list":
		if tool := str(cmd, "tool"); tool != "" && tool != "codex" {
			return nil, errors.New("当前原生桌面实验仅支持 Codex Desktop")
		}
		cursor, limit, err := pageArguments(cmd, maxSessions)
		if err != nil {
			return nil, err
		}
		ss, err := c.o.Desktop.NativeList(ctx)
		if err != nil {
			return nil, err
		}
		if ss == nil {
			ss = []protocol.SessionInfo{}
		}
		offset := 0
		if cursor != "" {
			decoded, decodeErr := base64.RawURLEncoding.DecodeString(cursor)
			var boundary struct {
				Version int    `json:"v"`
				Anchor  string `json:"a"`
			}
			if decodeErr != nil || len(decoded) > 256 || json.Unmarshal(decoded, &boundary) != nil || boundary.Version != 1 || boundary.Anchor == "" {
				return nil, errors.New("桌面会话读取位置无效")
			}
			// A stable thread boundary, not a numeric offset: a new task at the
			// head must not shift the user's next page into a gap or duplicate.
			offset = -1
			for i, item := range ss {
				if item.SessionKey == boundary.Anchor {
					offset = i + 1
					break
				}
			}
			if offset < 0 {
				return nil, errors.New("桌面目录已变化，请刷新")
			}
		}
		if limit > len(ss)-offset {
			limit = len(ss) - offset
		}
		end := offset + limit
		next := ""
		if end < len(ss) {
			raw, _ := json.Marshal(struct {
				Version int    `json:"v"`
				Anchor  string `json:"a"`
			}{1, ss[end-1].SessionKey})
			next = base64.RawURLEncoding.EncodeToString(raw)
		}
		return map[string]any{"sessions": ss[offset:end], "nextCursor": next, "pagination": "v1"}, nil
	case "desktop.session.open":
		key := str(cmd, "sessionKey")
		if _, err := desktoplink.CodexURL(key); err != nil {
			return nil, desktopcompanion.ErrDesktopScope
		}
		cursor, limit, err := pageArguments(cmd, 10)
		if err != nil {
			return nil, err
		}
		var info protocol.SessionInfo
		var events []protocol.Event
		next := ""
		messages, err := messagePageLimit(cmd)
		if err != nil {
			return nil, err
		}
		if pager, ok := c.o.Desktop.(interface {
			NativeOpenMessagesPage(context.Context, string, string, int) (protocol.SessionInfo, []protocol.Event, string, error)
		}); ok && messages != 0 {
			info, events, next, err = pager.NativeOpenMessagesPage(ctx, key, cursor, messages)
		} else if pager, ok := c.o.Desktop.(interface {
			NativeOpenPageLimited(context.Context, string, string, int) (protocol.SessionInfo, []protocol.Event, string, error)
		}); ok {
			info, events, next, err = pager.NativeOpenPageLimited(ctx, key, cursor, limit)
		} else if pager, ok := c.o.Desktop.(interface {
			NativeOpenPage(context.Context, string, string) (protocol.SessionInfo, []protocol.Event, string, error)
		}); ok {
			info, events, next, err = pager.NativeOpenPage(ctx, key, cursor)
		} else {
			if cursor != "" {
				return nil, desktopcompanion.ErrDesktopCapability
			}
			info, events, err = c.o.Desktop.NativeOpen(ctx, key)
		}
		if err != nil {
			return nil, err
		}
		if info.SessionKey != key || info.ControlSurface != "desktop" || !info.Controllable || info.ControlExpiresAt <= nowMS() {
			return nil, desktopcompanion.ErrDesktopUnavailable
		}
		filtered := []protocol.Event{}
		for _, event := range events {
			decorateEventMediaWith(c.attachmentsFor(ctx), &event)
			if event.SessionKey != "" && event.SessionKey != key {
				continue
			}
			if event.Session != nil && event.Session.SessionKey != key {
				continue
			}
			event.SessionKey = key
			event.Tool = "codex"
			if event.Session != nil {
				copy := *event.Session
				copy.ControlSurface = "desktop"
				copy.ControlExpiresAt = info.ControlExpiresAt
				event.Session = &copy
			}
			filtered = append(filtered, event)
		}
		result := map[string]any{"session": info, "events": filtered, "nextCursor": next}
		encoded, encodeErr := json.Marshal(result)
		if encodeErr != nil || len(encoded) > 1<<20 || len(next) > 4096 {
			return nil, desktopcompanion.ErrDesktopUnavailable
		}
		return result, nil
	case "desktop.session.send":
		return c.dispatchNativeSend(ctx, cmd)
	case "desktop.approval.respond":
		ctx, finishAdmission := c.nativeAdmissionContext(ctx, cmd)
		defer finishAdmission()
		approver, ok := c.o.Desktop.(nativeApprovalResponder)
		if !ok {
			return nil, desktopcompanion.ErrDesktopCapability
		}
		key, id, decision, op := str(cmd, "sessionKey"), str(cmd, "approvalId"), str(cmd, "decision"), str(cmd, "operationId")
		if _, err := desktoplink.CodexURL(key); err != nil {
			return nil, desktopcompanion.ErrDesktopScope
		}
		if !desktopcompanion.ValidNativeOperationID(id) || !desktopcompanion.ValidNativeOperationID(op) || (decision != "allow" && decision != "deny") {
			return nil, desktopcompanion.ErrDesktopRequestUnsent
		}
		for field := range cmd {
			if field != "type" && field != "controlSurface" && field != "sessionKey" && field != "accountId" && field != "approvalId" && field != "decision" && field != "message" && field != "operationId" {
				return nil, desktopcompanion.ErrDesktopRequestUnsent
			}
		}
		message := str(cmd, "message")
		if value, present := cmd["message"]; present {
			if _, ok := value.(string); !ok {
				return nil, desktopcompanion.ErrDesktopRequestUnsent
			}
		}
		st, err := c.o.Desktop.NativeStatus(ctx)
		if err != nil {
			return nil, err
		}
		if !desktopcompanion.ValidateNativeConnection(st, nowMS()) || !containsKey(st.SessionKeys, key) {
			return nil, desktopcompanion.ErrDesktopScope
		}
		if !st.Capabilities.Approval || st.ApprovalTransport != desktopcompanion.NativeApprovalTransport {
			return nil, desktopcompanion.ErrDesktopCapability
		}
		if err := desktopcompanion.CheckRemoteAdmission(ctx); err != nil {
			return nil, err
		}
		if err := approver.NativeRespondApproval(ctx, key, id, decision, message, op); err != nil {
			return nil, err
		}
		return map[string]any{"accepted": true, "controlSurface": "desktop", "sessionKey": key, "approvalId": id}, nil
	case "desktop.session.interrupt":
		return nil, desktopcompanion.ErrDesktopCapability
	default:
		return nil, errors.New("不支持的原生桌面命令；不会改用 CLI")
	}
}

// ListSessions merges every agent's sessions, newest first, at most 100.
func ListSessions(ctx context.Context, m agents.Manager, tool string) (map[string]any, error) {
	var all []protocol.SessionInfo
	var errs []string
	var failedTools []string
	agentsList := m.Agents()
	for _, a := range agentsList {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if a == nil {
			continue
		}
		if tool != "" && a.ID() != tool {
			continue
		}
		if err := requireAgentCapability(a, "list"); err != nil {
			if tool != "" {
				return nil, err
			}
			continue
		}
		ss, err := a.Sessions(ctx)
		if err != nil {
			errs = append(errs, a.Name()+"："+err.Error())
			failedTools = append(failedTools, a.ID())
			continue
		}
		if err := requireAgentCapability(a, "list"); err != nil {
			return nil, err
		}
		d, err := cliAgentDescriptor(a)
		if err != nil {
			return nil, err
		}
		for _, s := range ss {
			if !agents.SessionBelongsTo(a, s.SessionKey, s) {
				return nil, errors.New("Agent 返回的会话目录身份不一致")
			}
			s = agents.ProjectSessionInfo(s, a.ID(), d.Capabilities)
			agents.ApplyRememberedWorkspaceMode(m, &s)
			all = append(all, s)
		}
	}
	if len(all) == 0 && len(errs) > 0 {
		return nil, errors.New(strings.Join(errs, "；"))
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].UpdatedAt > all[j].UpdatedAt })
	if len(all) > maxSessions {
		all = all[:maxSessions]
	}
	if all == nil {
		all = []protocol.SessionInfo{}
	}
	result := map[string]any{"sessions": all}
	if len(errs) > 0 {
		result["errors"] = errs
		result["failedTools"] = failedTools
	}
	return result, nil
}

// allowedProjects is the configured allow-list plus the working folders of
// sessions that already exist on this computer. A paired phone can already
// continue those sessions, so starting a new one in the same folder grants
// nothing new; arbitrary other folders still need to be added on the computer.
func allowedProjects(ctx context.Context, m agents.Manager, configured []protocol.Project) []protocol.Project {
	out := append([]protocol.Project{}, configured...)
	seen := map[string]bool{}
	for _, p := range configured {
		seen[filepath.Clean(p.Path)] = true
	}
	listCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	res, err := ListSessions(listCtx, m, "")
	if err != nil {
		return out
	}
	sessions, _ := res["sessions"].([]protocol.SessionInfo)
	for _, s := range sessions {
		if s.Cwd == "" || !filepath.IsAbs(s.Cwd) {
			continue
		}
		clean := filepath.Clean(s.Cwd)
		if seen[clean] {
			continue
		}
		seen[clean] = true
		out = append(out, protocol.Project{Path: clean, Name: config.ProjectName(clean)})
		if len(out) >= 60 {
			break
		}
	}
	return out
}

func agentFor(m agents.Manager, key string) (agents.Agent, string, error) {
	tool, id, ok := strings.Cut(key, ":")
	if !ok || id == "" {
		return nil, "", errors.New("无效的会话")
	}
	a := m.Get(tool)
	if a == nil {
		return nil, "", fmt.Errorf("不支持的工具：%s", tool)
	}
	if !agents.SessionBelongsTo(a, key, protocol.SessionInfo{SessionKey: key}) {
		return nil, "", errors.New("无效的会话归属")
	}
	return a, id, nil
}

func containsKey(list []string, key string) bool {
	for _, item := range list {
		if item == key {
			return true
		}
	}
	return false
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func apiModels(a config.LocalAccount) []string {
	seen, out := map[string]bool{}, []string{}
	for _, m := range a.Models {
		if m != "" && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

func mirrorRemoteChoice(cfg *config.Config, family, id, model string) {
	targets := []string{family}
	if family == "codex" {
		targets = append(targets, "codex-desktop")
	}
	if family == "claude-desktop" {
		family = "claude"
	}
	if cfg.ToolAPISelections == nil {
		cfg.ToolAPISelections = map[string]string{}
	}
	if cfg.ToolModels == nil {
		cfg.ToolModels = map[string]string{}
	}
	if cfg.ToolProtocols == nil {
		cfg.ToolProtocols = map[string]string{}
	}
	a, _ := cfg.LocalAccount(id)
	model = strings.TrimSpace(model)
	protocol := config.InferProtocol(model, a.Wire)
	if protocol == "" {
		protocol = config.NativeProtocol(family)
	}
	for _, t := range targets {
		cfg.ToolAPISelections[t] = id
		if model != "" {
			cfg.ToolModels[t] = model
		} else {
			delete(cfg.ToolModels, t)
		}
		cfg.ToolProtocols[t] = protocol
	}
}

var defaultPhoneAttachments = newAttachmentStore()
var phoneAttachments = defaultPhoneAttachments

type importSessionKey struct{}

// ImportRequested tells NavigateDesktop to import the session (Claude Desktop's
// claude://resume) instead of only bringing the app forward.
func ImportRequested(ctx context.Context) bool {
	v, _ := ctx.Value(importSessionKey{}).(bool)
	return v
}
