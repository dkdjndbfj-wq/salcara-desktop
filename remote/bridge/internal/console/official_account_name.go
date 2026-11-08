package console

import (
	"errors"
	"net/http"

	"salcara/bridge/internal/codexaccounts"
	"salcara/bridge/internal/config"
)

func (s *Server) handleOfficialRename(w http.ResponseWriter, r *http.Request) {
	if s.d.CodexAccounts == nil {
		writeErr(w, http.StatusServiceUnavailable, "官方账号服务尚未就绪")
		return
	}
	var in struct {
		ID            string  `json:"id"`
		Name          *string `json:"name"`
		IdentityEpoch uint64  `json:"identityEpoch"`
	}
	if readReverseJSON(r, &in, 4096) != nil || !config.ValidAccountID(in.ID) || in.Name == nil || in.IdentityEpoch == 0 {
		writeErr(w, http.StatusBadRequest, "名称请求有误")
		return
	}
	row, err := s.d.CodexAccounts.Rename(r.Context(), in.ID, *in.Name, in.IdentityEpoch)
	if err != nil {
		status, message := http.StatusBadRequest, "名称未保存，请重试"
		switch {
		case errors.Is(err, codexaccounts.ErrConflict):
			status, message = http.StatusConflict, "账号已变更，请重新打开"
		case errors.Is(err, codexaccounts.ErrNotFound):
			status, message = http.StatusNotFound, "账号已移除"
		case errors.Is(err, codexaccounts.ErrInvalidImport):
			message = "名称格式无效"
		}
		writeErr(w, status, message)
		return
	}
	// This is metadata only. Do not attach a vault-only account, change source
	// identity, enable a pool member or rewrite tool credentials to rename it.
	err = s.d.Store.Update(func(c *config.Config) error {
		current, getErr := s.d.CodexAccounts.Get(in.ID)
		if getErr != nil || current.IdentityEpoch != row.IdentityEpoch {
			return codexaccounts.ErrConflict
		}
		for i := range c.LocalAccounts {
			a := &c.LocalAccounts[i]
			if a.CredentialRef == current.ID && a.CredentialEpoch == current.IdentityEpoch {
				a.Name = current.Name
			}
		}
		row = current
		return nil
	})
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "saved": true, "error": "名称已保存，列表同步未完成，请刷新后重试"})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "account": row})
}
