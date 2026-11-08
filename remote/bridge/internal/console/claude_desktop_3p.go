package console

import (
	"errors"
	"net/http"
	"reflect"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/launcher"
)

func (s *Server) claudeDesktop3PRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/local/claude-desktop/status", s.handleClaudeDesktop3PStatus)
	m.HandleFunc("POST /api/local/claude-desktop/switch", launchProgressEndpoint(s.handleClaudeDesktop3PSwitch))
	m.HandleFunc("POST /api/local/claude-desktop/restore", s.handleClaudeDesktop3PRestore)
}
func (s *Server) handleClaudeDesktop3PStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := ctxTimeout(r, 20*time.Second)
	defer cancel()
	writeJSON(w, s.d.ClaudeDesktop.Status(ctx, s.d.Store.Get().LocalToolPaths))
}
func (s *Server) handleClaudeDesktop3PSwitch(w http.ResponseWriter, r *http.Request) {
	launcher.LaunchPlan(r.Context(), "claude-desktop", false, false)
	var in struct {
		ID                     string  `json:"id"`
		Model                  string  `json:"model,omitempty"`
		CatalogOverride        *bool   `json:"catalogOverride,omitempty"`
		Confirmed              bool    `json:"confirmed"`
		AllowModeChange        bool    `json:"allowModeChange"`
		ExpectedSourceRevision *string `json:"expectedSourceRevision,omitempty"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if !in.Confirmed {
		writeErr(w, 400, "请保存当前任务后点击配置并打开")
		return
	}
	c := s.d.Store.Get()
	a, ok := c.LocalAccount(in.ID)
	if !ok {
		writeErr(w, 404, "API 密钥不存在")
		return
	}
	if err := checkLocalSourceRevision(c, a, in.ExpectedSourceRevision); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	ctx, cancel := ctxTimeout(r, 55*time.Second)
	defer cancel()
	launcher.LaunchPhase(ctx, "catalog", "读取可用模型")
	var catalogErr error
	a, catalogErr = s.refreshLaunchCatalog(ctx, a)
	if catalogErr != nil {
		writeErr(w, http.StatusConflict, catalogErr.Error())
		return
	}
	launcher.LaunchPhase(ctx, "selection", "读取当前工具配置")
	vaultAccount := a
	refreshedConfig := s.d.Store.Get()
	if !unchangedSelectionAfterCatalog(refreshedConfig, c, a, "claude-desktop") {
		writeErr(w, http.StatusConflict, "读取模型期间工具选择已改变，请重新打开")
		return
	}
	authorizationSourceRevision := c.AccountSourceRevision(vaultAccount)
	if in.ExpectedSourceRevision != nil {
		current, exists := refreshedConfig.LocalAccount(a.ID)
		if !exists || checkLocalSourceRevision(refreshedConfig, current, &authorizationSourceRevision) != nil {
			writeErr(w, http.StatusConflict, errLocalSourceChanged.Error())
			return
		}
	}
	c = refreshedConfig
	a = c.APIForTool(a, "claude-desktop")
	if in.Model != "" {
		a.Model = in.Model
	}
	a.CatalogOverride = true
	if in.CatalogOverride != nil {
		a.CatalogOverride = *in.CatalogOverride
	}
	a.GatewayLaunchLease = config.RandomToken(32)
	connection := c.ToolConnection(a, "claude-desktop", s.d.Port)
	connection.Models, connection.DesktopModelLabels = config.ClaudeDesktopCatalog(a)
	connection.Model, connection.Protocol, connection.Wire = "", "anthropic", "anthropic"
	processKey := c.GatewayRuntimeKey
	launchConfig := c
	authorize := func() error {
		return s.d.Store.Update(func(c *config.Config) error {
			current, exists := c.LocalAccount(a.ID)
			if in.ExpectedSourceRevision != nil && (!exists || checkLocalSourceRevision(*c, current, &authorizationSourceRevision) != nil) {
				return errLocalSourceChanged
			}
			if !exists || !sameLaunchAccount(current, vaultAccount) || c.GatewayRuntimeKey != processKey || !sameLaunchSelection(*c, launchConfig, "claude-desktop") {
				return errors.New("启动期间账号已变更，请重新应用")
			}
			if c.ToolCatalogOverride == nil {
				c.ToolCatalogOverride = map[string]bool{}
			}
			c.ToolCatalogOverride["claude-desktop"] = a.CatalogOverride
			c.RecordAppliedAPI("claude-desktop", a, "gateway")
			return nil
		})
	}
	result, err := s.d.ClaudeDesktop.SwitchAuthorized(ctx, connection, c.LocalToolPaths, in.AllowModeChange, authorize)
	if err != nil {
		s.d.Store.RevokeAppliedLaunchLease("claude-desktop", a.GatewayLaunchLease)
		if errors.Is(err, errLocalSourceChanged) {
			writeErr(w, http.StatusConflict, errLocalSourceChanged.Error())
			return
		}
		writeErr(w, 400, err.Error())
		return
	}
	launcher.LaunchPhase(ctx, "record", "保存本次启动状态")
	err = s.d.Store.Update(func(c *config.Config) error {
		current, exists := c.LocalAccount(a.ID)
		binding, applied := c.AppliedToolBinding("claude-desktop")
		if !exists || !sameLaunchAccount(current, vaultAccount) || !applied || binding.LaunchLease != a.GatewayLaunchLease || c.GatewayRuntimeKey != processKey {
			return errors.New("启动期间账号或授权已变更，请重新应用")
		}
		for i, x := range c.LocalAccounts {
			if x.ID == a.ID {
				c.LocalAccounts[i].LastUsedAt = time.Now().UnixMilli()
				c.LocalAccounts[i].Target = "claude-desktop"
			}
		}
		return nil
	})
	if err != nil {
		s.d.Store.RevokeAppliedLaunchLease("claude-desktop", a.GatewayLaunchLease)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		writeJSON(w, map[string]any{"error": "工具已启动，本次启动状态保存失败：" + err.Error(), "started": true, "result": result})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "result": result, "message": result.Message})
}
func (s *Server) handleClaudeDesktop3PRestore(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Confirmed bool `json:"confirmed"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if !in.Confirmed {
		writeErr(w, 400, "请先确认恢复原配置并保存当前任务")
		return
	}
	ctx, cancel := ctxTimeout(r, 55*time.Second)
	defer cancel()
	var restoredBinding config.AppliedAPI
	var hadBinding bool
	result, err := s.d.ClaudeDesktop.RestoreAuthorized(ctx, s.d.Store.Get().LocalToolPaths, func() error {
		// The launch mutex is held here, after quit-time profile validation.
		restoredBinding, hadBinding = s.d.Store.Get().ToolAPIApplied["claude-desktop"]
		if hadBinding {
			s.d.Store.RevokeAppliedLaunchLease("claude-desktop", restoredBinding.LaunchLease)
			restoredBinding.LaunchLease = ""
		}
		return nil
	})
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := s.d.Store.Update(func(c *config.Config) error {
		if current, ok := c.ToolAPIApplied["claude-desktop"]; hadBinding && ok && reflect.DeepEqual(current, restoredBinding) {
			delete(c.ToolAPIApplied, "claude-desktop")
		}
		return nil
	}); err != nil {
		result.Message += " 本次工具连接授权已撤销，但本地应用标记未能保存。"
	}
	writeJSON(w, map[string]any{"ok": true, "result": result, "message": result.Message})
}
