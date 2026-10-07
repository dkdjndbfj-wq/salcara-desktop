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

// commandGuard freezes the remote station/phone identity at SSE admission.
// Dispatch may wait on taskConfigMu while another command changes the active
// station; rechecking after that wait prevents an old A command from mutating
// workers that are already owned by B.
type commandGuard struct {
	identity  string
	deviceID  string
	bindingID string
	phoneHash string
}

func withCommandGuard(ctx context.Context, guard commandGuard) context.Context {
	return context.WithValue(ctx, commandGuardKey{}, guard)
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
	if typ != "device.ping" && (!phoneMatches(cfg, guard.bindingID, guard.phoneHash) || !c.canUpload(cfg)) {
		return errors.New("手机绑定已失效，请重新扫码")
	}
	return nil
}

// Dispatch executes one Command (PROTOCOL.md §3) and returns its result. The local console uses it too.
func (c *Client) Dispatch(ctx context.Context, cmd map[string]any) (any, error) {
	if handled, result, err := c.dispatchClaudeDesktopHistory(ctx, cmd); handled {
		return result, err
	}
	typ := str(cmd, "type")
	if typ == "agents.api.set" || typ == "remote.station.switch" || typ == "remote.station.receipt" || typ == "session.start" || typ == "session.send" || typ == "desktop.session.send" {
		c.taskConfigMu.Lock()
		defer c.taskConfigMu.Unlock()
		if err := c.validateCommandGuard(ctx, typ); err != nil {
			return nil, err
		}
		c.expireUpdateLocked()
		if c.updatePrepared {
			return nil, errors.New("程序正在更新，请稍后重试")
		}
	}
	if typ == "remote.station.switch" {
		return c.switchStation(ctx, stationSwitchCommand{
			TargetHubURL: str(cmd, "targetHubUrl"), TargetDeviceID: str(cmd, "targetDeviceId"), TargetComputerID: str(cmd, "targetComputerId"),
			Agent: str(cmd, "agent"), AccountID: str(cmd, "accountId"), Model: str(cmd, "model"), SessionKey: str(cmd, "sessionKey"), OperationID: str(cmd, "operationId"),
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
		index, _ := cmd["index"].(float64)
		total, _ := cmd["total"].(float64)
		done, err := phoneAttachments.put(str(cmd, "id"), str(cmd, "mime"), int(index), int(total), str(cmd, "data"))
		if err != nil {
			return nil, err
		}
		return map[string]any{"complete": done}, nil
	}
	if typ == "agents.api.set" {
		// Picks the API Bridge-run workers use for remote tasks. Keys stay here;
		// the original desktop tool configuration is not touched or restarted.
		family, account, model := str(cmd, "agent"), str(cmd, "accountId"), str(cmd, "model")
		mirror := family
		if family == "claude-desktop" {
			// Claude Desktop's Code sessions run on the Claude Code worker; the
			// choice also lands on the computer's Claude Desktop card.
			family = "claude"
		}
		if c.o.Store == nil {
			return nil, errors.New("程序还在启动，请稍后再试")
		}
		if err := c.checkRemoteAPIChange(ctx, family, str(cmd, "sessionKey")); err != nil {
			return nil, err
		}
		// Selecting a key needs no provider call; an explicit model, however,
		// must belong to that key's freshly verified catalog.
		var selected config.LocalAccount
		if account != "" {
			snapshot := c.o.Store.Get()
			id, ok := AccountForHandle(snapshot, account)
			if !ok {
				return nil, errors.New("电脑上没有这个 API，请刷新后重选")
			}
			selected, _ = snapshot.LocalAccount(id)
			if strings.TrimSpace(model) != "" {
				models, err := c.verifiedAPIModels(ctx, selected, false)
				if err != nil {
					return nil, err
				}
				if !containsKey(models, strings.TrimSpace(model)) {
					return nil, errors.New("这个 API 不支持该模型，请刷新后重选模型")
				}
			}
		}
		if err := c.o.Store.Update(func(cfg *config.Config) error {
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
			}
			if err := cfg.SetRemoteAPI(family, id, model); err != nil {
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
		return c.agentStatus(ctx), nil
	}
	if strings.HasPrefix(typ, "desktop.") && typ != "desktop.navigate" {
		return c.dispatchDesktop(ctx, cmd)
	}
	if surface, present := cmd["controlSurface"]; present {
		s, ok := surface.(string)
		if ok && s == "desktop" && typ == "session.send" {
			return c.dispatchNativeSend(ctx, cmd)
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
	cfg := c.o.Store.Get()
	switch typ {
	case "sessions.list":
		return dispatchSessionsPage(ctx, m, cmd)

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
		if starter, ok := a.(interface{ StartedFromPhone(string) bool }); ok && starter.StartedFromPhone(id) {
			// Claude Desktop only lists sessions it created itself; a session the
			// phone started is imported once through claude://resume.
			navCtx = context.WithValue(ctx, importSessionKey{}, true)
		}
		if err := c.o.NavigateDesktop(navCtx, key); err != nil {
			return nil, err
		}
		return map[string]any{"requested": true, "controlSurface": "desktop", "action": "navigate", "sessionKey": key}, nil

	case "session.open":
		return dispatchHistoryPage(ctx, m, cmd)

	case "session.start":
		tool := str(cmd, "tool")
		a := m.Get(tool)
		if a == nil {
			return nil, fmt.Errorf("不支持的工具：%s", tool)
		}
		prompt := strings.TrimSpace(str(cmd, "prompt"))
		if prompt == "" {
			return nil, errors.New("请输入任务内容")
		}
		effort := str(cmd, "effort")
		if !protocol.ValidModelEffort(effort) {
			return nil, errors.New("无效的推理强度")
		}
		cwd, err := CheckCwd(str(cmd, "cwd"), allowedProjects(ctx, m, cfg.Projects))
		if err != nil {
			return nil, err
		}
		approval := str(cmd, "approval")
		if approval == config.ApprovalAutoAll && !cfg.AllowPhoneAutoAll {
			return nil, errors.New("电脑端没有允许手机使用「全部自动」，请在电脑的「设置」里打开，或改用其他审批方式")
		}
		if !config.ValidApproval(approval) {
			approval = cfg.Approval
		}
		model := strings.TrimSpace(str(cmd, "model"))
		managed, err := c.validateManagedModel(ctx, cfg, a.ID(), model)
		if err != nil {
			return nil, err
		}
		if model == "" && !managed {
			if a.ID() == "codex" {
				model = cfg.CodexModel
			} else {
				model = cfg.ClaudeModel
			}
		}
		if err := validateModelEffort(ctx, cfg, a, model, effort); err != nil {
			return nil, err
		}
		images, err := phoneAttachments.resolve(stringList(cmd, "attachments"))
		if err != nil {
			return nil, err
		}
		if err := validateModelImages(ctx, cfg, a, model, "", len(images)); err != nil {
			return nil, err
		}
		var id string
		if starter, ok := a.(interface {
			StartWithOptions(context.Context, string, string, string, agents.TurnOptions) (string, error)
		}); ok {
			id, err = starter.StartWithOptions(ctx, cwd, prompt, approval, agents.TurnOptions{Model: model, Effort: effort, Images: images,
				Desktop: a.ID() == "claude" && str(cmd, "entrypoint") == "claude-desktop"})
		} else {
			id, err = a.Start(ctx, cwd, prompt, model, approval)
		}
		if err != nil {
			return nil, err
		}
		return map[string]any{"sessionKey": a.ID() + ":" + id}, nil

	case "session.send":
		a, id, err := agentFor(m, str(cmd, "sessionKey"))
		if err != nil {
			return nil, err
		}
		text := strings.TrimSpace(str(cmd, "text"))
		if text == "" {
			return nil, errors.New("请输入内容")
		}
		// The caller freezes its chosen executor with the delivery receipt.
		// Explicit CLI (and older missing-surface requests) never switch to a
		// desktop lease that happens to be available when a retry arrives.
		images, err := phoneAttachments.resolve(stringList(cmd, "attachments"))
		if err != nil {
			return nil, err
		}
		opts := agents.TurnOptions{Model: strings.TrimSpace(str(cmd, "model")), Effort: str(cmd, "effort"), Images: images}
		if len(opts.Model) > 200 || strings.ContainsAny(opts.Model, "\r\n\x00") {
			return nil, errors.New("模型名称无效")
		}
		if _, err := c.validateManagedModel(ctx, cfg, a.ID(), opts.Model); err != nil {
			return nil, err
		}
		if err := validateModelEffort(ctx, cfg, a, opts.Model, opts.Effort); err != nil {
			return nil, err
		}
		if err := validateModelImages(ctx, cfg, a, opts.Model, id, len(opts.Images)); err != nil {
			return nil, err
		}
		if sender, ok := a.(interface {
			SendWithOptions(context.Context, string, string, agents.TurnOptions) error
		}); ok && (opts.Model != "" || opts.Effort != "" || len(opts.Images) > 0) {
			err = sender.SendWithOptions(ctx, id, text, opts)
		} else if len(opts.Images) > 0 {
			return nil, errors.New("这个工具暂不支持图片")
		} else {
			err = a.Send(ctx, id, text)
		}
		if err != nil {
			return nil, err
		}
		return map[string]any{"via": "background"}, nil

	case "session.interrupt":
		a, id, err := agentFor(m, str(cmd, "sessionKey"))
		if err != nil {
			return nil, err
		}
		if err := a.Interrupt(ctx, id); err != nil {
			return nil, err
		}
		return map[string]any{}, nil

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
		for _, a := range m.Agents() {
			if agents.RespondWithAnswers(a, aid, agents.ApprovalResponse{Decision: decision, Message: str(cmd, "message"), Answers: answers}) {
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
		if p == nil {
			p = []protocol.Project{}
		}
		return map[string]any{"projects": p}, nil

	case "models.list":
		// The selected API's upstream catalog: the phone can switch to any of these
		// per message (other families are converted locally by the gateway).
		if c.o.Store != nil {
			cfg := c.o.Store.Get()
			family := str(cmd, "tool")
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
				models, err := c.verifiedAPIModels(ctx, vault, refresh)
				if err != nil {
					return nil, err
				}
				a.Models = models
				currentCfg := c.o.Store.Get()
				current, exists := remoteModelAccount(currentCfg, family)
				if !exists || current.ID != a.ID || config.AccountFingerprint(current) != config.AccountFingerprint(a) {
					return nil, errors.New("API 已更换，请重新读取模型")
				}
				return map[string]any{"models": apiModels(a), "modelCapabilities": protocol.UnknownModelCapabilities(apiModels(a), "relay-model-list"), "api": safeConfirmationLabel(current.Name, "已命名 API", currentCfg)}, nil
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

// Native commands route before manager lookup. Do not start/resume/open/send a
// CLI process, even when a native tool returns an error or no current history.
func (c *Client) dispatchNativeSend(ctx context.Context, cmd map[string]any) (any, error) {
	if c.o.Desktop == nil {
		return nil, desktopcompanion.ErrActivationRequired
	}
	key := str(cmd, "sessionKey")
	if _, err := desktoplink.CodexURL(key); err != nil {
		return nil, fmt.Errorf("%w；消息未发送，不会自动改用 CLI", desktopcompanion.ErrDesktopScope)
	}
	for _, field := range []string{"model", "effort"} {
		if value, present := cmd[field]; present && value != "" {
			if field == "model" {
				return nil, errors.New("桌面实时同步暂不支持手机指定模型，请在电脑端设置；消息未发送，不会自动改用 CLI")
			}
			return nil, errors.New("桌面实时同步暂不支持手机指定推理强度，请在电脑端设置；消息未发送，不会自动改用 CLI")
		}
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
	op := str(cmd, "operationId")
	if op == "" {
		op = config.NewUUID()
	}
	if !desktopcompanion.ValidNativeOperationID(op) {
		return nil, errors.New("桌面操作标识无效；消息未发送，不会自动改用 CLI")
	}
	if err = c.o.Desktop.NativeSend(ctx, key, text, op); err != nil {
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
		approver, ok := c.o.Desktop.(interface {
			NativeRespondApproval(context.Context, string, string, string, string, string) error
		})
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
			if field != "type" && field != "controlSurface" && field != "sessionKey" && field != "approvalId" && field != "decision" && field != "message" && field != "operationId" {
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
	agentsList := m.Agents()
	for _, a := range agentsList {
		if tool != "" && a.ID() != tool {
			continue
		}
		ss, err := a.Sessions(ctx)
		if err != nil {
			errs = append(errs, a.Name()+"："+err.Error())
			continue
		}
		for _, s := range ss {
			if s.SessionKey == "" {
				continue
			}
			s.ControlSurface = "cli"
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
	return map[string]any{"sessions": all}, nil
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

var phoneAttachments = newAttachmentStore()

type importSessionKey struct{}

// ImportRequested tells NavigateDesktop to import the session (Claude Desktop's
// claude://resume) instead of only bringing the app forward.
func ImportRequested(ctx context.Context) bool {
	v, _ := ctx.Value(importSessionKey{}).(bool)
	return v
}
