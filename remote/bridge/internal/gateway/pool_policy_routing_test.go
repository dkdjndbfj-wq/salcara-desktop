package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"salcara/bridge/internal/config"
)

func TestResolveAccountUsesAuthorizedAppliedSourceAsSinglePolicyAnchor(t *testing.T) {
	anchor, other := poolFixture("anchor", 0), poolFixture("other", 100)
	anchor.PoolStrategy = "single_account"
	h := New(func() string { return localTestKey }, func(string) (config.LocalAccount, bool) { return anchor, true })
	h.Pool = NewAccountPool()
	h.Accounts = func(string) []config.LocalAccount { return []config.LocalAccount{other, anchor} }
	h.PoolEnabled = func(string) bool { return true }
	h.PoolGroup = func(string) string { return "coding" }
	got, release, ok := h.resolveAccount("codex", httptest.NewRequest(http.MethodPost, "/gateway/codex/v1/responses", nil), []byte(`{"model":"gpt-5"}`))
	if !ok || got.ID != anchor.ID {
		t.Fatalf("candidate order/high priority replaced explicit single-account anchor: %+v", got)
	}
	release()
	anchor.Models = []string{"gpt-5"}
	other.Models = []string{"grok-4"}
	models := h.poolCatalog("codex")
	if len(models) != 1 || models[0] != "gpt-5" {
		t.Fatalf("single-account catalog exposed another member: %#v", models)
	}
}

func TestResolveAccountStandalonePoolCompatibilityAndProductionAnchorRequirement(t *testing.T) {
	a, b := poolFixture("a", 1), poolFixture("b", 10)
	request := httptest.NewRequest(http.MethodPost, "/gateway/codex/v1/responses", nil)
	body := []byte(`{"model":"gpt-5"}`)

	standalone := New(func() string { return localTestKey }, nil)
	standalone.Pool = NewAccountPool()
	standalone.Accounts = func(string) []config.LocalAccount { return []config.LocalAccount{a, b} }
	got, release, ok := standalone.resolveAccount("codex", request, body)
	if !ok || got.ID != b.ID {
		t.Fatalf("legacy standalone pool failed or panicked: %+v", got)
	}
	release()

	production := New(func() string { return localTestKey }, nil)
	production.Pool = NewAccountPool()
	production.Accounts = standalone.Accounts
	production.PoolEnabled = func(string) bool { return true }
	if _, _, ok := production.resolveAccount("codex", request, body); ok {
		t.Fatal("production-style pool ran without an authorized source anchor")
	}
}
