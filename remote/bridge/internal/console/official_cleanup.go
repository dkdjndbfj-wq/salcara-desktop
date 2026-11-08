package console

import (
	"encoding/json"
	"net/http"

	"salcara/bridge/internal/codexaccounts"
	"salcara/bridge/internal/config"
)

func (s *Server) officialCleanupRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /api/local/codex/cleanup/preview", s.handleOfficialCleanupPreview)
	m.HandleFunc("POST /api/local/codex/cleanup", s.handleOfficialCleanup)
}

func (s *Server) handleOfficialCleanupPreview(w http.ResponseWriter, r *http.Request) {
	if s.d.CodexAccounts == nil {
		writeErr(w, 503, "官方账号服务尚未就绪")
		return
	}
	rows, err := s.d.CodexAccounts.CleanupPreview(r.Context())
	if err != nil {
		writeErr(w, 400, "无法读取清理候选，请稍后重试")
		return
	}
	writeJSON(w, map[string]any{"accounts": rows})
}

func (s *Server) handleOfficialCleanup(w http.ResponseWriter, r *http.Request) {
	if s.d.CodexAccounts == nil {
		writeErr(w, 503, "官方账号服务尚未就绪")
		return
	}
	var request struct {
		Accounts []json.RawMessage `json:"accounts"`
	}
	if readReverseJSON(r, &request, 32768) != nil || len(request.Accounts) == 0 || len(request.Accounts) > codexaccounts.MaxAccounts {
		writeErr(w, 400, "请求格式有误")
		return
	}
	identities := make([]codexaccounts.CleanupIdentity, 0, len(request.Accounts))
	for _, raw := range request.Accounts {
		var fields map[string]json.RawMessage
		var item codexaccounts.CleanupIdentity
		// Exact nested keys too: encoding/json's case-insensitive matching is
		// insufficient for a destructive compare-and-swap request.
		if json.Unmarshal(raw, &fields) != nil || (len(fields) != 3 && len(fields) != 4) || fields["id"] == nil || fields["epoch"] == nil || fields["identityEpoch"] == nil || (len(fields) == 4 && fields["revision"] == nil) || json.Unmarshal(raw, &item) != nil || (fields["revision"] != nil && len(item.Revision) != 64) {
			writeErr(w, 400, "请求格式有误")
			return
		}
		identities = append(identities, item)
	}
	result, err := s.d.CodexAccounts.Cleanup(r.Context(), identities)
	if err != nil {
		writeErr(w, 400, "本次清理未完成，请刷新后重试")
		return
	}
	// Credentials are already durably revoked. Config failure may leave a
	// harmless stale card, never a live credential or deletion of a reimport.
	syncFailed := false
	if len(result.Deleted) > 0 {
		if s.d.Store == nil {
			syncFailed = true
		} else {
			err = s.d.Store.Update(func(c *config.Config) error {
				removeCleanedOfficialReferences(c, result.Deleted)
				return nil
			})
			syncFailed = err != nil
		}
	}
	writeJSON(w, map[string]any{"ok": true, "deleted": result.Deleted, "skipped": result.Skipped, "syncFailed": syncFailed})
}

func removeCleanedOfficialReferences(c *config.Config, deleted []codexaccounts.CleanupCandidate) {
	versions := map[string]uint64{}
	for _, item := range deleted {
		versions[item.ID] = item.IdentityEpoch
	}
	retained := make([]config.LocalAccount, 0, len(c.LocalAccounts))
	for _, account := range c.LocalAccounts {
		if epoch, ok := versions[account.CredentialRef]; ok && epoch == account.CredentialEpoch && account.SourceType == "official_account" && account.ProviderID == "openai" {
			detachAPIAccount(c, account.ID)
			continue
		}
		retained = append(retained, account)
	}
	c.LocalAccounts = retained
}
