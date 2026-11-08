package gateway

import (
	"testing"
	"time"

	"salcara/bridge/internal/config"
)

func poolFixture(id string, priority int) config.LocalAccount {
	return config.LocalAccount{
		ID: id, Name: id, Kind: "api", SourceType: "reverse_proxy", BaseURL: "https://provider.test", Key: "key-" + id,
		Model: "gpt-5", Models: []string{"gpt-5", "grok-4"}, PoolGroup: "coding", PoolEnabled: true,
		Priority: priority, Weight: 1, MaxConcurrency: 2,
	}
}

func TestAccountPoolIgnoresManualAccountsAndPinsSession(t *testing.T) {
	p := NewAccountPool()
	manual := poolFixture("manual", 100)
	manual.PoolEnabled = false
	a := poolFixture("a", 1)
	b := poolFixture("b", 1)
	selected, release, err := p.Select("codex", []config.LocalAccount{manual, a, b}, "", "gpt-5", "thread-1")
	if err != nil || selected.ID == "manual" {
		t.Fatalf("selected manual account or failed: %+v %v", selected, err)
	}
	selectedAgain, releaseAgain, err := p.Select("codex", []config.LocalAccount{manual, a, b}, "", "gpt-5", "thread-1")
	if err != nil || selectedAgain.ID != selected.ID {
		t.Fatalf("session was not sticky: %s -> %s (%v)", selected.ID, selectedAgain.ID, err)
	}
	releaseAgain()
	release()
}

func TestAccountPoolPriorityAndModelEligibility(t *testing.T) {
	p := NewAccountPool()
	high := poolFixture("high", 10)
	low := poolFixture("low", 1)
	low.Models = []string{"grok-4"}
	selected, release, err := p.Select("codex", []config.LocalAccount{high, low}, "", "gpt-5", "")
	if err != nil || selected.ID != high.ID {
		t.Fatalf("priority/model selection failed: %+v %v", selected, err)
	}
	release()
	if _, _, err = p.Select("codex", []config.LocalAccount{high, low}, "", "unknown-model", ""); err == nil {
		t.Fatal("unknown model was allowed despite every account exposing a catalog")
	}
}

func TestAccountPoolCapacityAndCooldownFailClosed(t *testing.T) {
	p := NewAccountPool()
	p.now = func() time.Time { return time.Unix(100, 0) }
	a := poolFixture("a", 1)
	a.MaxConcurrency = 1
	_, release, err := p.Select("claude", []config.LocalAccount{a}, "", "gpt-5", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Select("claude", []config.LocalAccount{a}, "", "gpt-5", ""); err != errPoolCapacity {
		t.Fatalf("expected capacity error, got %v", err)
	}
	release()
	p.MarkFailure(a.ID, 0)
	if _, _, err := p.Select("claude", []config.LocalAccount{a}, "", "gpt-5", ""); err == nil {
		t.Fatal("cooling account was selected")
	}
	p.now = func() time.Time { return time.Unix(100, 0).Add(3 * time.Second) }
	_, release, err = p.Select("claude", []config.LocalAccount{a}, "", "gpt-5", "")
	if err != nil {
		t.Fatalf("account did not recover after cooldown: %v", err)
	}
	release()
}

func TestAccountPoolFallsBackToLowerPriorityWhenPreferredMemberUnavailable(t *testing.T) {
	p := NewAccountPool()
	high := poolFixture("high", 10)
	high.MaxConcurrency = 1
	low := poolFixture("low", 1)
	_, release, err := p.Select("codex", []config.LocalAccount{high}, "", "gpt-5", "")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	selected, releaseLow, err := p.Select("codex", []config.LocalAccount{high, low}, "", "gpt-5", "")
	if err != nil || selected.ID != low.ID {
		t.Fatalf("healthy lower-priority fallback failed: %+v %v", selected, err)
	}
	releaseLow()
}

func TestAccountPoolDoesNotMixUnselectedGroups(t *testing.T) {
	p := NewAccountPool()
	a := poolFixture("a", 1)
	b := poolFixture("b", 1)
	b.PoolGroup = "other-provider"
	if _, _, err := p.Select("codex", []config.LocalAccount{a, b}, "", "gpt-5", ""); err != errPoolGroupRequired {
		t.Fatalf("expected explicit group requirement, got %v", err)
	}
	selected, release, err := p.Select("codex", []config.LocalAccount{a, b}, "other-provider", "gpt-5", "")
	if err != nil || selected.ID != b.ID {
		t.Fatalf("explicit group was not honored: %+v %v", selected, err)
	}
	release()
}

func TestAccountPoolCurrentDetectsCredentialReplacement(t *testing.T) {
	p := NewAccountPool()
	a := poolFixture("a", 1)
	if !p.Current("codex", []config.LocalAccount{a}, a) {
		t.Fatal("current account was rejected")
	}
	replaced := a
	replaced.Key = "new-key"
	if p.Current("codex", []config.LocalAccount{replaced}, a) {
		t.Fatal("old credential remained current after replacement")
	}
}

func TestAccountPoolCatalogUnionsAndHonorsModelRules(t *testing.T) {
	p := NewAccountPool()
	a := poolFixture("a", 1)
	a.Models = []string{"gpt-5", "grok-4"}
	a.ModelDenylist = []string{"grok-4"}
	b := poolFixture("b", 1)
	b.Models = []string{"gpt-5-mini", "deepseek-chat"}
	b.ModelAllowlist = []string{"gpt-5-mini"}
	manual := poolFixture("manual", 100)
	manual.PoolEnabled = false
	got := p.Catalog("codex", []config.LocalAccount{a, b, manual}, "coding")
	want := []string{"gpt-5", "gpt-5-mini"}
	if len(got) != len(want) {
		t.Fatalf("catalog length %d, got %#v", len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("catalog[%d] = %q, want %q (%#v)", i, got[i], want[i], got)
		}
	}
}

func TestAccountPoolMatchesClaudeDesktopOpaqueModelRoute(t *testing.T) {
	p := NewAccountPool()
	a := poolFixture("claude-route", 1)
	a.Models = []string{"gpt-5", "deepseek-chat"}
	route := config.ClaudeDesktopModelRoute("deepseek-chat")
	selected, release, err := p.Select("claude-desktop", []config.LocalAccount{a}, "coding", route, "")
	if err != nil || selected.ID != a.ID {
		t.Fatalf("opaque Claude Desktop route was not matched: %+v %v", selected, err)
	}
	release()
}

func TestAccountPoolModelRulesFailClosed(t *testing.T) {
	p := NewAccountPool()
	a := poolFixture("restricted", 1)
	a.ModelAllowlist = []string{"gpt-5"}
	a.ModelDenylist = []string{"gpt-5"}
	if _, _, err := p.Select("codex", []config.LocalAccount{a}, "", "gpt-5", ""); err == nil {
		t.Fatal("deny rule did not override allow rule")
	}
}

func TestAccountPoolLateOldCredentialHealthUpdateIsIgnored(t *testing.T) {
	p := NewAccountPool()
	a := poolFixture("rotating", 1)
	oldFingerprint := config.AccountFingerprint(a)
	_, release, err := p.Select("codex", []config.LocalAccount{a}, "", "gpt-5", "")
	if err != nil {
		t.Fatal(err)
	}
	release()
	p.MarkFailureFor(a.ID, oldFingerprint, 30*time.Second)
	replaced := a
	replaced.Key = "new-key"
	if _, release, err = p.Select("codex", []config.LocalAccount{replaced}, "", "gpt-5", ""); err != nil {
		t.Fatal(err)
	} else {
		release()
	}
	p.MarkFailureFor(a.ID, oldFingerprint, 30*time.Second)
	state := p.Snapshot([]config.LocalAccount{replaced})
	if len(state) != 1 || state[0].CoolingDown || state[0].Failures != 0 {
		t.Fatalf("late old credential update poisoned replacement: %+v", state)
	}
}

func TestAccountPoolQuotaReserveFailsClosedAndRevokesSticky(t *testing.T) {
	p := NewAccountPool()
	a := poolFixture("official", 1)
	a.SourceType = "official_account"
	a.ProviderID = "openai"
	a.CredentialRef = "vault-account"
	a.CredentialEpoch = 7
	a.QuotaReservePercent = 10
	remaining := 25.0
	modelSeen := ""
	p.SetQuotaProbe(func(ref string, identityEpoch uint64, model string) PoolQuotaObservation {
		if ref != a.CredentialRef || identityEpoch != a.CredentialEpoch {
			t.Fatalf("quota probe identity mismatch: %q %d", ref, identityEpoch)
		}
		modelSeen = model
		return PoolQuotaObservation{Fresh: true, Allowed: true, RemainingPercent: remaining}
	})
	selected, release, err := p.Select("codex", []config.LocalAccount{a}, "coding", "gpt-5", "quota-thread")
	if err != nil || selected.ID != a.ID || modelSeen != "gpt-5" {
		t.Fatalf("fresh quota was not selected: %+v %q %v", selected, modelSeen, err)
	}
	release()
	remaining = 10
	if _, _, err = p.Select("codex", []config.LocalAccount{a}, "coding", "gpt-5", "quota-thread"); err != errPoolQuotaReserve {
		t.Fatalf("reserve threshold did not revoke sticky account: %v", err)
	}
	p.SetQuotaProbe(func(string, uint64, string) PoolQuotaObservation { return PoolQuotaObservation{} })
	if _, _, err = p.Select("codex", []config.LocalAccount{a}, "coding", "gpt-5", ""); err != errPoolQuotaReserve {
		t.Fatalf("unknown quota did not fail closed: %v", err)
	}
}

func TestAccountPoolQuotaUsesResolvedClaudeDesktopModel(t *testing.T) {
	p := NewAccountPool()
	a := poolFixture("official-claude-route", 1)
	a.SourceType = "official_account"
	a.CredentialRef = "vault-account"
	a.CredentialEpoch = 3
	a.QuotaReservePercent = 10
	a.Models = []string{"deepseek-chat"}
	route := config.ClaudeDesktopModelRoute("deepseek-chat")
	p.SetQuotaProbe(func(_ string, _ uint64, model string) PoolQuotaObservation {
		if model != "deepseek-chat" {
			t.Fatalf("quota probe saw opaque route instead of provider model: %q", model)
		}
		return PoolQuotaObservation{Fresh: true, Allowed: true, RemainingPercent: 50}
	})
	selected, release, err := p.Select("claude-desktop", []config.LocalAccount{a}, "coding", route, "")
	if err != nil || selected.ID != a.ID {
		t.Fatalf("resolved route was not quota eligible: %+v %v", selected, err)
	}
	release()
}

func TestAccountPoolZeroQuotaReserveKeepsLegacyBehavior(t *testing.T) {
	p := NewAccountPool()
	a := poolFixture("legacy", 1)
	called := false
	p.SetQuotaProbe(func(string, uint64, string) PoolQuotaObservation {
		called = true
		return PoolQuotaObservation{}
	})
	_, release, err := p.Select("codex", []config.LocalAccount{a}, "coding", "gpt-5", "")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if called {
		t.Fatal("zero reserve unexpectedly required a usage observation")
	}
}
