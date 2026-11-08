package console

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/usageledger"
)

// A narrow versioned change cannot accidentally overwrite a concurrently
// edited model policy, account pool, key rotation or revocation.
func (s *Server) handleCodexAPIClientEnabled(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID               string `json:"id"`
		Enabled          *bool  `json:"enabled"`
		ExpectedRevision string `json:"expectedRevision"`
	}
	if r.URL.RawQuery != "" || readReverseJSON(r, &in, 4096) != nil || !config.ValidAccountID(in.ID) || in.Enabled == nil || in.ExpectedRevision == "" || len(in.ExpectedRevision) > 256 {
		writeErr(w, 400, "请提交客户端及当前配置版本")
		return
	}
	missing, stale := errors.New("missing"), errors.New("stale")
	committed, err := s.d.Store.UpdateWithSnapshot(func(c *config.Config) error {
		for i := range c.CodexAPI.Clients {
			row := &c.CodexAPI.Clients[i]
			if row.ID != in.ID {
				continue
			}
			if apiClientRevision(*c, *row) != in.ExpectedRevision {
				return stale
			}
			row.Disabled = !*in.Enabled
			return nil
		}
		return missing
	})
	switch {
	case errors.Is(err, missing):
		writeErr(w, 404, "客户端已移除，请刷新后重试")
	case errors.Is(err, stale):
		writeErr(w, 409, "客户端配置已改变，请刷新后重试")
	case err != nil:
		writeErr(w, 500, "启停状态未保存，原客户端状态保留")
	default:
		row, _ := committed.APIClient(in.ID)
		writeJSON(w, map[string]any{"ok": true, "id": in.ID, "enabled": !row.Disabled, "revision": apiClientRevision(committed, row)})
	}
}

// Only the authenticated local console exposes this metadata. The API-service
// LAN listener never registers this endpoint. Names describe current labels;
// historical IDs stay anonymous even when a client or account was removed.
func (s *Server) handleCodexAPIRequests(w http.ResponseWriter, r *http.Request) {
	limit, clientID := 30, ""
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeErr(w, 400, "请求记录筛选无效")
		return
	}
	for name, values := range q {
		if len(values) != 1 {
			writeErr(w, 400, "请求记录筛选无效")
			return
		}
		switch name {
		case "clientId":
			clientID = values[0]
			if !config.ValidAccountID(clientID) {
				writeErr(w, 400, "客户端标识无效")
				return
			}
		case "limit":
			limit, err = strconv.Atoi(values[0])
			if err != nil || limit < 1 || limit > 100 {
				writeErr(w, 400, "读取条数为 1–100")
				return
			}
		default:
			writeErr(w, 400, "请求记录筛选无效")
			return
		}
	}
	if s.usageLedger == nil {
		writeErr(w, 503, "本机请求记录暂不可用")
		return
	}
	f := usageledger.QueryFilter{ClientRequestsOnly: true, Limit: limit}
	if clientID != "" {
		f.ClientID = s.usageLedger.AnonymousClientID(clientID)
	}
	ctx, cancel := ctxTimeout(r, 5*time.Second)
	defer cancel()
	out, err := s.usageLedger.Query(ctx, f)
	if err != nil {
		ledgerQueryError(w, err)
		return
	}
	accounts, clients := map[string]string{}, map[string]string{}
	c := s.d.Store.Get()
	for _, a := range c.LocalAccounts {
		accounts[s.usageLedger.AnonymousAccountID(a.ID)] = a.Name
	}
	for _, client := range c.CodexAPI.Clients {
		clients[s.usageLedger.AnonymousClientID(client.ID)] = client.Name
	}
	writeJSON(w, struct {
		Records      []usageledger.Record `json:"records"`
		AccountNames map[string]string    `json:"accountNames"`
		ClientNames  map[string]string    `json:"clientNames"`
		CheckedAt    int64                `json:"checkedAt"`
		Status       usageledger.Snapshot `json:"status"`
		Total        int                  `json:"total"`
	}{out.Records, accounts, clients, time.Now().UnixMilli(), out.Status, out.Total})
}
