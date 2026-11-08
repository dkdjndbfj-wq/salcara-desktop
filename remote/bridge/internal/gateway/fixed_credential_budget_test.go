package gateway

import (
	"errors"
	"testing"
	"time"

	"salcara/bridge/internal/config"
)

func TestFixedRoutesAndPoolShareCredentialBudget(t *testing.T) {
	for _, fixedFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "pool-first", true: "fixed-first"}[fixedFirst], func(t *testing.T) {
			p := NewAccountPool()
			pooled, fixed := poolFixture("pooled", 1), poolFixture("fixed", 1)
			fixed.PoolEnabled = false
			fixed.PoolGroup = ""
			fixed.Kind = "claude"
			fixed.Key = pooled.Key
			pooled.MaxConcurrency, fixed.MaxConcurrency = 20, 1
			all := []config.LocalAccount{pooled, fixed}
			var release func()
			var err error
			if fixedFirst {
				release, err = p.AdmitFixed(fixed, all)
			} else {
				_, release, err = p.Select("codex", all, "coding", "gpt-5", "")
			}
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			var extra func()
			if fixedFirst {
				_, extra, err = p.Select("codex", all, "coding", "gpt-5", "")
				if !errors.Is(err, errPoolCapacity) {
					t.Fatalf("pool ignored fixed lease: %v", err)
				}
			} else {
				extra, err = p.AdmitFixed(fixed, all)
				if !errors.Is(err, ErrCredentialCapacity) {
					t.Fatalf("fixed ignored pool lease: %v", err)
				}
			}
			if extra != nil {
				extra()
			}
			release()
			release, err = p.AdmitFixed(fixed, all)
			if err != nil {
				t.Fatal(err)
			}
			release()
		})
	}
}

func TestFixedTrustedVirtualAccountSharesProtectedBudget(t *testing.T) {
	p := NewAccountPool()
	card := poolFixture("saved", 1)
	card.SourceType = "official_account"
	card.CredentialRef = "synthetic-ref"
	card.CredentialEpoch = 3
	card.MaxConcurrency = 1
	virtual := card
	virtual.ID = "named-client"
	virtual.PoolEnabled = false
	virtual.PoolGroup = ""
	virtual.MaxConcurrency = 50
	release, err := p.AdmitFixed(virtual, []config.LocalAccount{card})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, release, err := p.Select("codex", []config.LocalAccount{card}, "coding", "gpt-5", ""); !errors.Is(err, errPoolCapacity) {
		if release != nil {
			release()
		}
		t.Fatalf("virtual account multiplied saved budget: %v", err)
	}
	release()
	p.MarkFailureFor(virtual.ID, config.AccountFingerprint(virtual), time.Minute)
	if _, err := p.AdmitFixed(card, []config.LocalAccount{card}); !errors.Is(err, ErrCredentialCoolingDown) {
		t.Fatalf("fixed protected route ignored cooldown: %v", err)
	}
}

func TestFixedBudgetEditingCannotFreeOrCoolReplacement(t *testing.T) {
	p := NewAccountPool()
	old := poolFixture("fixed", 1)
	old.PoolEnabled = false
	old.MaxConcurrency = 1
	release, err := p.AdmitFixed(old, []config.LocalAccount{old})
	if err != nil {
		t.Fatal(err)
	}
	updated := old
	updated.Key = "synthetic-replacement"
	// Same card admission stays conservative until its old stream drains.
	release()
	newRelease, err := p.AdmitFixed(updated, []config.LocalAccount{updated})
	if err != nil {
		t.Fatal(err)
	}
	defer newRelease()
	release()
	p.MarkFailureFor(old.ID, config.AccountFingerprint(old), time.Minute)
	if _, extra, err := p.Select("codex", []config.LocalAccount{updated}, "coding", "gpt-5", ""); err == nil {
		extra()
		t.Fatal("non-pool card entered selector")
	}
	newRelease()
	if next, err := p.AdmitFixed(updated, []config.LocalAccount{updated}); err != nil {
		t.Fatal("late failure cooled replacement")
	} else {
		next()
	}
}

func TestFixedBudgetRejectsInvalidOrBoundedIdentity(t *testing.T) {
	p := NewAccountPool()
	invalid := poolFixture("bad", 1)
	invalid.Key = ""
	if release, err := p.AdmitFixed(invalid, nil); err == nil {
		release()
		t.Fatal("invalid fixed credential admitted")
	}
	p.maxBudgets = 1
	first, other := poolFixture("first", 1), poolFixture("other", 1)
	release, err := p.AdmitFixed(first, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if extra, err := p.AdmitFixed(other, nil); !errors.Is(err, ErrCredentialCapacity) {
		if extra != nil {
			extra()
		}
		t.Fatal("fixed route bypassed budget hard bound")
	}
}
