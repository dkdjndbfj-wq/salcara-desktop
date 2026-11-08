package console

import (
	"errors"
	"net/http"

	"salcara/bridge/internal/codexaccounts"
	"salcara/bridge/internal/config"
)

func (s *Server) handleAccountDailyUsage(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID                     string `json:"id"`
		ExpectedSourceRevision string `json:"expectedSourceRevision"`
	}
	if r.URL.RawQuery != "" || readReverseJSON(r, &in, 4096) != nil || !config.ValidAccountID(in.ID) || in.ExpectedSourceRevision == "" {
		writeErr(w, 400, "刷新请求有误")
		return
	}
	s.readAccountUsage(w, r, in.ID, in.ExpectedSourceRevision)
}

func (s *Server) handleAccountDailyRefresh(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID            string `json:"id"`
		IdentityEpoch uint64 `json:"identityEpoch"`
	}
	if r.URL.RawQuery != "" || readReverseJSON(r, &in, 4096) != nil || !config.ValidAccountID(in.ID) || in.IdentityEpoch == 0 {
		writeErr(w, 400, "刷新请求有误")
		return
	}
	if s.d.CodexAccounts == nil {
		writeErr(w, 503, "官方账号服务尚未就绪")
		return
	}
	usage, err := s.d.CodexAccounts.RefreshUsageForIdentity(r.Context(), in.ID, in.IdentityEpoch)
	if err != nil {
		message := "额度状态暂时不可用"
		status := http.StatusBadRequest
		switch {
		case errors.Is(err, codexaccounts.ErrConflict), errors.Is(err, codexaccounts.ErrNotFound):
			status, message = http.StatusConflict, "账号已变化，请刷新列表"
		case errors.Is(err, codexaccounts.ErrNeedsLogin):
			message = "账号需要重新登录"
		case errors.Is(err, codexaccounts.ErrSaveFailed):
			message = "请先重试保存凭据"
		}
		writeErr(w, status, message)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "id": in.ID, "identityEpoch": in.IdentityEpoch, "usage": usage})
}
