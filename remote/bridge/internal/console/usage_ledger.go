package console

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"salcara/bridge/internal/usageledger"
)

func newUsageLedger(d Deps) *usageledger.Ledger {
	dir := d.Dir
	if dir == "" && d.Store != nil {
		dir = filepath.Dir(d.Store.Path())
	}
	if dir == "" {
		return nil
	}
	return usageledger.Open(filepath.Join(dir, "request-usage-ledger.json"))
}

func (s *Server) observeUsage(o usageledger.Observation) {
	if s.usageLedger != nil {
		s.usageLedger.Append(o)
	}
}
func (s *Server) CloseUsageLedger() {
	if s.usageLedger == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if s.usageLedger.Close(ctx) != nil {
		s.log.Print("本机请求记录未能完整保存，请查看用量记录状态")
	}
}

func (s *Server) usageLedgerRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/local/usage-ledger", s.handleUsageLedger)
	m.HandleFunc("POST /api/local/usage-ledger/clear", s.handleUsageLedgerClear)
	m.HandleFunc("POST /api/local/usage-ledger/retention", s.handleUsageLedgerRetention)
	m.HandleFunc("POST /api/local/usage-ledger/retry-save", s.handleUsageLedgerRetry)
}

func (s *Server) handleUsageLedger(w http.ResponseWriter, r *http.Request) {
	limit := 100
	before := ""
	for name, values := range r.URL.Query() {
		if len(values) != 1 {
			writeErr(w, 400, "读取参数有误")
			return
		}
		switch name {
		case "limit":
			value, err := strconv.Atoi(values[0])
			if err != nil || value < 1 || value > 200 {
				writeErr(w, 400, "读取条数为 1–200")
				return
			}
			limit = value
		case "before":
			if len(values[0]) > 32 {
				writeErr(w, 400, "记录游标有误")
				return
			}
			before = values[0]
		default:
			writeErr(w, 400, "读取参数有误")
			return
		}
	}
	out, err := s.usageLedger.Snapshot(limit, before)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	accounts, clients := map[string]string{}, map[string]string{}
	if s.d.Store != nil {
		cfg := s.d.Store.Get()
		for _, account := range cfg.LocalAccounts {
			if id := s.usageLedger.AnonymousAccountID(account.ID); id != "" {
				accounts[id] = account.Name
			}
		}
		for _, client := range cfg.CodexAPI.Clients {
			if id := s.usageLedger.AnonymousClientID(client.ID); id != "" {
				clients[id] = client.Name
			}
		}
	}
	writeJSON(w, struct {
		usageledger.Snapshot
		AccountNames map[string]string `json:"accountNames"`
		ClientNames  map[string]string `json:"clientNames"`
	}{out, accounts, clients})
}
func (s *Server) handleUsageLedgerClear(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Confirmed bool `json:"confirmed"`
	}
	if readReverseJSON(r, &in, 4096) != nil || !in.Confirmed {
		writeErr(w, 400, "请确认清空本机请求记录")
		return
	}
	ctx, cancel := ctxTimeout(r, 10*time.Second)
	defer cancel()
	if err := s.usageLedger.Clear(ctx); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			writeErr(w, 500, "清空结果尚未确认，请刷新本机请求记录状态")
			return
		}
		writeErr(w, 500, "清空记录保存失败；原记录仍保留，请重试")
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
func (s *Server) handleUsageLedgerRetention(w http.ResponseWriter, r *http.Request) {
	var in usageledger.Retention
	if readReverseJSON(r, &in, 4096) != nil {
		writeErr(w, 400, "请求格式有误")
		return
	}
	if in.MaxRecords < 1 || in.MaxRecords > usageledger.MaxRecords || in.Days < 1 || in.Days > 365 {
		writeErr(w, 400, "保留条数为 1–4096，保留天数为 1–365")
		return
	}
	ctx, cancel := ctxTimeout(r, 10*time.Second)
	defer cancel()
	if err := s.usageLedger.SetRetention(ctx, in); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			writeErr(w, 500, "保留设置保存结果尚未确认，请刷新本机请求记录状态")
			return
		}
		writeErr(w, 500, "保留设置保存失败；原设置仍保留，请重试")
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
func (s *Server) handleUsageLedgerRetry(w http.ResponseWriter, r *http.Request) {
	var in struct{}
	if readReverseJSON(r, &in, 4096) != nil {
		writeErr(w, 400, "请求格式有误")
		return
	}
	ctx, cancel := ctxTimeout(r, 10*time.Second)
	defer cancel()
	if err := s.usageLedger.Flush(ctx); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			writeErr(w, 500, "保存结果尚未确认，请刷新本机请求记录状态")
			return
		}
		writeErr(w, 500, "本机请求记录仍未保存，请检查磁盘权限；没有重发推理请求")
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
