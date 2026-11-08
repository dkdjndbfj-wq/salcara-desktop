package gateway

import (
	"errors"
	"sync"
	"testing"
	"time"

	"salcara/bridge/internal/config"
)

func TestCredentialBudgetDuplicateCardsShareCapacity(t *testing.T) {
	for _, mode := range []string{"same-group", "different-group", "different-agent"} {
		t.Run(mode, func(t *testing.T) {
			pool := NewAccountPool()
			first, duplicate := poolFixture("first", 10), poolFixture("duplicate", 1)
			first.MaxConcurrency, duplicate.MaxConcurrency = 1, 50
			duplicate.Key = first.Key
			firstTarget, secondTarget, secondGroup := "codex", "codex", "coding"
			if mode == "different-group" {
				duplicate.PoolGroup, secondGroup = "other", "other"
			}
			if mode == "different-agent" {
				first.Kind, duplicate.Kind = "codex", "claude"
				secondTarget = "claude"
			}
			accounts := []config.LocalAccount{first, duplicate}
			_, release, err := pool.Select(firstTarget, accounts, "coding", "gpt-5", "")
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			_, extraRelease, err := pool.Select(secondTarget, accounts, secondGroup, "gpt-5", "")
			if extraRelease != nil {
				extraRelease()
			}
			if !errors.Is(err, errPoolCapacity) {
				t.Fatalf("duplicate credential bypassed capacity: %v", err)
			}
			release()
			_, extraRelease, err = pool.Select(secondTarget, accounts, secondGroup, "gpt-5", "")
			if err != nil {
				t.Fatalf("credential remained occupied after release: %v", err)
			}
			extraRelease()
		})
	}
}

func TestCredentialBudgetCooldownSurvivesCardAndProtocolAliases(t *testing.T) {
	pool := NewAccountPool()
	now := time.Unix(100, 0)
	pool.now = func() time.Time { return now }
	first, duplicate := poolFixture("first", 10), poolFixture("duplicate", 1)
	duplicate.Key, duplicate.PoolGroup = first.Key, "other"
	accounts := []config.LocalAccount{first, duplicate}
	_, release, err := pool.Select("codex", accounts, "coding", "gpt-5", "")
	if err != nil {
		t.Fatal(err)
	}
	pool.MarkFailureFor(first.ID, config.AccountFingerprint(first), time.Minute)
	release()
	if _, release, err := pool.Select("claude", accounts, "other", "gpt-5", ""); err == nil {
		release()
		t.Fatal("duplicate card ignored provider cooldown")
	}
	changed := first
	changed.Kind, changed.Protocol, changed.Model = "claude", "anthropic", "grok-4"
	if _, release, err := pool.Select("claude", []config.LocalAccount{changed}, "coding", "grok-4", ""); err == nil {
		release()
		t.Fatal("same credential changed protocol to clear cooldown")
	}
	now = now.Add(time.Minute)
	if _, release, err := pool.Select("claude", []config.LocalAccount{changed}, "coding", "grok-4", ""); err != nil {
		t.Fatalf("credential did not recover when cooldown expired: %v", err)
	} else {
		release()
	}
}

func TestCredentialBudgetConcurrentSuccessDoesNotClearNewCooldown(t *testing.T) {
	pool := NewAccountPool()
	first, duplicate := poolFixture("first", 10), poolFixture("duplicate", 1)
	duplicate.Key = first.Key
	accounts := []config.LocalAccount{first, duplicate}
	_, firstRelease, err := pool.Select("codex", accounts, "coding", "gpt-5", "first-stream")
	if err != nil {
		t.Fatal(err)
	}
	defer firstRelease()
	_, secondRelease, err := pool.Select("codex", accounts, "coding", "gpt-5", "second-stream")
	if err != nil {
		t.Fatal(err)
	}
	defer secondRelease()
	pool.MarkFailureFor(first.ID, config.AccountFingerprint(first), time.Minute)
	pool.MarkSuccessFor(first.ID, config.AccountFingerprint(first))
	firstRelease()
	secondRelease()
	if _, release, err := pool.Select("codex", accounts, "coding", "gpt-5", ""); err == nil {
		release()
		t.Fatal("late successful stream cleared a newer cooldown")
	}
}

func TestCredentialBudgetProtectedIdentityUsesRefAndEpoch(t *testing.T) {
	pool := NewAccountPool()
	first, duplicate := poolFixture("first", 10), poolFixture("duplicate", 1)
	first.SourceType, duplicate.SourceType = "official_account", "official_account"
	first.ProviderID, duplicate.ProviderID = "openai", "openai"
	first.CredentialRef, duplicate.CredentialRef = "synthetic-vault", "synthetic-vault"
	first.CredentialEpoch, duplicate.CredentialEpoch = 3, 3
	first.MaxConcurrency, duplicate.MaxConcurrency = 1, 1
	accounts := []config.LocalAccount{first, duplicate}
	_, release, err := pool.Select("codex", accounts, "coding", "gpt-5", "")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, extraRelease, err := pool.Select("codex", accounts, "coding", "gpt-5", ""); err != errPoolCapacity {
		if extraRelease != nil {
			extraRelease()
		}
		t.Fatalf("protected marker/card alias bypassed ref budget: %v", err)
	}
	duplicate.CredentialEpoch++
	if _, extraRelease, err := pool.Select("codex", []config.LocalAccount{duplicate}, "coding", "gpt-5", ""); err != nil {
		t.Fatalf("replacement identity inherited old identity occupancy: %v", err)
	} else {
		extraRelease()
	}
}

func TestCredentialBudgetCanonicalEndpointAliasesShareCapacity(t *testing.T) {
	pool := NewAccountPool()
	first, duplicate := poolFixture("first", 10), poolFixture("duplicate", 1)
	first.BaseURL, duplicate.BaseURL = "https://PROVIDER.test:443/mount/v1/", "https://provider.test/mount"
	duplicate.Key = first.Key
	first.MaxConcurrency, duplicate.MaxConcurrency = 1, 1
	accounts := []config.LocalAccount{first, duplicate}
	_, release, err := pool.Select("codex", accounts, "coding", "gpt-5", "")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, extraRelease, err := pool.Select("codex", accounts, "coding", "gpt-5", ""); err != errPoolCapacity {
		if extraRelease != nil {
			extraRelease()
		}
		t.Fatalf("equivalent API endpoint bypassed shared budget: %v", err)
	}
}

func TestCredentialBudgetActiveLimitSurvivesDeletedCard(t *testing.T) {
	pool := NewAccountPool()
	first, duplicate := poolFixture("first", 10), poolFixture("duplicate", 1)
	first.MaxConcurrency, duplicate.MaxConcurrency = 1, 50
	duplicate.Key = first.Key
	_, release, err := pool.Select("codex", []config.LocalAccount{first, duplicate}, "coding", "gpt-5", "")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, extraRelease, err := pool.Select("codex", []config.LocalAccount{duplicate}, "coding", "gpt-5", ""); err != errPoolCapacity {
		if extraRelease != nil {
			extraRelease()
		}
		t.Fatalf("deleting occupied lower-limit alias raised active credential budget: %v", err)
	}
	release()
	if _, extraRelease, err := pool.Select("codex", []config.LocalAccount{duplicate}, "coding", "gpt-5", ""); err != nil {
		t.Fatalf("released deleted card blocked current policy: %v", err)
	} else {
		extraRelease()
	}
}

func TestCredentialBudgetEffectiveLimitSurvivesDeletedUnselectedAlias(t *testing.T) {
	pool := NewAccountPool()
	lowerLimit, selected := poolFixture("lower-limit", 1), poolFixture("selected", 10)
	lowerLimit.MaxConcurrency, selected.MaxConcurrency = 1, 50
	selected.Key = lowerLimit.Key
	got, release, err := pool.Select("codex", []config.LocalAccount{lowerLimit, selected}, "coding", "gpt-5", "")
	if err != nil || got.ID != selected.ID {
		t.Fatalf("expected higher-priority alias selection: %v", err)
	}
	defer release()
	if _, extraRelease, err := pool.Select("codex", []config.LocalAccount{selected}, "coding", "gpt-5", ""); err != errPoolCapacity {
		if extraRelease != nil {
			extraRelease()
		}
		t.Fatalf("deleting unselected lower-limit card raised occupied shared budget: %v", err)
	}
}

func TestCredentialBudgetEditReleaseAndLateFailureStayWithOriginalCredential(t *testing.T) {
	pool := NewAccountPool()
	first, oldAlias := poolFixture("edited", 10), poolFixture("old-alias", 1)
	first.MaxConcurrency, oldAlias.MaxConcurrency = 4, 1
	oldAlias.Key = first.Key
	oldFingerprint := config.AccountFingerprint(first)
	_, oldRelease, err := pool.Select("codex", []config.LocalAccount{first, oldAlias}, "coding", "gpt-5", "")
	if err != nil {
		t.Fatal(err)
	}
	defer oldRelease()
	replacement := first
	replacement.Key = "replacement-synthetic-key"
	newAlias := oldAlias
	newAlias.ID, newAlias.Key = "new-alias", replacement.Key
	accounts := []config.LocalAccount{replacement, oldAlias, newAlias}
	_, newRelease, err := pool.Select("codex", accounts, "coding", "gpt-5", "")
	if err != nil {
		t.Fatalf("independent replacement credential could not run: %v", err)
	}
	defer newRelease()
	oldRelease()
	oldRelease() // Releases are safe even after card replacement and repeated calls.
	if _, extraRelease, err := pool.Select("codex", []config.LocalAccount{replacement, newAlias}, "coding", "gpt-5", ""); err != errPoolCapacity {
		if extraRelease != nil {
			extraRelease()
		}
		t.Fatalf("old release freed replacement credential slot: %v", err)
	}
	pool.MarkFailureFor(first.ID, oldFingerprint, time.Minute)
	if _, extraRelease, err := pool.Select("codex", []config.LocalAccount{oldAlias}, "coding", "gpt-5", ""); err == nil {
		extraRelease()
		t.Fatal("late failure lost original credential cooldown after card edit")
	}
	newRelease()
	if _, extraRelease, err := pool.Select("codex", []config.LocalAccount{replacement, newAlias}, "coding", "gpt-5", ""); err != nil {
		t.Fatalf("old failure poisoned replacement credential: %v", err)
	} else {
		extraRelease()
	}
}

func TestCredentialBudgetDeletedCooldownCannotBeReimportedAway(t *testing.T) {
	pool := NewAccountPool()
	first := poolFixture("first", 10)
	_, release, err := pool.Select("codex", []config.LocalAccount{first}, "coding", "gpt-5", "")
	if err != nil {
		t.Fatal(err)
	}
	pool.MarkFailureFor(first.ID, config.AccountFingerprint(first), time.Minute)
	release()
	_, _, _ = pool.Select("codex", nil, "coding", "gpt-5", "")
	first.ID, first.Name, first.PoolGroup = "reimported", "new display", "new-group"
	if _, extraRelease, err := pool.Select("claude", []config.LocalAccount{first}, "new-group", "gpt-5", ""); err == nil {
		extraRelease()
		t.Fatal("delete/reimport with a new card cleared provider cooldown")
	}
}

func TestCredentialBudgetConcurrentAliasAdmissionIsAtomic(t *testing.T) {
	pool := NewAccountPool()
	const count = 20
	accounts := make([]config.LocalAccount, count)
	for i := range accounts {
		accounts[i] = poolFixture(string(rune('a'+i)), 1)
		accounts[i].Key, accounts[i].MaxConcurrency = "shared-synthetic-key", 1
	}
	start := make(chan struct{})
	var workers sync.WaitGroup
	var mu sync.Mutex
	releases := make([]func(), 0, count)
	for i := 0; i < count; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			_, release, err := pool.Select("codex", accounts, "coding", "gpt-5", "")
			if err == nil {
				mu.Lock()
				releases = append(releases, release)
				mu.Unlock()
			} else if !errors.Is(err, errPoolCapacity) {
				t.Errorf("unexpected admission error: %v", err)
			}
		}()
	}
	close(start)
	workers.Wait()
	for _, release := range releases {
		release()
	}
	if len(releases) != 1 {
		t.Fatalf("admitted %d simultaneous aliases, expected one credential slot", len(releases))
	}
}

func TestCredentialBudgetDistinctCredentialsRemainIndependent(t *testing.T) {
	for _, mode := range []string{"key", "host", "mount", "scheme", "port", "protected-ref", "protected-epoch"} {
		t.Run(mode, func(t *testing.T) {
			pool := NewAccountPool()
			first, second := poolFixture("first", 10), poolFixture("second", 1)
			second.Key = first.Key
			first.MaxConcurrency, second.MaxConcurrency = 1, 1
			switch mode {
			case "key":
				second.Key = "other-synthetic-key"
			case "host":
				second.BaseURL = "https://other.test"
			case "mount":
				second.BaseURL = "https://provider.test/different-tenant"
			case "scheme":
				second.BaseURL = "http://provider.test"
			case "port":
				second.BaseURL = "https://provider.test:8443"
			default:
				first.SourceType, second.SourceType = "official_account", "official_account"
				first.CredentialRef, second.CredentialRef = "synthetic-first-ref", "synthetic-first-ref"
				first.CredentialEpoch, second.CredentialEpoch = 1, 1
				if mode == "protected-ref" {
					second.CredentialRef = "synthetic-second-ref"
				} else {
					second.CredentialEpoch = 2
				}
			}
			accounts := []config.LocalAccount{first, second}
			_, release, err := pool.Select("codex", accounts, "coding", "gpt-5", "")
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			pool.MarkFailureFor(first.ID, config.AccountFingerprint(first), time.Minute)
			got, extraRelease, err := pool.Select("codex", accounts, "coding", "gpt-5", "")
			if err != nil || got.ID != second.ID {
				t.Fatalf("independent upstream identity inherited another credential's budget: %v", err)
			}
			extraRelease()
		})
	}
}

func TestCredentialBudgetInvalidIdentityFailsClosed(t *testing.T) {
	for _, mode := range []string{"endpoint", "empty-key", "key-control", "missing-ref", "missing-epoch"} {
		t.Run(mode, func(t *testing.T) {
			pool := NewAccountPool()
			account := poolFixture("invalid", 1)
			switch mode {
			case "endpoint":
				account.BaseURL = "https://provider.test?tenant=other"
			case "empty-key":
				account.Key = ""
			case "key-control":
				account.Key = "synthetic\x00key"
			case "missing-ref":
				account.SourceType, account.CredentialEpoch = "official_account", 1
			case "missing-epoch":
				account.SourceType, account.CredentialRef = "official_account", "synthetic-ref"
			}
			if _, release, err := pool.Select("codex", []config.LocalAccount{account}, "coding", "gpt-5", ""); err == nil {
				release()
				t.Fatal("invalid upstream identity was admitted")
			}
			if len(pool.budgets) != 0 || len(pool.bindings) != 0 {
				t.Fatal("invalid identity created a shared unknown-credential bucket")
			}
		})
	}
}

func TestCredentialBudgetPerCardLimitPersistsAcrossKeyEdit(t *testing.T) {
	pool := NewAccountPool()
	account := poolFixture("edited", 1)
	account.MaxConcurrency = 1
	_, oldRelease, err := pool.Select("codex", []config.LocalAccount{account}, "coding", "gpt-5", "")
	if err != nil {
		t.Fatal(err)
	}
	defer oldRelease()
	replacement := account
	replacement.Key = "replacement-synthetic-key"
	if _, release, err := pool.Select("codex", []config.LocalAccount{replacement}, "coding", "gpt-5", ""); err != errPoolCapacity {
		if release != nil {
			release()
		}
		t.Fatalf("editing Key bypassed the card's own active request ceiling: %v", err)
	}
	oldRelease()
	if _, release, err := pool.Select("codex", []config.LocalAccount{replacement}, "coding", "gpt-5", ""); err != nil {
		t.Fatalf("released original request kept replacement card busy: %v", err)
	} else {
		release()
	}
}

func TestCredentialBudgetRetentionDoesNotEvictActiveOrCoolingEntries(t *testing.T) {
	pool := NewAccountPool()
	pool.maxStates, pool.maxBudgets, pool.maxBindings, pool.retention = 2, 2, 2, time.Minute
	now := time.Unix(100, 0)
	pool.now = func() time.Time { return now }
	active, cooling, next := poolFixture("active", 1), poolFixture("cooling", 1), poolFixture("next", 1)
	_, activeRelease, err := pool.Select("codex", []config.LocalAccount{active}, "coding", "gpt-5", "")
	if err != nil {
		t.Fatal(err)
	}
	defer activeRelease()
	_, coolingRelease, err := pool.Select("codex", []config.LocalAccount{cooling}, "coding", "gpt-5", "")
	if err != nil {
		t.Fatal(err)
	}
	pool.MarkFailureFor(cooling.ID, config.AccountFingerprint(cooling), 5*time.Minute)
	coolingRelease()
	now = now.Add(2 * time.Minute)
	if _, release, err := pool.Select("codex", []config.LocalAccount{next}, "coding", "gpt-5", ""); err != errPoolCapacity {
		if release != nil {
			release()
		}
		t.Fatalf("history bound evicted active/cooling budgets: %v", err)
	}
	if len(pool.budgets) != 2 || len(pool.bindings) != 2 {
		t.Fatal("active/cooling lifetime was lost during sweep")
	}
	activeRelease()
	now = now.Add(4 * time.Minute)
	if _, release, err := pool.Select("codex", []config.LocalAccount{next}, "coding", "gpt-5", ""); err != nil {
		t.Fatalf("expired inactive budget was never reclaimed: %v", err)
	} else {
		release()
	}
	pool.MarkFailureFor(active.ID, config.AccountFingerprint(active), time.Minute)
	if len(pool.budgets) != 1 || len(pool.bindings) != 1 || len(pool.state) != 1 {
		t.Fatal("late response recreated an expired retired budget")
	}
}

func TestCredentialBudgetBindingHistoryBoundPreservesInflightRevisions(t *testing.T) {
	pool := NewAccountPool()
	pool.maxBindings, pool.retention = 2, time.Minute
	now := time.Unix(100, 0)
	pool.now = func() time.Time { return now }
	account := poolFixture("changing", 1)
	_, firstRelease, err := pool.Select("codex", []config.LocalAccount{account}, "coding", "gpt-5", "")
	if err != nil {
		t.Fatal(err)
	}
	defer firstRelease()
	changed := account
	changed.Protocol = "responses"
	_, secondRelease, err := pool.Select("codex", []config.LocalAccount{changed}, "coding", "gpt-5", "")
	if err != nil {
		t.Fatal(err)
	}
	defer secondRelease()
	changed.Model = "grok-4"
	now = now.Add(2 * time.Minute)
	if _, release, err := pool.Select("codex", []config.LocalAccount{changed}, "coding", "grok-4", ""); err != errPoolCapacity {
		if release != nil {
			release()
		}
		t.Fatalf("new revision evicted active budget bindings: %v", err)
	}
	firstRelease()
	secondRelease()
	now = now.Add(2 * time.Minute)
	if _, release, err := pool.Select("codex", []config.LocalAccount{changed}, "coding", "grok-4", ""); err != nil {
		t.Fatalf("released expired binding history prevented future use: %v", err)
	} else {
		release()
	}
	if len(pool.bindings) != 1 {
		t.Fatal("obsolete binding history was not bounded")
	}
}

func TestCredentialBudgetEqualLoadRotatesIndependentSessions(t *testing.T) {
	pool := NewAccountPool()
	pool.now = func() time.Time { return time.Unix(100, 0) }
	first, second := poolFixture("first", 1), poolFixture("second", 1)
	accounts := []config.LocalAccount{first, second}
	for index, want := range []string{"first", "second", "first", "second"} {
		selected, release, err := pool.Select("codex", accounts, "coding", "gpt-5", string(rune('a'+index)))
		if err != nil {
			t.Fatal(err)
		}
		release()
		if selected.ID != want {
			t.Fatalf("independent serial turn %d did not rotate: got %s, want %s", index, selected.ID, want)
		}
	}
	// A card first introduced after another card's use must still get its turn.
	third := poolFixture("never-used", 1)
	selected, release, err := pool.Select("codex", append(accounts, third), "coding", "gpt-5", "new-session")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if selected.ID != third.ID {
		t.Fatal("never-used card was ranked after previously admitted cards")
	}
	// A healthy session affinity is stronger than the admission tie breaker.
	selected, release, err = pool.Select("codex", append(accounts, third), "coding", "gpt-5", "a")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if selected.ID != first.ID {
		t.Fatal("rotation replaced a still-healthy sticky session binding")
	}
}

func TestCredentialBudgetRoundRobinKeepsNormalizedWeightAndPriority(t *testing.T) {
	pool := NewAccountPool()
	first, weighted := poolFixture("first", 1), poolFixture("weighted", 1)
	first.MaxConcurrency, weighted.MaxConcurrency, weighted.Weight = 6, 6, 3
	accounts := []config.LocalAccount{first, weighted}
	releases := make([]func(), 0, 5)
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	for index, want := range []string{"first", "weighted", "weighted", "weighted", "first"} {
		selected, release, err := pool.Select("codex", accounts, "coding", "gpt-5", "")
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
		if selected.ID != want {
			t.Fatalf("normalized weighted load changed at turn %d: got %s, want %s", index, selected.ID, want)
		}
	}
	for _, release := range releases {
		release()
	}
	weighted.Priority++
	selected, release, err := pool.Select("codex", []config.LocalAccount{first, weighted}, "coding", "gpt-5", "")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if selected.ID != weighted.ID {
		t.Fatal("equal-load rotation overrode explicit account priority")
	}
}
