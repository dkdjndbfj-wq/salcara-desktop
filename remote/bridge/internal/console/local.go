package console

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/launcher"
	"salcara/bridge/internal/toolcfg"
)

func (s *Server) localRoutes(m *http.ServeMux) {
	s.claudeDesktop3PRoutes(m)
	m.HandleFunc("GET /api/local/accounts", s.handleLocalAccounts)
	m.HandleFunc("POST /api/local/accounts", s.handleLocalSave)
	m.HandleFunc("POST /api/local/accounts/delete", s.handleLocalDelete)
	m.HandleFunc("POST /api/local/accounts/select", s.handleLocalSelect)
	m.HandleFunc("POST /api/local/models", s.handleLocalModels)
	m.HandleFunc("POST /api/local/launch", s.handleLocalLaunch)
	m.HandleFunc("POST /api/local/switch/preview", s.handleLocalSwitchPreview)
	m.HandleFunc("POST /api/local/switch", s.handleLocalSwitch)
	m.HandleFunc("POST /api/local/default", s.handleLocalDefault)
	m.HandleFunc("POST /api/local/tools", s.handleLocalToolPaths)
	m.HandleFunc("POST /api/local/bind", s.handleLocalBind)
	m.HandleFunc("POST /api/local/resume", s.handleLocalResume)
	m.HandleFunc("GET /api/local/desktop-companion", s.handleDesktopCompanion)
	m.HandleFunc("POST /api/local/desktop-companion/install", s.handleInstallDesktopCompanion)
	m.HandleFunc("GET /api/local/desktop-companion/uninstall", s.handlePreviewUninstallDesktopCompanion)
	m.HandleFunc("POST /api/local/desktop-companion/uninstall", s.handleUninstallDesktopCompanion)
	m.HandleFunc("GET /api/local/desktop-companion/control", s.handleDesktopControl)
	m.HandleFunc("POST /api/local/desktop-companion/disconnect", s.handleDisconnectDesktop)
}

type localSwitchRequest struct {
	ID        string `json:"id"`
	Target    string `json:"target"`
	Workspace string `json:"workspace"`
	Confirmed bool   `json:"confirmed"`
	KeepModel bool   `json:"keepModel"`
}

func (s *Server) handleLocalSwitchPreview(w http.ResponseWriter, r *http.Request) {
	var in localSwitchRequest
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	c := s.d.Store.Get()
	a, ok := c.LocalAccount(in.ID)
	if !ok {
		writeErr(w, 404, "请先添加一个 API 账号")
		return
	}
	ctx, cancel := ctxTimeout(r, 20*time.Second)
	defer cancel()
	a = c.APIForTool(a, in.Target)
	if in.KeepModel {
		var err error
		a, err = launcher.PreserveCurrentToolModel(a, in.Target)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
	}
	info, err := s.d.Local.PreviewSwitch(ctx, c.ToolConnection(a, in.Target, s.d.Port), in.Target, in.Workspace, c.LocalToolPaths)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "info": info})
}

func (s *Server) handleLocalSwitch(w http.ResponseWriter, r *http.Request) {
	var in localSwitchRequest
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if !in.Confirmed {
		writeErr(w, 400, "请先确认任务已结束、内容已保存，再切换原工具")
		return
	}
	c := s.d.Store.Get()
	a, ok := c.LocalAccount(in.ID)
	if !ok {
		writeErr(w, 404, "账号不存在")
		return
	}
	ctx, cancel := ctxTimeout(r, 55*time.Second)
	defer cancel()
	a = c.APIForTool(a, in.Target)
	if in.KeepModel {
		var err error
		a, err = launcher.PreserveCurrentToolModel(a, in.Target)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
	}
	result, err := s.d.Local.Switch(ctx, c.ToolConnection(a, in.Target, s.d.Port), in.Target, in.Workspace, c.LocalToolPaths)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	err = s.d.Store.Update(func(c *config.Config) error {
		for i, x := range c.LocalAccounts {
			if x.ID == a.ID {
				c.LocalAccounts[i].LastUsedAt = time.Now().UnixMilli()
				c.LocalAccounts[i].Target = in.Target
			}
		}
		if a.Kind == "codex" {
			c.ActiveCodexAccount = a.ID
		} else {
			c.ActiveClaudeAccount = a.ID
		}
		provider := ""
		if a.Kind == "codex" {
			b, _ := os.ReadFile(toolcfg.CodexConfigPath())
			provider, _, _, _ = toolcfg.InspectCodexTOML(string(b))
			if provider == "" {
				provider = "openai"
			}
		}
		if in.KeepModel {
			if c.ToolModels == nil {
				c.ToolModels = map[string]string{}
			}
			if c.ToolProtocols == nil {
				c.ToolProtocols = map[string]string{}
			}
			c.ToolModels[in.Target], c.ToolProtocols[in.Target] = a.Model, a.Protocol
		}
		c.RecordAppliedAPI(in.Target, a, provider)
		return nil
	})
	if err != nil {
		result.Message += " 常用账号状态未能保存。"
	}
	s.log.Printf("local switch: target=%s account=%s pid=%d", in.Target, a.ID, result.PID)
	writeJSON(w, map[string]any{"ok": true, "result": result})
}

func maskedAccount(a config.LocalAccount) map[string]any {
	return map[string]any{"id": a.ID, "name": a.Name, "kind": a.Kind, "baseUrl": a.BaseURL, "keyMasked": mask(a.Key), "hasKey": a.Key != "", "model": a.Model, "models": a.Models, "authMode": a.AuthMode, "workspace": a.Workspace, "target": a.Target, "createdAt": a.CreatedAt, "lastUsedAt": a.LastUsedAt, "wire": a.Wire}
}

func (s *Server) handleLocalAccounts(w http.ResponseWriter, r *http.Request) {
	c := s.d.Store.Get()
	accounts := []map[string]any{}
	for _, a := range c.LocalAccounts {
		accounts = append(accounts, maskedAccount(a))
	}
	tools, pending := s.d.Local.InventorySnapshot(c.LocalToolPaths)
	native, nativePending := s.nativeConnectionSnapshot()
	hubState := "not_logged_in"
	if s.d.Hub != nil {
		status := s.d.Hub.Status()
		hubState = status.State
		if c.RemoteDeviceOnly && hubState == "connected" && (status.Pairing == nil || !status.Pairing.Paired) {
			hubState = "unpaired"
		}
	}
	bindings := toolBindings(c, tools, hubState, native)
	writeJSON(w, map[string]any{"accounts": accounts, "activeCodexAccount": c.ActiveCodexAccount, "activeClaudeAccount": c.ActiveClaudeAccount, "tools": tools, "discoveryPending": pending, "nativePending": nativePending, "toolPaths": c.LocalToolPaths, "bindings": bindings, "desktopControl": native.Active, "nativeConnection": native})
}

func (s *Server) handleLocalSave(w http.ResponseWriter, r *http.Request) {
	var a config.LocalAccount
	if err := readJSON(r, &a); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	newAccount := a.ID == ""
	if newAccount {
		a.ID = config.NewUUID()
	}
	err := s.d.Store.Update(func(c *config.Config) error {
		index := -1
		for i, old := range c.LocalAccounts {
			if old.ID == a.ID {
				index = i
				break
			}
		}
		if index < 0 && !newAccount {
			return errors.New("这个账号已被移除，请刷新列表")
		}
		if index >= 0 {
			old := c.LocalAccounts[index]
			if strings.TrimSpace(a.Key) == "" {
				a.Key = old.Key
			}
			a.CreatedAt, a.LastUsedAt = old.CreatedAt, old.LastUsedAt
			if a.Target == "" {
				a.Target = old.Target
			}
			if a.Models == nil {
				a.Models = old.Models
			}
		} else {
			if len(c.LocalAccounts) >= 256 {
				return errors.New("本地最多保存 256 个 API 账号")
			}
			a.CreatedAt, a.LastUsedAt = time.Now().UnixMilli(), 0
		}
		if err := config.ValidateLocalAccount(&a); err != nil {
			return err
		}
		a.Models = cleanModels(a.Models)
		if index >= 0 {
			c.LocalAccounts[index] = a
		} else {
			c.LocalAccounts = append(c.LocalAccounts, a)
		}
		if (a.Kind == "codex" || a.Kind == "api") && c.ActiveCodexAccount == "" {
			c.ActiveCodexAccount = a.ID
		}
		if (a.Kind == "claude" || a.Kind == "api") && c.ActiveClaudeAccount == "" {
			c.ActiveClaudeAccount = a.ID
		}
		return nil
	})
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "account": maskedAccount(a)})
}

func cleanModels(in []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, m := range in {
		m = strings.TrimSpace(m)
		if m != "" && len(m) <= 200 && !strings.ContainsAny(m, "\r\n\x00") && !seen[m] {
			seen[m] = true
			out = append(out, m)
			if len(out) == 1000 {
				break
			}
		}
	}
	return out
}

func readLocalID(w http.ResponseWriter, r *http.Request) (string, bool) {
	var in struct {
		ID string `json:"id"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return "", false
	}
	if !config.ValidAccountID(in.ID) {
		writeErr(w, 400, "无效的账号编号")
		return "", false
	}
	return in.ID, true
}

func (s *Server) handleLocalDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := readLocalID(w, r)
	if !ok {
		return
	}
	err := s.d.Store.Update(func(c *config.Config) error {
		found := false
		for i, a := range c.LocalAccounts {
			if a.ID == id {
				c.LocalAccounts = append(c.LocalAccounts[:i], c.LocalAccounts[i+1:]...)
				found = true
				break
			}
		}
		if !found {
			return errors.New("账号不存在")
		}
		for target, selected := range c.ToolAPISelections {
			if selected == id {
				c.ToolAPISelections[target] = ""
			}
		}
		if c.ActiveCodexAccount == id {
			c.ActiveCodexAccount = ""
		}
		if c.ActiveClaudeAccount == id {
			c.ActiveClaudeAccount = ""
		}
		for _, a := range c.LocalAccounts {
			if a.Kind == "codex" && c.ActiveCodexAccount == "" {
				c.ActiveCodexAccount = a.ID
			}
			if a.Kind == "claude" && c.ActiveClaudeAccount == "" {
				c.ActiveClaudeAccount = a.ID
			}
		}
		return nil
	})
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "message": "已从列表移除账号。正在运行的工具、会话目录和配置备份不会被删除；如需撤销凭据，请在服务商后台撤销 Key。"})
}

func (s *Server) handleLocalSelect(w http.ResponseWriter, r *http.Request) {
	id, ok := readLocalID(w, r)
	if !ok {
		return
	}
	err := s.d.Store.Update(func(c *config.Config) error {
		a, ok := c.LocalAccount(id)
		if !ok {
			return errors.New("账号不存在")
		}
		if a.Kind == "codex" {
			c.ActiveCodexAccount = id
		} else {
			c.ActiveClaudeAccount = id
		}
		return nil
	})
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "message": "默认账号已保存；没有修改系统工具配置"})
}

func (s *Server) handleLocalModels(w http.ResponseWriter, r *http.Request) {
	var in config.LocalAccount
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	saved, exists := s.d.Store.Get().LocalAccount(in.ID)
	if in.ID != "" && !exists {
		writeErr(w, 404, "账号不存在")
		return
	}
	if exists {
		if in.Key == "" {
			in.Key = saved.Key
		}
		if in.BaseURL == "" {
			in.BaseURL = saved.BaseURL
		}
		if in.Kind == "" {
			in.Kind = saved.Kind
		}
		if in.AuthMode == "" {
			in.AuthMode = saved.AuthMode
		}
	} else {
		in.ID = "probe"
	}
	in.Name = "模型读取"
	if err := config.ValidateLocalAccount(&in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	ctx, cancel := ctxTimeout(r, 20*time.Second)
	defer cancel()
	models, err := launcher.Models(ctx, in)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	// Only persist probe results for an unchanged saved provider, not an unsaved edit.
	if exists && in.Key == saved.Key && in.BaseURL == saved.BaseURL && in.Kind == saved.Kind && in.AuthMode == saved.AuthMode {
		if err := s.d.Store.Update(func(c *config.Config) error {
			for i, a := range c.LocalAccounts {
				if a.ID == saved.ID && a.Key == saved.Key && a.BaseURL == saved.BaseURL && a.AuthMode == saved.AuthMode {
					c.LocalAccounts[i].Models = models
				}
			}
			return nil
		}); err != nil {
			writeErr(w, 500, "模型已读取，但保存模型列表失败")
			return
		}
	}
	writeJSON(w, map[string]any{"ok": true, "models": models, "message": "已读取模型目录，不代表所有模型均支持所选工具；此操作没有发起生成请求"})
}

func (s *Server) handleLocalLaunch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID        string `json:"id"`
		Target    string `json:"target"`
		Workspace string `json:"workspace"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if !launcher.ValidTarget(in.Target) {
		writeErr(w, 400, "请选择要启动的工具")
		return
	}
	c := s.d.Store.Get()
	a, ok := c.LocalAccount(in.ID)
	if in.ID == "" {
		a, ok = c.ActiveLocalAccount(strings.Split(in.Target, "-")[0])
	}
	if !ok {
		writeErr(w, 404, "请先添加并选择一个本地 API 账号")
		return
	}
	ctx, cancel := ctxTimeout(r, 45*time.Second)
	defer cancel()
	a = c.APIForTool(a, in.Target)
	if config.NeedsAdapter(a) {
		writeErr(w, 400, "协议转换请使用工具工作台的原地应用；独立实例不能共享尚未应用的路由")
		return
	}
	result, err := s.d.Local.Launch(ctx, a, in.Target, in.Workspace, c.LocalToolPaths)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	// A launch cannot be undone if saving history fails. Report that independently.
	err = s.d.Store.Update(func(c *config.Config) error {
		for i, x := range c.LocalAccounts {
			if x.ID == a.ID {
				c.LocalAccounts[i].LastUsedAt = time.Now().UnixMilli()
				c.LocalAccounts[i].Target = in.Target
			}
		}
		return nil
	})
	if err != nil {
		result.Message += " 最近使用时间未能保存。"
	}
	s.log.Printf("local launcher: target=%s account=%s pid=%d", in.Target, a.ID, result.PID)
	writeJSON(w, map[string]any{"ok": true, "result": result})
}

func (s *Server) handleLocalDefault(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID     string `json:"id"`
		Kind   string `json:"kind"`
		Action string `json:"action"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var err error
	switch in.Action {
	case "apply":
		a, ok := s.d.Store.Get().LocalAccount(in.ID)
		if !ok {
			writeErr(w, 404, "账号不存在")
			return
		}
		err = s.d.Local.ApplyDefault(a)
	case "restore":
		err = s.d.Local.RestoreDefault(in.Kind)
		if err == nil {
			err = s.d.Store.Update(func(c *config.Config) error { delete(c.ToolAPIApplied, in.Kind); return nil })
		}
	default:
		writeErr(w, 400, "无效的配置操作")
		return
	}
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "message": "原工具 API 配置已" + map[string]string{"apply": "应用，请重新打开工具", "restore": "恢复；请重新打开原工具，会话和项目数据未改动"}[in.Action]})
}

func (s *Server) handleLocalToolPaths(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Paths map[string]string `json:"paths"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	for target, path := range in.Paths {
		if !launcher.ValidTarget(target) {
			writeErr(w, 400, "无效的工具名称")
			return
		}
		path = strings.Trim(strings.TrimSpace(path), "\"")
		if path != "" {
			if !filepath.IsAbs(path) {
				writeErr(w, 400, "程序路径需要绝对路径，不能填写命令和参数")
				return
			}
			st, err := os.Stat(path)
			if err != nil || (st.IsDir() && !(runtime.GOOS == "darwin" && strings.HasSuffix(strings.ToLower(path), ".app"))) {
				writeErr(w, 400, "程序不存在，请选择可执行文件或 macOS 的 .app")
				return
			}
		}
		in.Paths[target] = path
	}
	err := s.d.Store.Update(func(c *config.Config) error {
		if c.LocalToolPaths == nil {
			c.LocalToolPaths = map[string]string{}
		}
		for k, v := range in.Paths {
			if v == "" {
				delete(c.LocalToolPaths, k)
			} else {
				c.LocalToolPaths[k] = v
			}
		}
		return nil
	})
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if s.d.Local != nil {
		s.d.Local.InvalidateInventory()
	}
	writeJSON(w, map[string]bool{"ok": true})
}
