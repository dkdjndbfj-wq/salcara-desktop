package console

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/desktopcompanion"
	"salcara/bridge/internal/hubclient"
	"salcara/bridge/internal/launcher"
	"salcara/bridge/internal/protocol"
)

type remoteLamp struct {
	State     string `json:"state"`
	Label     string `json:"label"`
	Detail    string `json:"detail"`
	Supported bool   `json:"supported"`
}
type toolBinding struct {
	AccountID string      `json:"accountId"`
	AppliedID string      `json:"appliedId,omitempty"`
	Model     string      `json:"model"`
	Protocol  string      `json:"protocol"`
	Pending   bool        `json:"pending"`
	Override  bool        `json:"override"`
	Remote    remoteLamp  `json:"remote"`
	CLIRemote *remoteLamp `json:"cliRemote,omitempty"`
}

func toolBindings(c config.Config, tools []launcher.Tool, hubState string, native ...desktopcompanion.NativeConnection) map[string]toolBinding {
	installed := map[string]bool{}
	for _, t := range tools {
		installed[t.ID] = t.Available
	}
	out := map[string]toolBinding{}
	for _, t := range tools {
		a, _ := c.SelectedToolAPI(t.ID)
		applied, valid := c.AppliedToolAccount(t.ID)
		b := toolBinding{AccountID: a.ID, AppliedID: applied.ID, Model: a.Model, Protocol: a.Protocol, Pending: !valid || applied.ID != a.ID || config.AccountFingerprint(a) != config.AccountFingerprint(applied), Override: a.CatalogOverride}
		// Remote tasks work with the tool's own login; choosing/applying an API is optional.
		lamp := remoteLamp{State: "off", Label: "手机远程未连接", Detail: "在「手机远程」连接中转站并扫码绑定手机", Supported: true}
		backend := t.Kind
		if !installed[backend] {
			lamp = remoteLamp{State: "unsupported", Label: "远程需安装 CLI", Detail: "手机远程通过对应的 Codex / Claude Code 在电脑上继续对话", Supported: false}
		} else if !c.LoggedIn() {
		} else if hubState == hubclient.StateInvalidKey {
			lamp.State, lamp.Label, lamp.Detail = "error", "远程连接被拒绝", "站点拒绝了这台电脑，请在「手机远程」重新连接"
			if c.RemoteDeviceOnly {
				lamp.Label, lamp.Detail = "设备验证失败", "本站拒绝了这台电脑的设备身份，请在「手机远程」检查连接"
			}
		} else if hubState == "unpaired" {
			lamp.State, lamp.Label, lamp.Detail = "off", "手机未绑定", "在「手机远程」扫码绑定手机"
		} else if hubState != hubclient.StateConnected {
			lamp.State, lamp.Label, lamp.Detail = "connecting", "远程连接中", "正在连接中转站"
		} else {
			lamp.State, lamp.Label, lamp.Detail = "connected", "手机可继续", "已配对的手机可以查看并继续这台电脑上的对话"
		}
		if !t.Available {
			lamp.State, lamp.Label, lamp.Detail, lamp.Supported = "unsupported", "工具未安装", "先安装此工具或填写程序路径", false
		}
		b.Remote = lamp
		out[t.ID] = b
	}
	// CLI session continuation is not control of the running desktop window.
	// Keep desktop capability independent of API application and Hub connectivity.
	for _, t := range tools {
		if !strings.HasSuffix(t.ID, "-desktop") {
			continue
		}
		b := out[t.ID]
		detail := "完整桌面消息发送和回答同步尚未接入；Hub 在线或应用 API 不会启用桌面控制"
		if t.ID == "codex-desktop" {
			detail += "。会话定位仅请求在原应用打开，不代表桌面控制"
		} else {
			detail += "。Claude Desktop 会话定位也未接入"
		}
		b.Remote = remoteLamp{State: "unsupported", Label: "桌面远控未接入", Detail: detail, Supported: false}
		if t.ID == "codex-desktop" {
			b.Remote = remoteLamp{State: "pending", Label: "等待 Codex 授权", Detail: "在 Codex Desktop 中明确调用 salcara_desktop_connect 并指定已有会话；安装插件、选择 API 或 Hub 在线均不会自动授权", Supported: false}
			if len(native) > 0 && desktopcompanion.ValidateNativeConnection(native[0], time.Now().UnixMilli()) {
				b.Remote = remoteLamp{State: "connected", Label: "桌面授权已连接", Detail: "实验原生续聊授权有效；仅指定会话可由已配对手机续聊，最多 10 分钟，不支持桌面停止或手机审批", Supported: true}
				if hubState != hubclient.StateConnected {
					b.Remote.State, b.Remote.Label, b.Remote.Detail = "connecting", "桌面已授权 · 服务器未连", "Codex 原生授权有效，但手机通道尚未连上服务器；请检查远程连接"
				}
			}
		}
		if !t.Available {
			b.Remote = remoteLamp{State: "unsupported", Label: "桌面工具未安装", Detail: "先安装此桌面工具；不能用 CLI 卡片的状态代替桌面授权", Supported: false}
		}
		cli := remoteLamp{State: "unsupported", Label: "CLI 未安装", Detail: "安装对应 CLI 后，在 CLI 卡片配置 API 和远程连接；不会代替桌面远控", Supported: false}
		if cliBinding, ok := out[t.Kind]; ok {
			cli = cliBinding.Remote
		}
		b.CLIRemote = &cli
		out[t.ID] = b
	}
	return out
}

func (s *Server) handleLocalBind(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Target   string  `json:"target"`
		ID       string  `json:"id"`
		Model    *string `json:"model"`
		Protocol *string `json:"protocol"`
		Override *bool   `json:"override"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if !launcher.ValidTarget(in.Target) {
		writeErr(w, 400, "无效的工具")
		return
	}
	err := s.d.Store.Update(func(c *config.Config) error {
		if in.ID != "" {
			_, ok := c.LocalAccount(in.ID)
			if !ok {
				return errors.New("密钥条目不存在")
			}
		}
		if c.ToolAPISelections == nil {
			c.ToolAPISelections = map[string]string{}
		}
		if c.ToolModels == nil {
			c.ToolModels = map[string]string{}
		}
		if c.ToolProtocols == nil {
			c.ToolProtocols = map[string]string{}
		}
		if in.Model != nil {
			model := strings.TrimSpace(*in.Model)
			if len(model) > 200 || strings.ContainsAny(model, "\r\n\x00") {
				return errors.New("无效的模型 ID")
			}
			c.ToolModels[in.Target] = model
		}
		if in.Protocol != nil {
			p := *in.Protocol
			if p != "" && p != "responses" && p != "chat" && p != "anthropic" {
				return errors.New("不支持的上游接口协议")
			}
			c.ToolProtocols[in.Target] = p
		}
		if in.Override != nil {
			if c.ToolCatalogOverride == nil {
				c.ToolCatalogOverride = map[string]bool{}
			}
			if *in.Override {
				c.ToolCatalogOverride[in.Target] = true
			} else {
				delete(c.ToolCatalogOverride, in.Target)
			}
		}
		c.ToolAPISelections[in.Target] = in.ID
		return nil
	})
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	message := "已保存；点「打开」后生效"
	if in.Target == "claude-desktop" {
		message = "选择仅保存在 Bridge；Claude Desktop 自动应用暂不可用，请在原应用中手动配置"
	}
	writeJSON(w, map[string]any{"ok": true, "message": message})
}

func (s *Server) handleLocalResume(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Target     string `json:"target"`
		SessionKey string `json:"sessionKey"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if !launcher.ValidTarget(in.Target) {
		writeErr(w, 400, "无效的恢复工具")
		return
	}
	// This entry is explicitly CLI restoration. Claude Desktop's API selection
	// cannot be applied automatically and must not supply the Code credentials.
	target := in.Target
	if target == "claude-desktop" {
		target = "claude"
	}
	kind := strings.Split(target, "-")[0]
	c := s.d.Store.Get()
	a, selected := c.SelectedToolAPI(target)
	applied, valid := c.AppliedToolAccount(target)
	if !selected || !valid || applied.ID != a.ID || config.AccountFingerprint(a) != config.AccountFingerprint(applied) {
		message := "请先在工具卡片重启应用当前 API，再恢复会话"
		if in.Target == "claude-desktop" {
			message = "CLI 恢复会话使用 Claude Code 卡片的 API；请先在 Claude Code 卡片应用当前 API"
		}
		writeErr(w, 409, message)
		return
	}
	m := s.manager()
	if m == nil {
		writeErr(w, 503, "会话读取器还未准备好")
		return
	}
	ctx, cancel := ctxTimeout(r, 45*time.Second)
	defer cancel()
	list, err := hubclient.ListSessions(ctx, m, kind)
	if err != nil {
		writeErr(w, 400, "读取原会话失败，请刷新会话列表")
		return
	}
	// Never accept arbitrary session paths or IDs from the browser.
	sessions, _ := list["sessions"].([]protocol.SessionInfo)
	for _, session := range sessions {
		if session.SessionKey != in.SessionKey {
			continue
		}
		if session.Status == "running" || session.Status == "waiting_approval" {
			writeErr(w, 409, "原会话仍在运行，请先结束原任务，避免两个进程同时续写")
			return
		}
		if session.Cwd == "" {
			writeErr(w, 409, "原会话缺少项目目录，请先检查会话信息，不会改用默认目录新建")
			return
		}
		connection := c.ToolConnection(applied, target, s.d.Port)
		result, err := s.d.Local.Resume(ctx, connection, kind, strings.TrimPrefix(in.SessionKey, kind+":"), session.Cwd, c.LocalToolPaths)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true, "result": result})
		return
	}
	writeErr(w, 404, "原会话不存在或已不在当前列表中；不会改用新建会话")
}
