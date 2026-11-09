package console

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/launcher"
	"salcara/bridge/internal/toolcfg"
)

const safeAgentChangeProtocol = "safe-agent-change.v1"

type safeAgentChangeRequest struct {
	Protocol               string  `json:"protocol"`
	OperationID            string  `json:"operationId"`
	Target                 string  `json:"target"`
	ID                     string  `json:"id"`
	Model                  string  `json:"model"`
	WireProtocol           string  `json:"wireProtocol"`
	Override               bool    `json:"override"`
	ApplyOnly              bool    `json:"applyOnly"`
	Workspace              string  `json:"workspace,omitempty"`
	AllowModeChange        bool    `json:"allowModeChange,omitempty"`
	ExpectedSourceRevision *string `json:"expectedSourceRevision"`
}

func (s *Server) reserveAgentChange(protocol, operation string) error {
	if protocol != safeAgentChangeProtocol || !agents.ValidControlOperationID(operation) {
		return errors.New("安全切换协议或操作标识无效，请更新 Bridge 并重新打开")
	}
	s.agentChangeIDMu.Lock()
	defer s.agentChangeIDMu.Unlock()
	operation = strings.ToLower(operation)
	if s.agentChangeIDs == nil {
		s.agentChangeIDs = map[string]struct{}{}
	}
	if _, exists := s.agentChangeIDs[operation]; exists {
		return errors.New("本次操作已提交；请核对结果后重新点击，不会自动重发")
	}
	// Retain tombstones for this process lifetime. Expiring an unknown delivery
	// would silently turn a retry into a second stop/launch.
	if len(s.agentChangeIDs) >= 4096 {
		return errors.New("本次运行的切换请求过多，请稍后重新打开 Bridge")
	}
	s.agentChangeIDs[operation] = struct{}{}
	return nil
}

func safeAgentChangeError(w http.ResponseWriter, status int, code string, err error) {
	w.WriteHeader(status)
	writeJSON(w, map[string]any{"ok": false, "protocol": safeAgentChangeProtocol, "code": "safe_agent_change_" + code, "error": err.Error()})
}

func validateSafeAgentChange(in *safeAgentChangeRequest) error {
	if in.Protocol != safeAgentChangeProtocol || !agents.ValidControlOperationID(in.OperationID) || !launcher.ValidTarget(in.Target) || !config.ValidAccountID(in.ID) {
		return errors.New("请求格式有误，请重新打开 Agent 配置")
	}
	if in.ExpectedSourceRevision == nil || *in.ExpectedSourceRevision == "" {
		return errLocalSourceChanged
	}
	in.Model = strings.TrimSpace(in.Model)
	if len(in.Model) > 200 || strings.ContainsAny(in.Model, "\r\n\x00") {
		return errors.New("无效的模型 ID")
	}
	switch in.WireProtocol {
	case "", "responses", "chat", "anthropic":
	default:
		return errors.New("不支持的上游接口协议")
	}
	return nil
}

func bindSafeAgentChange(c *config.Config, in safeAgentChangeRequest) {
	if c.ToolAPISelections == nil {
		c.ToolAPISelections = map[string]string{}
	}
	if c.ToolModels == nil {
		c.ToolModels = map[string]string{}
	}
	if c.ToolProtocols == nil {
		c.ToolProtocols = map[string]string{}
	}
	if c.ToolCatalogOverride == nil {
		c.ToolCatalogOverride = map[string]bool{}
	}
	c.ToolAPISelections[in.Target], c.ToolModels[in.Target], c.ToolProtocols[in.Target], c.ToolCatalogOverride[in.Target] = in.ID, in.Model, in.WireProtocol, in.Override
}

func (s *Server) handleSafeAgentChange(w http.ResponseWriter, r *http.Request) {
	if !s.admitUpdateWork(w) {
		return
	}
	var in safeAgentChangeRequest
	if err := readReverseJSON(r, &in, 8192); err != nil {
		safeAgentChangeError(w, 400, "invalid", err)
		return
	}
	if err := validateSafeAgentChange(&in); err != nil {
		safeAgentChangeError(w, 400, "invalid", err)
		return
	}
	before := s.d.Store.Get()
	a, exists := before.LocalAccount(in.ID)
	if !exists {
		safeAgentChangeError(w, 404, "source", errors.New("API 账号不存在"))
		return
	}
	if err := checkLocalSourceRevision(before, a, in.ExpectedSourceRevision); err != nil {
		safeAgentChangeError(w, 409, "source_changed", err)
		return
	}
	if in.Model != "" && len(a.Models) > 0 && (!containsLocalModel(a.Models, in.Model) || !config.ModelAllowed(a, in.Model)) {
		safeAgentChangeError(w, 409, "model", errors.New("所选模型不可用，请重新选择"))
		return
	}
	if s.d.Local == nil || in.Target == "claude-desktop" && s.d.ClaudeDesktop == nil {
		safeAgentChangeError(w, 409, "unavailable", errors.New("Agent 配置服务不可用"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 55*time.Second)
	defer cancel()
	initial := before.Clone()
	bindSafeAgentChange(&initial, in)
	initialAccount := initial.APIForTool(a, in.Target)
	initialAccount.GatewayLaunchLease = config.RandomToken(32) // validation only; never published
	initialConnection := initial.ToolConnection(initialAccount, in.Target, s.d.Port)
	if err := s.validateSafeNativeConfiguration(ctx, initialConnection, in.Target, in.Workspace, before.LocalToolPaths, in.AllowModeChange); err != nil {
		safeAgentChangeError(w, 400, "invalid_native", err)
		return
	}
	if err := s.reserveAgentChange(in.Protocol, in.OperationID); err != nil {
		safeAgentChangeError(w, 409, "duplicate", err)
		return
	}
	if in.ApplyOnly {
		launcher.LaunchApplyPlan(ctx)
	} else {
		launcher.LaunchPlan(ctx, in.Target, false, false)
	}
	launcher.LaunchPhase(ctx, "stop", "正在安全停止相关 Agent 任务")
	release, err := s.beginSafeLocalToolChange(ctx, in.Target)
	if err != nil {
		safeAgentChangeError(w, 409, "stop_unconfirmed", err)
		return
	}
	defer release()
	preStopGuard := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		now := s.d.Store.Get()
		account, exists := now.LocalAccount(in.ID)
		if !exists || !sameLaunchAccount(account, a) || !sameLaunchSelection(now, before, in.Target) {
			return errLocalSourceChanged
		}
		return checkLocalSourceRevision(now, account, in.ExpectedSourceRevision)
	}
	ctx = launcher.WithConfigurationGuard(ctx, preStopGuard)
	if s.nativeConfigurationStop != nil {
		ctx = launcher.WithNativeConfigurationStopForTest(ctx, s.nativeConfigurationStop)
	}
	if in.Target == "claude-desktop" {
		err = s.d.ClaudeDesktop.StopForConfigurationChange(ctx, before.LocalToolPaths)
	} else {
		err = s.d.Local.StopConfigurationFamily(ctx, initialConnection, in.Target, in.Workspace, before.LocalToolPaths)
	}
	if err != nil {
		safeAgentChangeError(w, 409, "stop_unconfirmed", err)
		return
	}
	// A source/selection change while waiting for stop invalidates the click.
	current := s.d.Store.Get()
	currentAccount, currentExists := current.LocalAccount(in.ID)
	if ctx.Err() != nil || !currentExists || !sameLaunchAccount(currentAccount, a) || !sameLaunchSelection(current, before, in.Target) || checkLocalSourceRevision(current, currentAccount, in.ExpectedSourceRevision) != nil {
		safeAgentChangeError(w, 409, "source_changed", errors.New("停止期间配置已改变或请求已取消，请重新打开"))
		return
	}
	launcher.LaunchPhase(ctx, "catalog", "读取可用模型")
	a, _, err = s.refreshLaunchCatalogChecked(ctx, a, func(now config.Config) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !sameLaunchSelection(now, before, in.Target) {
			return errLocalSourceChanged
		}
		account, exists := now.LocalAccount(in.ID)
		if !exists {
			return errLocalSourceChanged
		}
		return checkLocalSourceRevision(now, account, in.ExpectedSourceRevision)
	})
	if err != nil {
		safeAgentChangeError(w, 409, "catalog", err)
		return
	}
	c := s.d.Store.Get()
	if !unchangedSelectionAfterCatalog(c, before, a, in.Target) {
		safeAgentChangeError(w, 409, "source_changed", errLocalSourceChanged)
		return
	}
	vault := a
	revision := c.AccountSourceRevision(vault)
	prospective := c.Clone()
	bindSafeAgentChange(&prospective, in)
	a = launchCatalogDefault(prospective.APIForTool(a, in.Target))
	if in.Model != "" && a.Model != in.Model {
		safeAgentChangeError(w, 409, "model", errors.New("所选模型已不在可用目录中，请重新选择"))
		return
	}
	a.GatewayLaunchLease = config.RandomToken(32)
	lease := a.GatewayLaunchLease
	committed := false
	finalizing := false
	guard := func() error {
		if err := ctx.Err(); err != nil && !finalizing {
			return err
		}
		if s.d.Hub != nil {
			if err := s.d.Hub.CheckUpdateAdmission(); err != nil {
				return err
			}
		}
		now := s.d.Store.Get()
		account, exists := now.LocalAccount(in.ID)
		selection := c
		if committed {
			selection = prospective
		}
		if !exists || !sameLaunchAccount(account, vault) || checkLocalSourceRevision(now, account, &revision) != nil || now.GatewayRuntimeKey != c.GatewayRuntimeKey || !sameLaunchSelection(now, selection, in.Target) {
			return errLocalSourceChanged
		}
		if committed {
			if binding, ok := now.AppliedToolBinding(in.Target); !ok || binding.LaunchLease != lease {
				return errLocalSourceChanged
			}
		}
		return nil
	}
	ctx = launcher.WithManagedLaunch(launcher.WithConfigurationGuard(ctx, guard))
	if s.nativeConfigurationStop != nil {
		ctx = launcher.WithNativeConfigurationStopForTest(ctx, s.nativeConfigurationStop)
	}
	authorize := func() error {
		if err := guard(); err != nil {
			return err
		}
		err := s.d.Store.Update(func(now *config.Config) error {
			account, exists := now.LocalAccount(in.ID)
			if ctx.Err() != nil || !exists || !sameLaunchAccount(account, vault) || checkLocalSourceRevision(*now, account, &revision) != nil || now.GatewayRuntimeKey != c.GatewayRuntimeKey || !sameLaunchSelection(*now, c, in.Target) {
				return errLocalSourceChanged
			}
			provider := ""
			if in.Target == "claude-desktop" {
				provider = "gateway"
			}
			if config.ToolKind(in.Target) == "codex" {
				data, _ := os.ReadFile(toolcfg.CodexConfigPath())
				provider, _, _, _ = toolcfg.InspectCodexTOML(string(data))
				if provider == "" {
					provider = "openai"
				}
			}
			bindSafeAgentChange(now, in)
			now.RecordAppliedAPI(in.Target, a, provider)
			return nil
		})
		committed = err == nil
		return err
	}
	connection := prospective.ToolConnection(a, in.Target, s.d.Port)
	var result launcher.Result
	if in.Target == "claude-desktop" {
		connection.Models, connection.DesktopModelLabels = config.ClaudeDesktopCatalog(a)
		connection.Model, connection.Protocol, connection.Wire = "", "anthropic", "anthropic"
		configure := s.d.ClaudeDesktop.SwitchAuthorized
		if in.ApplyOnly {
			configure = s.d.ClaudeDesktop.ApplyAuthorized
		}
		result, err = configure(ctx, connection, c.LocalToolPaths, in.AllowModeChange, authorize)
	} else {
		configure := s.d.Local.SwitchAuthorized
		if in.ApplyOnly {
			configure = s.d.Local.ApplyAuthorized
		}
		result, err = configure(ctx, connection, in.Target, in.Workspace, c.LocalToolPaths, authorize)
	}
	if err != nil {
		if committed {
			s.rollbackSafeAgentChange(c, prospective, in.Target, lease)
		}
		s.d.Store.RevokeAppliedLaunchLease(in.Target, lease)
		safeAgentChangeError(w, 409, "apply", err)
		return
	}
	launcher.LaunchPhase(ctx, "record", "保存本次应用状态")
	// Once configuration/start was acknowledged, HTTP cancellation cannot
	// revoke the credential underneath the running process. Finish only the
	// source/selection/lease CAS; never replay an uncertain native start.
	finalizing = true
	if err := guard(); err != nil {
		safeAgentChangeRecordError(w, in.OperationID, in.ApplyOnly, result, err)
		return
	}
	if err := s.d.Store.Update(func(now *config.Config) error {
		account, exists := now.LocalAccount(in.ID)
		if !exists || !sameLaunchAccount(account, vault) || checkLocalSourceRevision(*now, account, &revision) != nil || now.GatewayRuntimeKey != c.GatewayRuntimeKey || !sameLaunchSelection(*now, prospective, in.Target) {
			return errLocalSourceChanged
		}
		if binding, ok := now.AppliedToolBinding(in.Target); !ok || binding.LaunchLease != lease {
			return errLocalSourceChanged
		}
		for i := range now.LocalAccounts {
			if now.LocalAccounts[i].ID == in.ID {
				now.LocalAccounts[i].LastUsedAt = time.Now().UnixMilli()
				now.LocalAccounts[i].Target = in.Target
			}
		}
		return nil
	}); err != nil {
		safeAgentChangeRecordError(w, in.OperationID, in.ApplyOnly, result, err)
		return
	}
	if !in.ApplyOnly && in.Target == "codex-desktop" {
		if marker, ok := s.d.Companion.(interface{ MarkManagedLaunch() }); ok {
			marker.MarkManagedLaunch()
		}
	}
	writeJSON(w, map[string]any{"ok": true, "protocol": safeAgentChangeProtocol, "operationId": in.OperationID, "result": result})
}

func (s *Server) validateSafeNativeConfiguration(ctx context.Context, connection config.LocalAccount, target, workspace string, paths map[string]string, allowModeChange bool) error {
	if s.d.Local == nil {
		return errors.New("Agent 配置服务不可用")
	}
	if target == "claude-desktop" {
		if s.d.ClaudeDesktop == nil {
			return errors.New("Claude Desktop 配置服务不可用")
		}
		connection.Models, connection.DesktopModelLabels = config.ClaudeDesktopCatalog(connection)
		connection.Model, connection.Protocol, connection.Wire = "", "anthropic", "anthropic"
		return s.d.ClaudeDesktop.ValidateConfigurationChange(ctx, connection, paths, allowModeChange)
	}
	_, err := s.d.Local.PrepareSwitch(ctx, connection, target, workspace, paths)
	return err
}

func safeAgentChangeRecordError(w http.ResponseWriter, operation string, applyOnly bool, result launcher.Result, err error) {
	w.WriteHeader(http.StatusConflict)
	writeJSON(w, map[string]any{"ok": false, "protocol": safeAgentChangeProtocol, "operationId": operation, "code": "safe_agent_change_record_uncertain", "error": "配置或启动已提交，但状态保存未确认，请在原工具核对：" + err.Error(), "started": !applyOnly, "applied": true, "uncertain": true, "result": result})
}

func restoreMapEntry[T any](dst map[string]T, src map[string]T, key string) {
	if value, exists := src[key]; exists {
		dst[key] = value
	} else {
		delete(dst, key)
	}
}

func (s *Server) rollbackSafeAgentChange(before, proposed config.Config, target, lease string) {
	_ = s.d.Store.Update(func(now *config.Config) error {
		binding, ok := now.ToolAPIApplied[config.ToolFamily(target)]
		if !ok || binding.LaunchLease != lease {
			return nil
		}
		if sameLaunchSelection(*now, proposed, target) {
			restoreMapEntry(now.ToolAPISelections, before.ToolAPISelections, target)
			restoreMapEntry(now.ToolModels, before.ToolModels, target)
			restoreMapEntry(now.ToolProtocols, before.ToolProtocols, target)
			restoreMapEntry(now.ToolCatalogOverride, before.ToolCatalogOverride, target)
		}
		restoreMapEntry(now.ToolAPIApplied, before.ToolAPIApplied, config.ToolFamily(target))
		return nil
	})
}
