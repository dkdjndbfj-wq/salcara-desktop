package console

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"salcara/bridge/internal/pluginhook"
	"salcara/bridge/internal/protocol"
)

func (s *Server) pluginRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/local/plugins/connect", s.handlePluginConnect)
	mux.HandleFunc("POST /api/local/plugins/disconnect", s.handlePluginDisconnect)
}
func (s *Server) handlePluginConnect(w http.ResponseWriter, r *http.Request) {
	if !isLoopback(r.RemoteAddr) {
		writeErr(w, http.StatusForbidden, "仅允许本机桌面插件连接")
		return
	}
	var input struct {
		Port  int    `json:"port"`
		Token string `json:"token"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&input) != nil || s.pluginHooks == nil {
		writeErr(w, http.StatusBadRequest, "插件连接参数无效")
		return
	}
	if err := s.pluginHooks.Configure(input.Port, input.Token); err != nil {
		writeErr(w, http.StatusBadRequest, "插件连接参数无效")
		return
	}
	writeJSON(w, map[string]any{"ok": true, "leaseSeconds": int(pluginhook.LeaseDuration / time.Second)})
}
func (s *Server) handlePluginDisconnect(w http.ResponseWriter, r *http.Request) {
	if !isLoopback(r.RemoteAddr) {
		writeErr(w, http.StatusForbidden, "仅允许本机桌面插件连接")
		return
	}
	var input struct {
		Token string `json:"token"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&input) != nil {
		writeErr(w, http.StatusBadRequest, "插件连接参数无效")
		return
	}
	disconnected := s.pluginHooks.Disconnect(input.Token)
	writeJSON(w, map[string]any{"ok": true, "disconnected": disconnected})
}

// Lifecycle subscriptions receive metadata, not arbitrary tool output, commands,
// approval credentials or raw image data from the underlying event envelope.
func (s *Server) publishPluginEvent(ev protocol.Event) {
	if !s.pluginHooks.Enabled() {
		return
	}
	name := ""
	switch ev.Type {
	case "session.updated":
		name = "session:update"
	case "turn":
		switch ev.Status {
		case "started":
			name = "session:start"
		case "completed", "failed", "interrupted", "cancelled":
			name = "session:end"
		}
	}
	if name == "" {
		return
	}
	data := map[string]any{"type": ev.Type, "sessionKey": ev.SessionKey, "tool": ev.Tool, "ts": ev.TS, "status": ev.Status, "turnId": ev.TurnID}
	if ev.Session != nil {
		info := ev.Session
		data["sessionKey"], data["tool"], data["status"] = info.SessionKey, info.Tool, info.Status
		data["title"], data["updatedAt"], data["model"] = info.Title, info.UpdatedAt, info.Model
	}
	s.pluginHooks.FireEvent(context.Background(), name, data)
}
func (s *Server) publishPluginTools(items []setupItem, pending bool) {
	if !s.pluginHooks.Enabled() {
		return
	}
	tools := make([]map[string]any, 0, len(items))
	for _, item := range items {
		tools = append(tools, map[string]any{"id": item.ID, "name": item.Name, "installed": item.Installed, "version": item.Version})
	}
	s.pluginHooks.FireEvent(context.Background(), "tool:detect", map[string]any{"tools": tools, "discoveryPending": pending})
}
