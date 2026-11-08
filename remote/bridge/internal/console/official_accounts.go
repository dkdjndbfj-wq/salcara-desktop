package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"salcara/bridge/internal/codexaccounts"
	"salcara/bridge/internal/config"
)

func (s *Server) officialAccountRoutes(m *http.ServeMux) {
	s.officialCleanupRoutes(m)
	m.HandleFunc("GET /api/local/codex/accounts", func(w http.ResponseWriter, r *http.Request) {
		if s.d.CodexAccounts == nil {
			writeErr(w, 503, "官方账号服务尚未就绪")
			return
		}
		writeJSON(w, map[string]any{"accounts": s.officialAccountDisplays()})
	})
	m.HandleFunc("POST /api/local/codex/import/preview", s.handleOfficialPreview)
	m.HandleFunc("POST /api/local/codex/import/check", s.handleOfficialImportCheck)
	m.HandleFunc("POST /api/local/codex/import", s.handleOfficialImport)
	m.HandleFunc("POST /api/local/codex/attach", s.handleOfficialAttach)
	m.HandleFunc("POST /api/local/codex/rename", s.handleOfficialRename)
	m.HandleFunc("POST /api/local/codex/refresh", s.handleOfficialRefresh)
	m.HandleFunc("POST /api/local/codex/usage/refresh", s.handleOfficialUsageRefresh)
	m.HandleFunc("POST /api/local/codex/subscription/refresh", s.handleOfficialSubscriptionRefresh)
	m.HandleFunc("POST /api/local/codex/usage/reset", s.handleOfficialUsageReset)
	m.HandleFunc("POST /api/local/codex/reset-credit/prepare", s.handleOfficialResetCreditPrepare)
	m.HandleFunc("POST /api/local/codex/reset-credit/status", s.handleOfficialResetCreditStatus)
	m.HandleFunc("POST /api/local/codex/reset-credit/consume", s.handleOfficialResetCreditConsume)
	m.HandleFunc("POST /api/local/codex/retry-save", s.handleOfficialRetrySave)
	m.HandleFunc("POST /api/local/codex/delete", s.handleOfficialDelete)
	m.HandleFunc("POST /api/local/codex/login", s.handleOfficialLogin)
	m.HandleFunc("POST /api/local/codex/login/status", s.handleOfficialLoginStatus)
	m.HandleFunc("POST /api/local/codex/login/cancel", s.handleOfficialLoginCancel)
	m.HandleFunc("POST /api/local/codex/login/callback", s.handleOfficialLoginCallback)
}

type officialImport struct {
	Credentials json.RawMessage `json:"credentials"`
	Files       []string        `json:"files,omitempty"`
	ReplaceID   string          `json:"replaceId,omitempty"`
	Format      string          `json:"format,omitempty"`
	Selection   json.RawMessage `json:"selection,omitempty"`
}

func (s *Server) preparedOfficialImport(w http.ResponseWriter, r *http.Request) (*codexaccounts.PreparedImport, string, bool) {
	if s.d.CodexAccounts == nil {
		writeErr(w, 503, "官方账号服务尚未就绪")
		return nil, "", false
	}
	var in officialImport
	if readReverseJSON(r, &in, 8<<20) != nil {
		writeErr(w, 400, "导入格式无效")
		return nil, "", false
	}
	raw := []byte(in.Credentials)
	var exactFile string
	if json.Unmarshal(in.Credentials, &exactFile) == nil {
		raw = []byte(exactFile)
	}
	var p *codexaccounts.PreparedImport
	var err error
	if in.Files != nil {
		if len(in.Credentials) != 0 {
			writeErr(w, 400, "文件和粘贴内容不能同时导入")
			return nil, "", false
		}
		files := make([][]byte, len(in.Files))
		for i, file := range in.Files {
			files[i] = []byte(file)
		}
		p, err = codexaccounts.ParseImportFiles(files, in.Format)
	} else {
		switch in.Format {
		case "", "json":
			p, err = codexaccounts.ParseImport(raw)
		case "access_token", "refresh_token":
			p, err = codexaccounts.ParseTokenImport(raw, in.Format)
		default:
			err = codexaccounts.ErrInvalidImport
		}
	}
	if err != nil {
		writeErr(w, 400, "导入格式无效")
		return nil, "", false
	}
	if in.ReplaceID != "" && len(p.Preview().Accounts) != 1 {
		writeErr(w, 400, "替换账号只能选择单条导入内容")
		return nil, "", false
	}
	if in.Selection != nil {
		if r.URL.Path != "/api/local/codex/import" {
			writeErr(w, 400, "检查时请使用完整导入内容")
			return nil, "", false
		}
		p, err = selectedOfficialImport(p, in.Selection)
		if err != nil {
			writeErr(w, 400, "请选择有效且不重复的导入账号")
			return nil, "", false
		}
	}
	return p, in.ReplaceID, true
}
func (s *Server) handleOfficialPreview(w http.ResponseWriter, r *http.Request) {
	p, _, ok := s.preparedOfficialImport(w, r)
	if !ok {
		return
	}
	writeJSON(w, p.Preview())
}
func (s *Server) handleOfficialImport(w http.ResponseWriter, r *http.Request) {
	p, replaceID, ok := s.preparedOfficialImport(w, r)
	if !ok {
		return
	}
	rows, err := s.d.CodexAccounts.Import(r.Context(), p, replaceID)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	rows, refreshFailed := refreshImportedCredentials(r.Context(), s.d.CodexAccounts, p, rows)
	if err = s.attachOfficialRows(rows); err != nil {
		// The protected import is durable. Never delete it to "roll back" a
		// separate config-file failure; attach is explicitly retryable.
		writeJSON(w, map[string]any{"ok": false, "saved": true, "accounts": rows, "error": "账号已安全保存，API 列表同步失败，请重试关联"})
		return
	}
	if refreshFailed {
		writeJSON(w, map[string]any{"ok": false, "saved": true, "accounts": rows, "error": "账号已保存，部分 Refresh Token 尚未完成更新；请在账号列表更新凭据或重试保存"})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "accounts": rows})
}
func (s *Server) attachOfficialRows(rows []codexaccounts.Account) error {
	return s.d.Store.Update(func(c *config.Config) error {
		for _, row := range rows {
			current, err := s.d.CodexAccounts.Get(row.ID)
			if err != nil || current.IdentityEpoch != row.IdentityEpoch {
				return errors.New("已导入账号在关联前被移除或替换，请刷新列表")
			}
			row = current
			id := "official-" + row.ID
			if row.Mode == "chatgpt_web_session" {
				// Web sessions can query quota, but are not Codex API credentials.
				// Replacement must also remove an older API association.
				out := c.LocalAccounts[:0]
				removedIDs := []string{id}
				for _, account := range c.LocalAccounts {
					if account.CredentialRef != row.ID {
						out = append(out, account)
					} else {
						removedIDs = append(removedIDs, account.ID)
					}
				}
				c.LocalAccounts = out
				for _, removedID := range removedIDs {
					detachAPIAccount(c, removedID)
				}
				continue
			}
			a, exists := c.LocalAccount(id)
			if !exists {
				a = config.LocalAccount{ID: id, Kind: "api", CreatedAt: time.Now().UnixMilli()}
			}
			a.Name, a.SourceType, a.ProviderID, a.ProviderName = row.Name, "official_account", "openai", "OpenAI"
			a.CredentialRef, a.CredentialEpoch = row.ID, row.IdentityEpoch
			a.BaseURL, a.Key, a.AuthMode, a.Wire = "https://chatgpt.com/backend-api/codex", "protected-"+row.ID, "bearer", "responses"
			if err := config.ValidateLocalAccount(&a); err != nil {
				return err
			}
			if !exists {
				c.LocalAccounts = append(c.LocalAccounts, a)
			} else {
				for i := range c.LocalAccounts {
					if c.LocalAccounts[i].ID == id {
						c.LocalAccounts[i] = a
						break
					}
				}
			}
		}
		return nil
	})
}
func (s *Server) officialID(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.d.CodexAccounts == nil {
		writeErr(w, 503, "官方账号服务尚未就绪")
		return "", false
	}
	var in struct {
		ID string `json:"id"`
	}
	if readReverseJSON(r, &in, 4096) != nil || !config.ValidAccountID(in.ID) {
		writeErr(w, 400, "请求格式有误")
		return "", false
	}
	return in.ID, true
}
func (s *Server) handleOfficialAttach(w http.ResponseWriter, r *http.Request) {
	id, ok := s.officialID(w, r)
	if !ok {
		return
	}
	row, err := s.d.CodexAccounts.Get(id)
	if err == nil && row.Mode == "chatgpt_web_session" {
		writeErr(w, 400, "网页会话仅用于查询额度，请通过 OAuth 登录后加入 API 服务")
		return
	}
	if err == nil {
		err = s.attachOfficialRows([]codexaccounts.Account{row})
	}
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "account": row})
}
func (s *Server) handleOfficialRefresh(w http.ResponseWriter, r *http.Request) {
	id, ok := s.officialID(w, r)
	if !ok {
		return
	}
	row, err := s.d.CodexAccounts.Refresh(r.Context(), id)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "account": row})
}

func (s *Server) handleOfficialUsageRefresh(w http.ResponseWriter, r *http.Request) {
	id, ok := s.officialID(w, r)
	if !ok {
		return
	}
	usage, err := s.d.CodexAccounts.RefreshUsage(r.Context(), id)
	if err != nil {
		message := "额度状态暂时不可用"
		switch {
		case errors.Is(err, codexaccounts.ErrNeedsLogin):
			message = "账号需要重新登录后才能刷新额度"
		case errors.Is(err, codexaccounts.ErrConflict):
			message = "账号已变更，请刷新列表后重试"
		case errors.Is(err, codexaccounts.ErrSaveFailed):
			message = "凭据更新尚未安全保存，请先重试保存"
		case errors.Is(err, codexaccounts.ErrNotFound):
			message = "账号已移除"
		}
		writeJSON(w, map[string]any{"ok": false, "usage": usage, "error": message})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "usage": usage})
}

func (s *Server) handleOfficialUsageReset(w http.ResponseWriter, r *http.Request) {
	id, ok := s.officialID(w, r)
	if !ok {
		return
	}
	usage, err := s.d.CodexAccounts.ResetUsage(id)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	// Clear only local scheduler backoff. In-flight requests retain their
	// leases; this action never revokes credentials or claims to reset
	// provider-side credits.
	localID := "official-" + id
	cleared := false
	if s.accountPool != nil {
		cleared = s.accountPool.ResetHealth(localID)
	}
	writeJSON(w, map[string]any{"ok": true, "usage": usage, "healthReset": cleared, "message": "已清除本地额度缓存和账号冷却状态；官方额度不会被本地重置，请刷新额度获取最新数据"})
}

func resetCreditErrorMessage(err error) string {
	switch codexaccounts.ResetCreditErrorCode(err) {
	case "needs_login":
		return "账号需要重新登录；官方重置次数未确认是否消费"
	case "account_changed":
		return "账号已变更，请刷新列表后重试"
	case "result_unknown":
		return "上游结果未知，请先刷新额度确认，勿重复提交"
	case "api_key_unsupported":
		return "API Key 账号不支持官方额度重置"
	case "rate_limited":
		return "上游暂时限流，请稍后重试"
	case "usage_refresh_required":
		return "请刷新官方额度，确认上次重置后的可用次数"
	case "no_reset_credits":
		return "当前没有可用的官方重置次数"
	case "previous_not_completed":
		return "请先确认上一次重置结果，不能另建重置操作"
	default:
		return "官方额度重置未完成"
	}
}

func (s *Server) handleOfficialResetCreditPrepare(w http.ResponseWriter, r *http.Request) {
	if s.d.CodexAccounts == nil {
		writeErr(w, 503, "官方账号服务尚未就绪")
		return
	}
	var in struct {
		ID                      string  `json:"id"`
		PreviousRedeemRequestID *string `json:"previousRedeemRequestId"`
	}
	if readReverseJSON(r, &in, 4096) != nil || !config.ValidAccountID(in.ID) {
		writeErr(w, 400, "请求格式有误")
		return
	}
	var request codexaccounts.ResetCreditRequest
	var err error
	if in.PreviousRedeemRequestID != nil {
		request, err = s.d.CodexAccounts.PrepareNextResetCredit(r.Context(), in.ID, *in.PreviousRedeemRequestID)
	} else {
		request, err = s.d.CodexAccounts.PrepareResetCredit(r.Context(), in.ID)
	}
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"ok": false, "request": request, "error": resetCreditErrorMessage(err), "code": codexaccounts.ResetCreditErrorCode(err)})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "request": request, "warning": "确认后会向 OpenAI 消费一次官方额度重置次数；这不是清除本机缓存。"})
}

func (s *Server) handleOfficialResetCreditStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := s.officialID(w, r)
	if !ok {
		return
	}
	request, err := s.d.CodexAccounts.ResetCreditStatus(id)
	if err != nil {
		writeErr(w, 404, resetCreditErrorMessage(err))
		return
	}
	writeJSON(w, map[string]any{"ok": true, "request": request})
}

func (s *Server) handleOfficialResetCreditConsume(w http.ResponseWriter, r *http.Request) {
	if s.d.CodexAccounts == nil {
		writeErr(w, 503, "官方账号服务尚未就绪")
		return
	}
	var in struct {
		ID              string `json:"id"`
		RedeemRequestID string `json:"redeemRequestId"`
	}
	if readReverseJSON(r, &in, 4096) != nil || !config.ValidAccountID(in.ID) || strings.TrimSpace(in.RedeemRequestID) == "" {
		writeErr(w, 400, "请求格式有误")
		return
	}
	request, err := s.d.CodexAccounts.ConsumeResetCredit(r.Context(), in.ID, in.RedeemRequestID)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "request": request, "error": resetCreditErrorMessage(err), "code": codexaccounts.ResetCreditErrorCode(err)})
		return
	}
	if s.accountPool != nil {
		s.accountPool.ResetHealth("official-" + in.ID)
	}
	writeJSON(w, map[string]any{"ok": true, "request": request, "message": "已消费一次官方额度重置次数；请刷新额度确认结果"})
}
func (s *Server) handleOfficialRetrySave(w http.ResponseWriter, r *http.Request) {
	id, ok := s.officialID(w, r)
	if !ok {
		return
	}
	row, err := s.d.CodexAccounts.RetrySave(r.Context(), id)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "account": row})
}
func (s *Server) handleOfficialDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := s.officialID(w, r)
	if !ok {
		return
	}
	if err := s.d.CodexAccounts.Delete(r.Context(), id); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	// Delete credentials first, so a config-write failure cannot continue
	// executing a revoked account. The leftover card is safe and retryable.
	err := s.d.Store.Update(func(c *config.Config) error {
		out := c.LocalAccounts[:0]
		for _, a := range c.LocalAccounts {
			if a.CredentialRef != id {
				out = append(out, a)
			}
		}
		c.LocalAccounts = out
		detachAPIAccount(c, "official-"+id)
		return nil
	})
	if err != nil {
		writeErr(w, 500, "凭据已移除，API 列表同步失败")
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
func (s *Server) handleOfficialLogin(w http.ResponseWriter, r *http.Request) {
	if s.d.CodexAccounts == nil {
		writeErr(w, 503, "官方账号服务尚未就绪")
		return
	}
	var in codexaccounts.LoginRequest
	if readReverseJSON(r, &in, 4096) != nil {
		writeErr(w, 400, "请求格式有误")
		return
	}
	// Service.Close and explicit CancelLogin own this async operation. It must
	// not die when the short HTTP request finishes.
	row, err := s.d.CodexAccounts.StartLogin(context.Background(), in)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, row)
}
func (s *Server) handleOfficialLoginStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := s.officialID(w, r)
	if !ok {
		return
	}
	row, err := s.d.CodexAccounts.LoginStatus(id)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if row.Account != nil {
		// A completed login is a cached result, not authorization to recreate a
		// deleted card or replace a newer protected identity on a later poll.
		current, currentErr := s.d.CodexAccounts.Get(row.Account.ID)
		if currentErr != nil || current.IdentityEpoch != row.Account.IdentityEpoch {
			writeErr(w, 409, "账号已移除或变更，请刷新列表")
			return
		}
		row.Account = &current
		if err = s.attachOfficialRows([]codexaccounts.Account{current}); err != nil {
			writeErr(w, 500, "账号已安全保存，API 列表同步失败")
			return
		}
	}
	writeJSON(w, row)
}
func (s *Server) handleOfficialLoginCancel(w http.ResponseWriter, r *http.Request) {
	id, ok := s.officialID(w, r)
	if !ok {
		return
	}
	s.d.CodexAccounts.CancelLogin(id)
	writeJSON(w, map[string]any{"ok": true})
}
