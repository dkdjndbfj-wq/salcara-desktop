package console

import (
	"net/http"
	"os"
	"time"
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
		writeErr(w, 409, "还有请求等待处理，请完成后再更新")
		return
	}
	ctx, cancel := ctxTimeout(r, 3*time.Second)
	defer cancel()
	if err := s.d.Hub.PrepareUpdate(ctx); err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
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
	if in.PID != os.Getpid() || in.Version != s.d.Version || s.d.Hub == nil || s.d.Quit == nil {
		writeErr(w, 409, "无法确认核心身份，未执行更新")
		return false
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
