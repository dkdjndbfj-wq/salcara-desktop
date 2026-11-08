package console

import (
	"net/http"
	"os"
	"time"

	"salcara/bridge/internal/hubclient"
)

// This route is only for the owning desktop updater. Cookie, JSON and
// same-origin checks are inherited from /api; ordinary Quit is unchanged.
func (s *Server) handleUpdatePrepare(w http.ResponseWriter, r *http.Request) {
	if !s.updateOwner(w, r) {
		return
	}
	s.pendMu.Lock()
	pending := len(s.pending) != 0
	s.pendMu.Unlock()
	if pending {
		writeUpdateBusy(w, "还有请求等待处理，请完成后再更新")
		return
	}
	ctx, cancel := ctxTimeout(r, 3*time.Second)
	defer cancel()
	if err := s.d.Hub.PrepareUpdate(ctx); err != nil {
		writeUpdateBusy(w, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func writeUpdateBusy(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusConflict)
	writeJSON(w, map[string]string{"error": message, "code": "update_busy"})
}

func (s *Server) updateOwner(w http.ResponseWriter, r *http.Request) bool {
	var in struct {
		PID     int    `json:"pid"`
		Version string `json:"version"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return false
	}
	return s.updateOwnerIdentity(w, in.PID, in.Version)
}

func (s *Server) updateOwnerIdentity(w http.ResponseWriter, pid int, version string) bool {
	if pid != os.Getpid() || version != s.d.Version || s.d.Hub == nil || s.d.Quit == nil {
		writeErr(w, 409, "无法确认核心身份，未执行更新")
		return false
	}
	return true
}

func (s *Server) handleUpdatePolicyStatus(w http.ResponseWriter, r *http.Request) {
	if s.d.Hub == nil {
		writeErr(w, http.StatusServiceUnavailable, "核心尚未准备好")
		return
	}
	writeJSON(w, map[string]any{"pid": os.Getpid(), "version": s.d.Version, "policy": s.d.Hub.UpdatePolicy()})
}

// Only the same-origin desktop shell may publish a verified release policy.
// Attached shells can identify this core via GET, but installation retains its
// separate ownership and idle checks in the desktop updater.
func (s *Server) handleUpdatePolicy(w http.ResponseWriter, r *http.Request) {
	var in struct {
		PID                     int    `json:"pid"`
		Version                 string `json:"version"`
		Required                *bool  `json:"required"`
		MinimumSupportedVersion string `json:"minimumSupportedVersion"`
		TargetVersion           string `json:"targetVersion"`
		Reason                  string `json:"reason"`
	}
	if err := readJSON(r, &in); err != nil || in.Required == nil {
		writeErr(w, http.StatusBadRequest, "更新兼容策略无效")
		return
	}
	if !s.updateOwnerIdentity(w, in.PID, in.Version) {
		return
	}
	policy := hubclient.UpdatePolicy{Required: *in.Required, MinimumSupportedVersion: in.MinimumSupportedVersion, TargetVersion: in.TargetVersion, Reason: in.Reason}
	if err := s.d.Hub.SetUpdatePolicy(policy); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "policy": s.d.Hub.UpdatePolicy()})
}

func (s *Server) admitUpdateWork(w http.ResponseWriter) bool {
	if s.d.Hub != nil {
		if err := s.d.Hub.CheckUpdateAdmission(); err != nil {
			writeErr(w, http.StatusUpgradeRequired, err.Error())
			return false
		}
	}
	return true
}

func (s *Server) handleUpdateCancel(w http.ResponseWriter, r *http.Request) {
	if !s.updateOwner(w, r) {
		return
	}
	writeJSON(w, map[string]bool{"ok": s.d.Hub.CancelUpdate()})
}

func (s *Server) handleUpdateCommit(w http.ResponseWriter, r *http.Request) {
	if !s.updateOwner(w, r) {
		return
	}
	s.pendMu.Lock()
	pending := len(s.pending) != 0
	s.pendMu.Unlock()
	if pending {
		s.d.Hub.CancelUpdate()
		writeErr(w, 409, "还有请求等待处理，请完成后再更新")
		return
	}
	if err := s.d.Hub.CommitUpdate(); err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go func() { time.Sleep(300 * time.Millisecond); s.d.Quit() }()
}
