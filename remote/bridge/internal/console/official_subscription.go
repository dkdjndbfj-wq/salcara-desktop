package console

import (
	"context"
	"errors"
	"net/http"

	"salcara/bridge/internal/codexaccounts"
	"salcara/bridge/internal/config"
)

func (s *Server) handleOfficialSubscriptionRefresh(w http.ResponseWriter, r *http.Request) {
	if s.d.CodexAccounts == nil {
		writeErr(w, http.StatusServiceUnavailable, "官方账号服务尚未就绪")
		return
	}
	var in struct {
		ID            string `json:"id"`
		IdentityEpoch uint64 `json:"identityEpoch"`
	}
	if readReverseJSON(r, &in, 4096) != nil || !config.ValidAccountID(in.ID) || in.IdentityEpoch == 0 {
		writeErr(w, http.StatusBadRequest, "请求格式有误，请刷新账号列表后重试")
		return
	}
	snapshot, err := s.d.CodexAccounts.RefreshSubscriptionForIdentity(r.Context(), in.ID, in.IdentityEpoch)
	if err != nil {
		message := "官方未返回可确认的订阅到期时间，订阅状态仍为未知"
		switch {
		case errors.Is(err, codexaccounts.ErrNeedsLogin):
			message = "账号需要重新登录后才能核验订阅"
		case errors.Is(err, codexaccounts.ErrConflict):
			message = "账号已变更，请刷新列表后重试"
		case errors.Is(err, codexaccounts.ErrSaveFailed):
			message = "凭据更新尚未安全保存，请先重试保存"
		case errors.Is(err, codexaccounts.ErrNotFound):
			message = "账号已移除"
		case errors.Is(err, context.DeadlineExceeded):
			message = "订阅核验超时，请稍后重试"
		case errors.Is(err, context.Canceled):
			message = "订阅核验已取消"
		}
		writeJSON(w, map[string]any{"ok": false, "subscription": snapshot, "error": message})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "subscription": snapshot})
}
