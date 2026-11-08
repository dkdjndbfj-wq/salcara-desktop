package console

import (
	"errors"
	"net/http"

	"salcara/bridge/internal/codexaccounts"
	"salcara/bridge/internal/config"
)

// A manually pasted callback is parsed by the same active PKCE session as the
// browser listener. This endpoint never fetches the URL or accepts a bare code.
func (s *Server) handleOfficialLoginCallback(w http.ResponseWriter, r *http.Request) {
	if s.d.CodexAccounts == nil {
		writeErr(w, http.StatusServiceUnavailable, "官方账号服务尚未就绪")
		return
	}
	var in struct {
		ID          string `json:"id"`
		CallbackURL string `json:"callbackURL"`
	}
	if readReverseJSON(r, &in, 24<<10) != nil || !config.ValidAccountID(in.ID) || len(in.CallbackURL) == 0 || len(in.CallbackURL) > 20<<10 {
		writeErr(w, http.StatusBadRequest, "回调格式无效，请粘贴本次登录的完整回调地址")
		return
	}
	if err := s.d.CodexAccounts.SubmitLoginCallback(in.ID, in.CallbackURL); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, codexaccounts.ErrNotFound) || errors.Is(err, codexaccounts.ErrConflict) || errors.Is(err, codexaccounts.ErrClosed) {
			status = http.StatusConflict
		}
		writeErr(w, status, "回调未被接受；请确认本次登录仍在进行，或重新登录")
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
