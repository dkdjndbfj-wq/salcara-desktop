package console

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/usageledger"
)

// Registered only below the local console cookie/origin middleware. These
// routes are deliberately absent from the API-service-only LAN listener.
func (s *Server) usageLedgerQueryRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/local/usage-ledger/query", s.handleUsageLedgerQuery)
	m.HandleFunc("GET /api/local/usage-ledger/pricing", s.handleUsageLedgerPricing)
	m.HandleFunc("POST /api/local/usage-ledger/pricing", s.handleUsageLedgerPricingUpdate)
	m.HandleFunc("POST /api/local/usage-ledger/reprice", s.handleUsageLedgerReprice)
}

func ledgerQueryFilter(r *http.Request) (usageledger.QueryFilter, error) {
	f := usageledger.QueryFilter{}
	for key, values := range r.URL.Query() {
		if len(values) != 1 || len(values[0]) > 200 {
			return f, usageledger.ErrQuery
		}
		v := values[0]
		switch key {
		case "from", "to":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return f, usageledger.ErrQuery
			}
			if key == "from" {
				f.From = n
			} else {
				f.To = n
			}
		case "limit", "offset", "httpStatus":
			n, err := strconv.Atoi(v)
			if err != nil {
				return f, usageledger.ErrQuery
			}
			switch key {
			case "limit":
				if n < 1 {
					return f, usageledger.ErrQuery
				}
				f.Limit = n
			case "offset":
				f.Offset = n
			case "httpStatus":
				f.HTTPStatus = &n
			}
		case "accountId":
			f.AccountID = v
		case "clientId":
			f.ClientID = v
		case "routeId":
			f.RouteID = v
		case "model":
			f.Model = v
		case "modelContains":
			f.ModelContains = v
		case "state":
			f.State = v
		case "serviceTier":
			f.ServiceTier = v
		case "sort":
			f.Sort = v
		case "direction":
			f.Direction = v
		case "revision":
			f.Revision = v
		default:
			return f, usageledger.ErrQuery
		}
	}
	return f, nil
}
func (s *Server) handleUsageLedgerQuery(w http.ResponseWriter, r *http.Request) {
	f, err := ledgerQueryFilter(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	ctx, cancel := ctxTimeout(r, 10*time.Second)
	defer cancel()
	out, err := s.usageLedger.Query(ctx, f)
	if err != nil {
		ledgerQueryError(w, err)
		return
	}
	accounts, clients := map[string]string{}, map[string]string{}
	sources := map[string]ledgerAccountSource{}
	if s.d.Store != nil {
		cfg := s.d.Store.Get()
		for _, a := range cfg.LocalAccounts {
			if id := s.usageLedger.AnonymousAccountID(a.ID); id != "" {
				accounts[id] = a.Name
				sources[id] = ledgerAccountSourceFor(a)
			}
		}
		for _, c := range cfg.CodexAPI.Clients {
			if id := s.usageLedger.AnonymousClientID(c.ID); id != "" {
				clients[id] = c.Name
			}
		}
	}
	writeJSON(w, struct {
		usageledger.QueryResult
		AccountNames   map[string]string              `json:"accountNames"`
		AccountSources map[string]ledgerAccountSource `json:"accountSources"`
		ClientNames    map[string]string              `json:"clientNames"`
	}{out, accounts, sources, clients})
}

// Current local labels only: never an account's historical billing identity.
// Do not expose credentials, paths, query strings or raw provider account IDs.
type ledgerAccountSource struct {
	Kind string `json:"kind"`
	Host string `json:"host,omitempty"`
}

func ledgerAccountSourceFor(a config.LocalAccount) ledgerAccountSource {
	out := ledgerAccountSource{Kind: "api_key"}
	if strings.TrimSpace(a.CredentialRef) != "" || a.SourceType == "official_account" {
		out.Kind = "official_account"
		return out
	}
	u, err := url.Parse(strings.TrimSpace(a.BaseURL))
	if err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Hostname() != "" {
		out.Host = u.Host
	}
	return out
}
func (s *Server) handleUsageLedgerPricing(w http.ResponseWriter, r *http.Request) {
	if len(r.URL.Query()) != 0 {
		writeErr(w, 400, "读取参数有误")
		return
	}
	out, err := s.usageLedger.Pricing()
	if err != nil {
		ledgerQueryError(w, err)
		return
	}
	writeJSON(w, out)
}
func (s *Server) handleUsageLedgerPricingUpdate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ExpectedVersion uint64                   `json:"expectedVersion"`
		Entries         []usageledger.ModelPrice `json:"entries"`
	}
	if readReverseJSON(r, &in, 256<<10) != nil || in.ExpectedVersion == 0 || in.Entries == nil {
		writeErr(w, 400, "请提交明确的价表版本及条目")
		return
	}
	ctx, cancel := ctxTimeout(r, 10*time.Second)
	defer cancel()
	if err := s.usageLedger.ReplacePricing(ctx, in.ExpectedVersion, in.Entries); err != nil {
		ledgerQueryError(w, err)
		return
	}
	out, err := s.usageLedger.Pricing()
	if err != nil {
		ledgerQueryError(w, err)
		return
	}
	writeJSON(w, out)
}
func (s *Server) handleUsageLedgerReprice(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ExpectedVersion uint64 `json:"expectedVersion"`
		Confirmed       bool   `json:"confirmed"`
	}
	if readReverseJSON(r, &in, 4096) != nil || in.ExpectedVersion == 0 || !in.Confirmed {
		writeErr(w, 400, "请确认使用当前价表重算本机历史估值")
		return
	}
	ctx, cancel := ctxTimeout(r, 10*time.Second)
	defer cancel()
	if err := s.usageLedger.Reprice(ctx, in.ExpectedVersion); err != nil {
		ledgerQueryError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "pricingVersion": in.ExpectedVersion})
}
func ledgerQueryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, usageledger.ErrRevision):
		writeErr(w, 409, usageledger.ErrRevision.Error())
	case errors.Is(err, usageledger.ErrPricing), errors.Is(err, usageledger.ErrQuery):
		writeErr(w, 400, err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeErr(w, 503, "本机记录操作结果尚未确认，请刷新状态")
	default:
		writeErr(w, 500, "本机记录操作失败，请查看保存状态；没有重发推理请求")
	}
}
