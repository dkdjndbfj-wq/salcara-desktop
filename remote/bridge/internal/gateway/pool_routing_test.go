package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/config"
)

func TestPoolMembersDoNotOverrideManualToolSelection(t *testing.T) {
	manual, member := poolFixture("manual", 1), poolFixture("member", 100)
	manual.PoolEnabled = false
	h := New(func() string { return localTestKey }, func(string) (config.LocalAccount, bool) { return manual, true })
	h.Pool, h.Accounts = NewAccountPool(), func(string) []config.LocalAccount { return []config.LocalAccount{manual, member} }
	h.PoolEnabled = func(string) bool { return false }
	got, release, ok := h.resolveAccount("codex", httptest.NewRequest(http.MethodPost, "/gateway/codex/v1/responses", nil), []byte(`{"model":"gpt-5"}`))
	if !ok || got.ID != manual.ID {
		t.Fatalf("manual route changed to pool: %+v", got)
	}
	release()
	if len(h.poolCatalog("codex")) != 0 {
		t.Fatal("manual route advertised unrelated pool catalog")
	}
	manual.Key = "replacement"
	if h.accountStillCurrentFingerprint("codex", got.ID, config.AccountFingerprint(got)) {
		t.Fatal("removed manual credential was still current")
	}
}

func TestRemoteRouteNeverSubstitutesPoolMember(t *testing.T) {
	a, b := poolFixture("selected", 1), poolFixture("other", 100)
	a.Priority, b.Priority = 0, 99
	a.Kind, b.Kind = "codex", "codex"
	h := New(func() string { return localTestKey }, func(string) (config.LocalAccount, bool) { return a, true })
	h.Pool, h.Accounts = NewAccountPool(), func(string) []config.LocalAccount { return []config.LocalAccount{a, b} }
	h.PoolEnabled = func(string) bool { return true }
	got, release, ok := h.resolveAccount("remote-codex", httptest.NewRequest(http.MethodPost, "/gateway/remote-codex/v1/responses", nil), []byte(`{"model":"gpt-5"}`))
	if !ok || got.ID != a.ID {
		t.Fatal("remote proof for selected card substituted pool account")
	}
	release()
	if len(h.poolCatalog("remote-codex")) != 0 {
		t.Fatal("remote route exposed unauthorized pooled catalog")
	}
	got, release, ok = h.resolveAccount("codex", httptest.NewRequest(http.MethodPost, "/gateway/codex/v1/responses", nil), []byte(`{"model":"gpt-5"}`))
	if !ok || got.ID != b.ID {
		t.Fatal("local pool scheduling regressed")
	}
	release()
}

func TestPoolCatalogIsIndependentOfCapacityCooldownAndFirstMemberRules(t *testing.T) {
	a, b := poolFixture("a", 1), poolFixture("b", 1)
	a.Kind, b.Kind = "codex", "codex"
	a.MaxConcurrency, b.MaxConcurrency = 1, 1
	a.Models, a.ModelAllowlist, a.Model = []string{"gpt-5"}, []string{"gpt-5"}, ""
	b.Models, b.Model = []string{"gpt-5-mini"}, ""
	p := NewAccountPool()
	_, releaseA, err := p.Select("codex", []config.LocalAccount{a}, "coding", "gpt-5", "")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseA()
	_, releaseB, err := p.Select("codex", []config.LocalAccount{b}, "coding", "gpt-5-mini", "")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseB()
	p.MarkFailure(b.ID, time.Minute)
	h := New(func() string { return localTestKey }, func(string) (config.LocalAccount, bool) { return a, true })
	h.Pool, h.Accounts = p, func(string) []config.LocalAccount { return []config.LocalAccount{a, b} }
	h.PoolEnabled, h.PoolGroup = func(string) bool { return true }, func(string) string { return "coding" }
	r := httptest.NewRequest(http.MethodGet, "/gateway/codex/v1/models", nil)
	r.RemoteAddr = "127.0.0.1:10000"
	r.Header.Set("Authorization", "Bearer "+localTestKey)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"gpt-5-mini"`) || !strings.Contains(w.Body.String(), `"id":"gpt-5"`) {
		t.Fatalf("catalog unavailable or filtered by first member: %d %s", w.Code, w.Body)
	}
	for _, state := range p.Snapshot([]config.LocalAccount{a, b}) {
		if state.Inflight != 1 {
			t.Fatalf("catalog occupied task slot: %+v", state)
		}
	}
}

func TestPoolRevalidationRejectsDisabledMembershipOrChangedGroup(t *testing.T) {
	a := poolFixture("a", 1)
	h := New(func() string { return localTestKey }, func(string) (config.LocalAccount, bool) { return a, true })
	h.Pool, h.Accounts = NewAccountPool(), func(string) []config.LocalAccount { return []config.LocalAccount{a} }
	group := "coding"
	h.PoolEnabled, h.PoolGroup = func(string) bool { return true }, func(string) string { return group }
	fingerprint := config.AccountFingerprint(a)
	group = "other"
	if h.accountStillCurrentFingerprint("codex", a.ID, fingerprint) {
		t.Fatal("old pool group remained authorized")
	}
	group, a.PoolEnabled = "coding", false
	if h.accountStillCurrentFingerprint("codex", a.ID, fingerprint) {
		t.Fatal("disabled member remained authorized")
	}
}

func TestPoolAffinityIsIsolatedBetweenAgentRoutes(t *testing.T) {
	p := NewAccountPool()
	a, b := poolFixture("a", 1), poolFixture("b", 1)
	a.Kind, b.Kind = "codex", "claude"
	first, release, err := p.Select("codex", []config.LocalAccount{a, b}, "coding", "gpt-5", "same-thread")
	if err != nil {
		t.Fatal(err)
	}
	release()
	second, release, err := p.Select("claude", []config.LocalAccount{a, b}, "coding", "gpt-5", "same-thread")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if first.ID != "a" || second.ID != "b" {
		t.Fatalf("agent routes shared or ignored affinity namespace: %s -> %s", first.ID, second.ID)
	}
}
