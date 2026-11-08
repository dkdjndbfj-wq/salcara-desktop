package console

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/usageledger"
)

func ledgerQueryHandler(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.usageLedgerQueryRoutes(mux)
	return s, s.requireAuth(mux)
}
func TestUsageLedgerQueryPricingRoutesRequireConsoleAndRejectAmbiguity(t *testing.T) {
	s, h := ledgerQueryHandler(t)
	for _, route := range []struct{ method, path string }{{"GET", "/api/local/usage-ledger/query"}, {"GET", "/api/local/usage-ledger/pricing"}, {"POST", "/api/local/usage-ledger/pricing"}, {"POST", "/api/local/usage-ledger/reprice"}} {
		if w := do(h, route.method, route.path, "127.0.0.1:47831", nil, `{}`); w.Code != 401 {
			t.Fatal("ledger query/pricing bypassed console cookie", route, w.Code)
		}
	}
	for _, query := range []string{"?limit=201", "?limit=0", "?offset=1", "?limit=1&limit=2", "?model=sk-secret", "?sort=time%3BDELETE", "?unknown=x", "?from=-1"} {
		if w := localRequest(s, h, "GET", "/api/local/usage-ledger/query"+query, nil); w.Code != 400 {
			t.Fatal("ambiguous filter accepted", query, w.Code, w.Body)
		}
	}
	for _, body := range []string{`{}`, `{"expectedVersion":1}`, `{"expectedVersion":1,"entries":[],"Entries":[]}`, `{"expectedVersion":1,"entries":[],"key":"synthetic-secret"}`, `{"expectedVersion":1,"entries":[]} {}`} {
		w := do(h, "POST", "/api/local/usage-ledger/pricing", "127.0.0.1:47831", map[string]string{"Cookie": CookieName + "=" + s.Token(), "Content-Type": "application/json"}, body)
		if w.Code != 400 {
			t.Fatal("ambiguous pricebook accepted", body, w.Code)
		}
	}
	if w := do(h, "POST", "/api/local/usage-ledger/pricing", "127.0.0.1:47831", map[string]string{"Cookie": CookieName + "=" + s.Token(), "Content-Type": "application/json", "Origin": "https://evil.example"}, `{"expectedVersion":1,"entries":[]}`); w.Code != 403 {
		t.Fatal("cross-origin pricing allowed")
	}
}

func TestUsageLedgerQueryExposesDetachedUnknownCountersAndExplicitRepricing(t *testing.T) {
	s, h := ledgerQueryHandler(t)
	input, output, cached := int64(5), int64(2), int64(0)
	s.observeUsage(usageledger.Observation{StartedAt: time.Now(), AccountID: "synthetic-private-account", Route: "codex-api", Model: "gpt-ledger-query", State: "completed", HTTPStatus: 200, UsageFinal: true, Tokens: usageledger.Tokens{Input: &input, Output: &output, CachedInput: &cached}})
	flushConsoleLedger(t, s)
	first := localRequest(s, h, "GET", "/api/local/usage-ledger/query", nil)
	if first.Code != 200 {
		t.Fatal(first.Code, first.Body)
	}
	var initial usageledger.QueryResult
	if json.Unmarshal(first.Body.Bytes(), &initial) != nil || initial.Total != 1 || initial.Summary.TotalTokens.Sum != nil || initial.Records[0].ProviderCost != nil || initial.Records[0].EstimatedCost != nil {
		t.Fatal("unknown usage/money became known", first.Body)
	}
	body := `{"expectedVersion":1,"entries":[{"model":"gpt-ledger-query","tier":"","inputTokenBasis":"includes_cache","cacheCreationMode":"included_input","rates":{"inputUsdPerMillion":1,"cachedInputUsdPerMillion":0.1,"cacheCreationUsdPerMillion":null,"outputUsdPerMillion":2},"longContextThreshold":null,"longRates":null}]}`
	headers := map[string]string{"Cookie": CookieName + "=" + s.Token(), "Content-Type": "application/json"}
	if w := do(h, "POST", "/api/local/usage-ledger/pricing", "127.0.0.1:47831", headers, body); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if w := do(h, "POST", "/api/local/usage-ledger/pricing", "127.0.0.1:47831", headers, body); w.Code != 409 {
		t.Fatal("stale pricing editor changed version", w.Code)
	}
	if w := do(h, "POST", "/api/local/usage-ledger/reprice", "127.0.0.1:47831", headers, `{"expectedVersion":2,"confirmed":false}`); w.Code != 400 {
		t.Fatal("unconfirmed historical repricing accepted")
	}
	if w := do(h, "POST", "/api/local/usage-ledger/reprice", "127.0.0.1:47831", headers, `{"expectedVersion":2,"confirmed":true}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	final := localRequest(s, h, "GET", "/api/local/usage-ledger/query", nil)
	var result usageledger.QueryResult
	if json.Unmarshal(final.Body.Bytes(), &result) != nil || result.Records[0].EstimatedCost == nil || result.Records[0].EstimatedCost.Source != "user_pricebook" || result.Records[0].EstimatedCost.Currency != "USD" || result.Records[0].ProviderCost != nil {
		t.Fatal("estimate falsely became provider money", final.Body)
	}
	if strings.Contains(final.Body.String(), "synthetic-private-account") {
		t.Fatal("raw account ID leaked in query DTO")
	}
	if w := localRequest(s, h, "GET", "/api/local/usage-ledger/query?revision="+initial.Revision, nil); w.Code != 409 {
		t.Fatal("old query revision was reused", w.Code)
	}
}

func TestUsageLedgerAccountSourceOnlyExposesCurrentSafeHostAndKind(t *testing.T) {
	for _, tt := range []struct {
		name    string
		account config.LocalAccount
		want    ledgerAccountSource
	}{
		{"api", config.LocalAccount{BaseURL: "https://user:synthetic-secret@relay.example:8443/private/path?key=synthetic-query#synthetic-fragment"}, ledgerAccountSource{Kind: "api_key", Host: "relay.example:8443"}},
		{"official", config.LocalAccount{BaseURL: "https://private.example", SourceType: "official_account"}, ledgerAccountSource{Kind: "official_account"}},
		{"vault", config.LocalAccount{BaseURL: "https://private.example", CredentialRef: "synthetic-private-ref"}, ledgerAccountSource{Kind: "official_account"}},
		{"invalid", config.LocalAccount{BaseURL: "file:///private/path"}, ledgerAccountSource{Kind: "api_key"}},
		{"relative", config.LocalAccount{BaseURL: "/private/path"}, ledgerAccountSource{Kind: "api_key"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := ledgerAccountSourceFor(tt.account); got != tt.want {
				t.Fatalf("source = %#v, want %#v", got, tt.want)
			}
		})
	}
	s, h := ledgerQueryHandler(t)
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.LocalAccounts = []config.LocalAccount{{ID: "synthetic-first-private", Name: "same name", Kind: "api", BaseURL: "https://first.example/v1", Key: "synthetic-first-key"}, {ID: "synthetic-second-private", Name: "same name", Kind: "api", BaseURL: "https://second.example/v1", Key: "synthetic-second-key"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"synthetic-first-private", "synthetic-second-private", "synthetic-deleted-private"} {
		s.observeUsage(usageledger.Observation{StartedAt: time.Now(), AccountID: id, State: "completed", HTTPStatus: 200})
	}
	flushConsoleLedger(t, s)
	w := localRequest(s, h, "GET", "/api/local/usage-ledger/query", nil)
	var out struct {
		usageledger.QueryResult
		AccountNames   map[string]string              `json:"accountNames"`
		AccountSources map[string]ledgerAccountSource `json:"accountSources"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatal(w.Code, w.Body)
	}
	if len(out.ByAccount) != 3 || len(out.AccountNames) != 2 || len(out.AccountSources) != 2 {
		t.Fatal("same names merged or deleted account lost", w.Body)
	}
	if out.AccountSources[s.usageLedger.AnonymousAccountID("synthetic-first-private")].Host != "first.example" || out.AccountSources[s.usageLedger.AnonymousAccountID("synthetic-second-private")].Host != "second.example" {
		t.Fatal("source labels crossed", w.Body)
	}
	for _, secret := range []string{"synthetic-first-private", "synthetic-second-private", "synthetic-deleted-private", "synthetic-first-key", "synthetic-second-key", "/v1"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("sensitive source leaked", secret)
		}
	}
	if err := s.d.Store.Update(func(c *config.Config) error { c.LocalAccounts = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	w = localRequest(s, h, "GET", "/api/local/usage-ledger/query", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"accountSources":{}`) || !strings.Contains(w.Body.String(), `"total":3`) {
		t.Fatal("deleted labels retained or history dropped", w.Body)
	}
}
