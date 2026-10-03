package console

import (
	"net/http"
	"time"

	"salcara/bridge/internal/config"
)

func (s *Server) claudeDesktop3PRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/local/claude-desktop/status", s.handleClaudeDesktop3PStatus)
	m.HandleFunc("POST /api/local/claude-desktop/switch", s.handleClaudeDesktop3PSwitch)
	m.HandleFunc("POST /api/local/claude-desktop/restore", s.handleClaudeDesktop3PRestore)
}
func (s *Server) handleClaudeDesktop3PStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := ctxTimeout(r, 20*time.Second)
	defer cancel()
	writeJSON(w, s.d.ClaudeDesktop.Status(ctx, s.d.Store.Get().LocalToolPaths))
}
func (s *Server) handleClaudeDesktop3PSwitch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID              string `json:"id"`
		Model           string `json:"model,omitempty"`
		CatalogOverride *bool  `json:"catalogOverride,omitempty"`
		Confirmed       bool   `json:"confirmed"`
		AllowModeChange bool   `json:"allowModeChange"`
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
	a = c.APIForTool(a, "claude-desktop")
	if in.Model != "" {
		a.Model = in.Model
	}
	a.CatalogOverride = true
	if in.CatalogOverride != nil {
		a.CatalogOverride = *in.CatalogOverride
	}
	connection := c.ToolConnection(a, "claude-desktop", s.d.Port)
	connection.Models, connection.DesktopModelLabels = config.ClaudeDesktopCatalog(a)
	connection.Model, connection.Protocol, connection.Wire = "", "anthropic", "anthropic"
	ctx, cancel := ctxTimeout(r, 55*time.Second)
	defer cancel()
	result, err := s.d.ClaudeDesktop.Switch(ctx, connection, c.LocalToolPaths, in.AllowModeChange)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	err = s.d.Store.Update(func(c *config.Config) error {
		for i, x := range c.LocalAccounts {
			if x.ID == a.ID {
				c.LocalAccounts[i].LastUsedAt = time.Now().UnixMilli()
				c.LocalAccounts[i].Target = "claude-desktop"
			}
		}
		if c.ToolCatalogOverride == nil {
			c.ToolCatalogOverride = map[string]bool{}
		}
		c.ToolCatalogOverride["claude-desktop"] = a.CatalogOverride
		c.RecordAppliedAPI("claude-desktop", a, "gateway")
		return nil
	})
	if err != nil {
		result.Message += " 本地 API 选择记录保存失败，原工具已启动。"
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
	result, err := s.d.ClaudeDesktop.Restore(ctx, s.d.Store.Get().LocalToolPaths)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := s.d.Store.Update(func(c *config.Config) error { delete(c.ToolAPIApplied, "claude-desktop"); return nil }); err != nil {
		result.Message += " 本地应用标记未能保存。"
	}
	writeJSON(w, map[string]any{"ok": true, "result": result, "message": result.Message})
}
