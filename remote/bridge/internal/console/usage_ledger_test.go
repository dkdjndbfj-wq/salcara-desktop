package console

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/usageledger"
)

type consoleLedgerView struct {
	usageledger.Snapshot
	AccountNames map[string]string `json:"accountNames"`
	ClientNames  map[string]string `json:"clientNames"`
}

func consoleLedgerSnapshot(t *testing.T, s *Server, h http.Handler) consoleLedgerView {
	t.Helper()
	w := localRequest(s, h, "GET", "/api/local/usage-ledger", nil)
	if w.Code != 200 {
		t.Fatalf("ledger read: %d %s", w.Code, w.Body)
	}
	var out consoleLedgerView
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func flushConsoleLedger(t *testing.T, s *Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.usageLedger.Flush(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestUsageLedgerRequiresConsoleCookieAndStrictMutations(t *testing.T) {
	s, h := newTestServer(t)
	for _, path := range []string{"/api/local/usage-ledger", "/api/local/usage-ledger/clear", "/api/local/usage-ledger/retention", "/api/local/usage-ledger/retry-save"} {
		method := "POST"
		if path == "/api/local/usage-ledger" {
			method = "GET"
		}
		if w := do(h, method, path, "127.0.0.1:47831", nil, `{}`); w.Code != 401 {
			t.Fatal("ledger bypassed cookie authentication")
		}
	}
	for _, query := range []string{"?limit=201", "?limit=1&limit=2", "?unknown=value", "?before=" + strings.Repeat("x", 33)} {
		if w := localRequest(s, h, "GET", "/api/local/usage-ledger"+query, nil); w.Code != 400 {
			t.Fatalf("ambiguous read accepted: %s", query)
		}
	}
	for _, body := range []string{`{}`, `{"confirmed":false}`, `{"confirmed":true,"Confirmed":true}`, `{"confirmed":true,"prompt":"secret"}`, `{"confirmed":true} {}`} {
		if w := do(h, "POST", "/api/local/usage-ledger/clear", "127.0.0.1:47831", map[string]string{"Cookie": CookieName + "=" + s.Token(), "Content-Type": "application/json"}, body); w.Code != 400 {
			t.Fatalf("ambiguous clear accepted: %s", body)
		}
	}
	if w := do(h, "POST", "/api/local/usage-ledger/clear", "127.0.0.1:47831", map[string]string{"Cookie": CookieName + "=" + s.Token(), "Content-Type": "application/json", "Origin": "https://evil.example"}, `{"confirmed":true}`); w.Code != 403 {
		t.Fatal("cross-origin ledger clear accepted")
	}
	for _, asset := range []string{"/usage-ledger.js", "/usage-ledger.css"} {
		if w := do(h, "GET", asset, "127.0.0.1:47831", nil, ""); w.Code != 200 {
			t.Fatalf("ledger UI asset missing: %s", asset)
		}
	}
}

func TestProductionGatewayAndNamedAPISharePersistentAnonymousLedger(t *testing.T) {
	s, _ := newTestServer(t)
	s.d.Store.SetRuntimeGatewayKey("synthetic-ledger-runtime")
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"response","status":"completed","model":"gpt-ledger","output":[],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}`)
	}))
	defer u.Close()
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.LocalAccounts = []config.LocalAccount{{ID: "private-raw-account-id", Name: "显示账号 A <img src=x>", Kind: "api", SourceType: "api_key", BaseURL: u.URL, Key: "synthetic-private-provider-key", Model: "gpt-ledger", Models: []string{"gpt-ledger"}}}
		c.RecordAppliedAPI("codex", c.APIForTool(c.LocalAccounts[0], "codex"), "fixture")
		c.CodexAPI = config.CodexAPIService{Enabled: true, RuntimeLease: c.GatewayRuntimeKey, Clients: []config.NamedAPIClient{{ID: "private-raw-client-id", Name: "显示客户端 A", AccountID: c.LocalAccounts[0].ID, KeyDigest: config.APIClientDigest("synthetic-private-client-key")}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	for _, path := range []string{"/gateway/codex/v1/responses", "/codex-api/v1/responses"} {
		key := s.d.Store.Get().LocalGatewayCredential("codex")
		if strings.HasPrefix(path, "/codex-api/") {
			key = "synthetic-private-client-key"
		}
		r := httptest.NewRequest("POST", path, strings.NewReader(`{"model":"gpt-ledger","input":"synthetic private prompt"}`))
		r.Host = "127.0.0.1:47831"
		r.RemoteAddr = "127.0.0.1:13000"
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("inference %s: %d %s", path, w.Code, w.Body)
		}
	}
	flushConsoleLedger(t, s)
	out := consoleLedgerSnapshot(t, s, h)
	if out.Count != 2 || out.Unsaved {
		t.Fatalf("production observer wiring lost/duplicated records: %+v", out)
	}
	for _, row := range out.Records {
		if out.AccountNames[row.AccountID] != "显示账号 A <img src=x>" || row.Model != "gpt-ledger" || row.Tokens.Total == nil || *row.Tokens.Total != 10 {
			t.Fatal("authenticated account labels or real provider usage missing")
		}
	}
	if out.ClientNames[out.Records[0].ClientID] != "显示客户端 A" {
		t.Fatal("named client label not resolved")
	}
	path := filepath.Join(filepath.Dir(s.d.Store.Path()), "request-usage-ledger.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-raw-account-id", "private-raw-client-id", "synthetic-private-provider-key", "synthetic-private-client-key", "synthetic private prompt", "显示账号", "显示客户端", u.URL} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("persistent ledger contains private data: %s", secret)
		}
	}
	if err := s.d.Store.Update(func(c *config.Config) error { c.LocalAccounts = nil; c.CodexAPI.Clients = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	out = consoleLedgerSnapshot(t, s, h)
	if out.Count != 2 || len(out.AccountNames) != 0 || len(out.ClientNames) != 0 {
		t.Fatal("deleted accounts were cached into display names or erased historical records")
	}
	if w := localRequest(s, h, "POST", "/api/local/usage-ledger/retention", map[string]any{"maxRecords": 1, "days": 1}); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if consoleLedgerSnapshot(t, s, h).Count != 1 {
		t.Fatal("retention control not applied")
	}
	if w := localRequest(s, h, "POST", "/api/local/usage-ledger/clear", map[string]bool{"confirmed": true}); w.Code != 200 || consoleLedgerSnapshot(t, s, h).Count != 0 {
		t.Fatal("clear control not applied")
	}
}

func TestConsoleUsageLedgerSaveFailuresAreVisibleAndRetryIsLocalOnly(t *testing.T) {
	s, h := newTestServer(t)
	s.observeUsage(usageledger.Observation{StartedAt: time.Now(), AccountID: "synthetic", Route: "codex/v1/responses", State: "completed", HTTPStatus: 200})
	flushConsoleLedger(t, s)
	path := filepath.Join(filepath.Dir(s.d.Store.Path()), "request-usage-ledger.json")
	if os.Remove(path) != nil || os.Mkdir(path, 0o700) != nil {
		t.Fatal("could not block ledger write")
	}
	for _, mutation := range []struct {
		path string
		body any
	}{{"/api/local/usage-ledger/clear", map[string]bool{"confirmed": true}}, {"/api/local/usage-ledger/retention", map[string]int{"maxRecords": 1, "days": 1}}, {"/api/local/usage-ledger/retry-save", map[string]any{}}} {
		if w := localRequest(s, h, "POST", mutation.path, mutation.body); w.Code != 500 {
			t.Fatal("write failure hidden")
		}
	}
	out := consoleLedgerSnapshot(t, s, h)
	if out.Count != 1 || out.Retention.MaxRecords != 1000 || out.SaveError == "" || !out.Unsaved {
		t.Fatal("failed save changed records/retention or hid failure")
	}
	s.observeUsage(usageledger.Observation{StartedAt: time.Now(), Route: "codex/v1/responses", State: "interrupted", HTTPStatus: 200})
	if s.usageLedger.Flush(context.Background()) == nil {
		t.Fatal("blocked persistence unexpectedly succeeded")
	}
	if os.Remove(path) != nil {
		t.Fatal("could not release ledger destination")
	}
	if w := localRequest(s, h, "POST", "/api/local/usage-ledger/retry-save", map[string]any{}); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if out := consoleLedgerSnapshot(t, s, h); out.SaveError != "" || out.Unsaved || out.Count != 2 {
		t.Fatal("retry did not save retained local observations")
	}
}

func TestCancelledConsoleLedgerMutationReportsUnconfirmedOutcome(t *testing.T) {
	s, _ := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, mutation := range []struct {
		handle func(http.ResponseWriter, *http.Request)
		body   string
	}{{s.handleUsageLedgerClear, `{"confirmed":true}`}, {s.handleUsageLedgerRetention, `{"maxRecords":1,"days":1}`}, {s.handleUsageLedgerRetry, `{}`}} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(mutation.body)).WithContext(ctx)
		w := httptest.NewRecorder()
		mutation.handle(w, r)
		if w.Code != 500 || !strings.Contains(w.Body.String(), "尚未确认") || strings.Contains(w.Body.String(), "原记录仍保留") || strings.Contains(w.Body.String(), "原设置仍保留") {
			t.Fatalf("cancelled command asserted an unverified durable outcome: %s", w.Body)
		}
	}
}
