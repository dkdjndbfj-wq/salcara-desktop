package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"salcara/bridge/internal/config"
)

func TestFixedGatewaySharesPoolCredentialBudgetAndDoesNotReserveCatalogSlot(t *testing.T) {
	a := config.LocalAccount{ID: "fixed-card", Kind: "codex", SourceType: "official_account", CredentialRef: "synthetic-credential", CredentialEpoch: 1, BaseURL: "https://chatgpt.com/backend-api/codex", Model: "chosen-model", Models: []string{"chosen-model"}, MaxConcurrency: 4}
	pooled := a
	pooled.ID, pooled.PoolGroup, pooled.PoolEnabled = "pooled-card", "group-A", true
	lower := a
	lower.ID, lower.PoolGroup, lower.MaxConcurrency = "other-group", "group-B", 1
	cards := []config.LocalAccount{a, pooled, lower}
	p := NewAccountPool()
	h := New(func() string { return localTestKey }, func(string) (config.LocalAccount, bool) { return a, true })
	h.Pool, h.Accounts, h.PoolEnabled = p, func(string) []config.LocalAccount { return cards }, func(string) bool { return false }
	var calls atomic.Int32
	h.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"fixed-response","object":"response","status":"completed","output":[]}`))}, nil
	}
	_, release, err := p.Select("codex", cards, "group-A", "chosen-model", "")
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/gateway/codex/"+path, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("Authorization", "Bearer "+localTestKey)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := request(http.MethodPost, "v1/responses", `{"model":"chosen-model","input":"synthetic"}`); w.Code != 409 || calls.Load() != 0 {
		t.Fatal("fixed route bypassed occupied pool", w.Code, calls.Load())
	}
	if w := request(http.MethodGet, "v1/models", ""); w.Code != 200 {
		t.Fatal("catalog consumed shared slot", w.Code)
	}
	release()
	if w := request(http.MethodPost, "v1/responses", `{"model":"chosen-model","input":"synthetic"}`); w.Code != 200 || calls.Load() != 1 {
		t.Fatal("fixed route failed after release", w.Code, calls.Load())
	}
	_, release, err = p.Select("codex", cards, "group-A", "chosen-model", "")
	if err != nil {
		t.Fatal("fixed route leaked shared slot", err)
	}
	release()
}
