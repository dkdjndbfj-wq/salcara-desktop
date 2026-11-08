package gateway

import (
	"testing"
	"time"

	"salcara/bridge/internal/config"
)

func officialPoolFixture(id string) config.LocalAccount {
	a := poolFixture(id, 0)
	a.SourceType = "official_account"
	a.ProviderID = "openai"
	a.CredentialRef = "vault-" + id
	a.CredentialEpoch = 1
	return a
}

func TestAccountPoolCockpitMetricStrategiesKnownBeforeUnknown(t *testing.T) {
	p := NewAccountPool()
	a, b, unknown := officialPoolFixture("a"), officialPoolFixture("b"), officialPoolFixture("unknown")
	expiryA, expiryB := time.Unix(300, 0), time.Unix(200, 0)
	p.SetRoutingProbe(func(ref string, _ uint64, _ string) PoolRoutingObservation {
		switch ref {
		case a.CredentialRef:
			return PoolRoutingObservation{Current: true, QuotaFresh: true, QuotaAllowed: true, RemainingPercent: 20, PlanFresh: true, PlanFamily: "enterprise", SubscriptionKnown: true, SubscriptionActiveUntil: &expiryA}
		case b.CredentialRef:
			return PoolRoutingObservation{Current: true, QuotaFresh: true, QuotaAllowed: true, RemainingPercent: 80, PlanFresh: true, PlanFamily: "free", SubscriptionKnown: true, SubscriptionActiveUntil: &expiryB}
		default:
			return PoolRoutingObservation{Current: true}
		}
	})
	tests := map[string]string{
		"auto": "a", "plan_high_first": "a", "plan_low_first": "b",
		"quota_high_first": "b", "quota_low_first": "a", "expiry_soon_first": "b",
	}
	for strategy, want := range tests {
		selected, release, err := p.SelectWithPolicy("codex", []config.LocalAccount{unknown, a, b}, "coding", "gpt-5", "", PoolPolicy{Strategy: strategy, SingleAccountID: a.ID, Revision: strategy})
		if err != nil || selected.ID != want {
			t.Errorf("%s selected %q, want %q: %v", strategy, selected.ID, want, err)
		}
		if release != nil {
			release()
		}
	}
}

func TestAccountPoolSingleAccountAndPolicyRevisionRetireSticky(t *testing.T) {
	p := NewAccountPool()
	a, b := poolFixture("a", 1), poolFixture("b", 100)
	legacy, release, err := p.SelectWithPolicy("codex", []config.LocalAccount{a, b}, "coding", "gpt-5", "thread", PoolPolicy{Revision: "legacy"})
	if err != nil || legacy.ID != b.ID {
		t.Fatalf("legacy seed failed: %+v %v", legacy, err)
	}
	release()
	selected, release, err := p.SelectWithPolicy("codex", []config.LocalAccount{a, b}, "coding", "gpt-5", "thread", PoolPolicy{Strategy: "single_account", SingleAccountID: a.ID, Revision: "single-v1"})
	if err != nil || selected.ID != a.ID {
		t.Fatalf("old sticky bypassed single account: %+v %v", selected, err)
	}
	release()

	selected, release, err = p.SelectWithPolicy("codex", []config.LocalAccount{a, b}, "coding", "gpt-5", "thread-2", PoolPolicy{Strategy: "custom", Revision: "custom-v1"})
	if err != nil || selected.ID != b.ID {
		t.Fatalf("custom seed failed: %+v %v", selected, err)
	}
	release()
	b.PoolBackup = true
	selected, release, err = p.SelectWithPolicy("codex", []config.LocalAccount{a, b}, "coding", "gpt-5", "thread-2", PoolPolicy{Strategy: "custom", Revision: "custom-v1"})
	if err != nil || selected.ID != a.ID {
		t.Fatalf("changed policy reused old sticky/backup: %+v %v", selected, err)
	}
	release()
}

func TestAccountPoolMemberPolicyChangeRetiresSticky(t *testing.T) {
	p := NewAccountPool()
	a, b := poolFixture("a", 10), poolFixture("b", 1)
	selected, release, err := p.SelectWithPolicy("codex", []config.LocalAccount{a, b}, "coding", "gpt-5", "thread", PoolPolicy{Strategy: "custom", Revision: "custom"})
	if err != nil || selected.ID != a.ID {
		t.Fatalf("sticky seed failed: %+v %v", selected, err)
	}
	release()
	a.Priority = 0
	b.Priority = 20
	selected, release, err = p.SelectWithPolicy("codex", []config.LocalAccount{a, b}, "coding", "gpt-5", "thread", PoolPolicy{Strategy: "custom", Revision: "custom"})
	if err != nil || selected.ID != b.ID {
		t.Fatalf("changed member policy reused stale sticky: %+v %v", selected, err)
	}
	release()
}

func TestAccountPoolCustomBackupUsedOnlyWhenPrimaryUnavailable(t *testing.T) {
	p := NewAccountPool()
	primary, backup := poolFixture("primary", 0), poolFixture("backup", 100)
	primary.MaxConcurrency = 1
	backup.PoolBackup = true
	selected, release, err := p.SelectWithPolicy("codex", []config.LocalAccount{backup, primary}, "coding", "gpt-5", "", PoolPolicy{Strategy: "custom", Revision: "custom"})
	if err != nil || selected.ID != primary.ID {
		t.Fatalf("backup displaced eligible primary: %+v %v", selected, err)
	}
	defer release()
	selected, releaseBackup, err := p.SelectWithPolicy("codex", []config.LocalAccount{backup, primary}, "coding", "gpt-5", "", PoolPolicy{Strategy: "custom", Revision: "custom"})
	if err != nil || selected.ID != backup.ID {
		t.Fatalf("backup was not used after primary capacity exhausted: %+v %v", selected, err)
	}
	releaseBackup()
}

func TestAccountPoolCustomKeepsHealthyBackupStickyForExistingSession(t *testing.T) {
	p := NewAccountPool()
	primary, backup := poolFixture("primary", 0), poolFixture("backup", 0)
	primary.MaxConcurrency = 1
	backup.PoolBackup = true
	_, holdPrimary, err := p.SelectWithPolicy("codex", []config.LocalAccount{primary, backup}, "coding", "gpt-5", "", PoolPolicy{Strategy: "custom", Revision: "custom"})
	if err != nil {
		t.Fatal(err)
	}
	selected, releaseBackup, err := p.SelectWithPolicy("codex", []config.LocalAccount{primary, backup}, "coding", "gpt-5", "thread", PoolPolicy{Strategy: "custom", Revision: "custom"})
	if err != nil || selected.ID != backup.ID {
		t.Fatalf("backup session was not established: %+v %v", selected, err)
	}
	releaseBackup()
	holdPrimary()
	selected, releaseBackup, err = p.SelectWithPolicy("codex", []config.LocalAccount{primary, backup}, "coding", "gpt-5", "thread", PoolPolicy{Strategy: "custom", Revision: "custom"})
	if err != nil || selected.ID != backup.ID {
		t.Fatalf("healthy backup affinity was discarded after primary recovered: %+v %v", selected, err)
	}
	releaseBackup()
}

func TestAccountPoolRandomUsesInjectedChooserAfterSafetyFiltering(t *testing.T) {
	p := NewAccountPool()
	p.randomIndex = func(n int) int {
		if n != 2 {
			t.Fatalf("random chooser saw %d candidates, want 2", n)
		}
		return 1
	}
	a, b, denied := poolFixture("a", 0), poolFixture("b", 0), poolFixture("denied", 0)
	denied.ModelDenylist = []string{"gpt-5"}
	selected, release, err := p.SelectWithPolicy("codex", []config.LocalAccount{denied, a, b}, "coding", "gpt-5", "", PoolPolicy{Strategy: "random", Revision: "random"})
	if err != nil || selected.ID != b.ID {
		t.Fatalf("random selection bypassed filtering/chooser: %+v %v", selected, err)
	}
	release()
}
