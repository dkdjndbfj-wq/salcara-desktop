package console

import (
	"errors"
	"net/http"

	"salcara/bridge/internal/config"
)

// A card already identifies the account. First join creates the managed default
// connection; later joins extend only the explicitly selected connection, keeping
// primary fallbacks, model rules, disabled state and keys intact.
func (s *Server) handleCodexAPIJoin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AccountID      string  `json:"accountId"`
		ClientID       string  `json:"clientId,omitempty"`
		SourceRevision *string `json:"expectedSourceRevision"`
		ClientRevision *string `json:"expectedClientRevision,omitempty"`
	}
	if r.URL.RawQuery != "" || readReverseJSON(r, &in, 4096) != nil || !config.ValidAccountID(in.AccountID) || in.SourceRevision == nil || in.ClientID != "" && !config.ValidAccountID(in.ClientID) {
		writeErr(w, 400, "加入请求格式无效")
		return
	}
	id := in.ClientID
	if id == "" {
		id = config.NewUUID()
	}
	_, err := s.d.Store.UpdateWithSnapshot(func(c *config.Config) error {
		account, ok := c.LocalAccount(in.AccountID)
		if !ok || checkLocalSourceRevision(*c, account, in.SourceRevision) != nil {
			return errCodexAPIMemberChanged
		}
		if in.ClientID == "" {
			if len(c.CodexAPI.Clients) != 0 || in.ClientRevision != nil {
				return errCodexAPIMemberChanged
			}
			client := config.NamedAPIClient{ID: id, Name: "本机服务", AccountID: account.ID, AccountIDs: []string{account.ID}, ImageAccountIDs: []string{account.ID}, KeyDigest: config.APIClientDigest("sk-salcara-client-" + config.RandomToken(32)), MaxConcurrency: 16}
			if err := config.ValidateAPIClient(client); err != nil {
				return err
			}
			c.CodexAPI.Clients = append(c.CodexAPI.Clients, client)
			return nil
		}
		client, ok := c.APIClient(id)
		if !ok || in.ClientRevision == nil || checkAPIMemberRevisions(*c, account, client, in.SourceRevision, in.ClientRevision) != nil {
			return errCodexAPIMemberChanged
		}
		if len(client.AccountIDs) == 0 {
			client.AccountIDs = []string{client.AccountID}
		}
		if len(client.ImageAccountIDs) == 0 {
			client.ImageAccountIDs = []string{client.AccountID}
		}
		client.AccountIDs = toggleMember(client.AccountIDs, account.ID, true)
		client.ImageAccountIDs = toggleMember(client.ImageAccountIDs, account.ID, true)
		if err := config.ValidateAPIClient(client); err != nil {
			return err
		}
		for i := range c.CodexAPI.Clients {
			if c.CodexAPI.Clients[i].ID == id {
				c.CodexAPI.Clients[i] = client
				return nil
			}
		}
		return errCodexAPIMemberChanged
	})
	if errors.Is(err, errCodexAPIMemberChanged) {
		writeErr(w, 409, "账号或服务配置已变化，请重新打开")
		return
	}
	if err != nil {
		writeErr(w, 500, "加入未保存，请重新读取服务状态")
		return
	}
	writeJSON(w, map[string]any{"ok": true, "clientId": id, "accountId": in.AccountID})
}
