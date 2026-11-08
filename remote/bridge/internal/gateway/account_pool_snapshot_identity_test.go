package gateway

import (
	"testing"
	"time"

	"salcara/bridge/internal/config"
)

func TestAccountPoolSnapshotActivityRequiresCurrentIdentity(t *testing.T) {
	for _, name := range []string{"api-key", "official-epoch", "same-credential-policy"} {
		t.Run(name, func(t *testing.T) {
			p := NewAccountPool()
			now := time.Date(2026, 10, 14, 1, 0, 0, 0, time.UTC)
			p.now = func() time.Time { return now }
			original := poolFixture("same-id", 1)
			if name == "official-epoch" {
				original.SourceType, original.ProviderID = "official_account", "openai"
				original.CredentialRef, original.CredentialEpoch = "synthetic-vault", 7
			}
			release, err := p.AdmitFixed(original, []config.LocalAccount{original})
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			rows := p.Snapshot([]config.LocalAccount{original})
			if len(rows) != 1 || rows[0].Inflight != 1 || !rows[0].LastUsedAt.Equal(now) {
				t.Fatal("current admitted identity lost its activity", rows)
			}
			replaced := original
			switch name {
			case "api-key":
				replaced.Key = "synthetic-replacement-key"
			case "official-epoch":
				replaced.CredentialEpoch++
			case "same-credential-policy":
				replaced.Model = "different-model"
			}
			assertNoRetiredActivity := func(phase string) {
				t.Helper()
				for _, row := range p.Snapshot([]config.LocalAccount{replaced}) {
					if row.Inflight != 0 || !row.LastUsedAt.IsZero() {
						t.Fatalf("%s replacement inherited retired activity: %+v", phase, row)
					}
				}
			}
			assertNoRetiredActivity("inflight")
			release()
			assertNoRetiredActivity("finished")
			rows = p.Snapshot([]config.LocalAccount{original})
			if len(rows) != 1 || rows[0].Inflight != 0 || !rows[0].LastUsedAt.Equal(now) {
				t.Fatal("presentation filtering mutated the old lease or recent-use history", rows)
			}
		})
	}
}

func TestAccountPoolSnapshotKeepsCurrentSharedCredentialHealth(t *testing.T) {
	p := NewAccountPool()
	now := time.Date(2026, 10, 14, 2, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }
	original, alias := poolFixture("same-id", 1), poolFixture("current-alias", 1)
	alias.Key = "synthetic-current-key"
	releaseOld, err := p.AdmitFixed(original, []config.LocalAccount{original, alias})
	if err != nil {
		t.Fatal(err)
	}
	defer releaseOld()
	releaseAlias, err := p.AdmitFixed(alias, []config.LocalAccount{original, alias})
	if err != nil {
		t.Fatal(err)
	}
	defer releaseAlias()
	p.MarkFailureFor(alias.ID, config.AccountFingerprint(alias), time.Minute)
	p.MarkFailureFor(original.ID, config.AccountFingerprint(original), 2*time.Minute)
	replaced := original
	replaced.Key = alias.Key
	rows := p.Snapshot([]config.LocalAccount{replaced, alias})
	if len(rows) != 2 {
		t.Fatal("current shared budget health was omitted", rows)
	}
	if rows[0].AccountID != replaced.ID || rows[0].Inflight != 0 || !rows[0].LastUsedAt.IsZero() {
		t.Fatal("replacement borrowed activity from an old identity or credential alias", rows[0])
	}
	if rows[1].AccountID != alias.ID || rows[1].Inflight != 1 || !rows[1].LastUsedAt.Equal(now) {
		t.Fatal("current alias activity was hidden", rows[1])
	}
	for _, row := range rows {
		if row.Failures != 1 || !row.CoolingDown || !row.CooldownUntil.Equal(now.Add(time.Minute)) {
			t.Fatal("health came from the old credential instead of the current shared budget", row)
		}
	}
	releaseOld()
	releaseAlias()
	rows = p.Snapshot([]config.LocalAccount{replaced, alias})
	if len(rows) != 2 || rows[0].Inflight != 0 || !rows[0].LastUsedAt.IsZero() || rows[1].Inflight != 0 || rows[0].Failures != 1 || rows[1].Failures != 1 {
		t.Fatal("late release changed replacement activity or shared health", rows)
	}
}

func TestAccountPoolSnapshotOverlappingIdentityLeasesStaySeparate(t *testing.T) {
	p := NewAccountPool()
	now := time.Date(2026, 10, 14, 3, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }
	original := poolFixture("same-id", 1)
	releaseOld, err := p.AdmitFixed(original, []config.LocalAccount{original})
	if err != nil {
		t.Fatal(err)
	}
	defer releaseOld()
	replaced := original
	replaced.Key = "synthetic-replacement-key"
	now = now.Add(time.Minute)
	releaseNew, err := p.AdmitFixed(replaced, []config.LocalAccount{replaced})
	if err != nil {
		t.Fatal(err)
	}
	defer releaseNew()
	rows := p.Snapshot([]config.LocalAccount{replaced})
	if len(rows) != 1 || rows[0].Inflight != 1 || !rows[0].LastUsedAt.Equal(now) {
		t.Fatal("replacement borrowed old lease activity", rows)
	}
	releaseNew()
	rows = p.Snapshot([]config.LocalAccount{replaced})
	if len(rows) != 1 || rows[0].Inflight != 0 || !rows[0].LastUsedAt.Equal(now) {
		t.Fatal("completed replacement still displayed a retired in-flight lease", rows)
	}
	releaseOld()
	rows = p.Snapshot([]config.LocalAccount{replaced})
	if len(rows) != 1 || rows[0].Inflight != 0 || !rows[0].LastUsedAt.Equal(now) {
		t.Fatal("old release mutated current identity activity", rows)
	}
}

func TestAccountPoolSnapshotValidationCannotFabricateRecentUse(t *testing.T) {
	p := NewAccountPool()
	now := time.Date(2026, 10, 14, 4, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }
	original := poolFixture("same-id", 1)
	release, err := p.AdmitFixed(original, []config.LocalAccount{original})
	if err != nil {
		t.Fatal(err)
	}
	release()
	replaced := original
	replaced.Key = "synthetic-replacement-key"
	now = now.Add(time.Minute)
	if err := p.ValidateFixedModels(replaced, "gpt-5"); err != nil {
		t.Fatal(err)
	}
	for _, row := range p.Snapshot([]config.LocalAccount{replaced}) {
		if row.Inflight != 0 || !row.LastUsedAt.IsZero() {
			t.Fatal("validation inherited a retired request timestamp", row)
		}
	}
	// Creating an eligible pool candidate is also not request admission.
	p.mu.Lock()
	if state := p.ensureAccountStateLocked(replaced, now); state == nil {
		p.mu.Unlock()
		t.Fatal("synthetic candidate was not valid")
	}
	p.mu.Unlock()
	for _, row := range p.Snapshot([]config.LocalAccount{replaced}) {
		if row.Inflight != 0 || !row.LastUsedAt.IsZero() {
			t.Fatal("candidate discovery fabricated recent request activity", row)
		}
	}
}

func TestAccountPoolSnapshotSharedCredentialAliasesKeepOwnActivity(t *testing.T) {
	p := NewAccountPool()
	firstAt := time.Date(2026, 10, 14, 5, 0, 0, 0, time.UTC)
	now := firstAt
	p.now = func() time.Time { return now }
	first, second := poolFixture("first", 1), poolFixture("second", 1)
	second.Key = first.Key
	accounts := []config.LocalAccount{first, second}
	releaseFirst, err := p.AdmitFixed(first, accounts)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseFirst()
	now = now.Add(time.Minute)
	releaseSecond, err := p.AdmitFixed(second, accounts)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseSecond()
	rows := p.Snapshot(accounts)
	if len(rows) != 2 || rows[0].Inflight != 1 || rows[1].Inflight != 1 || !rows[0].LastUsedAt.Equal(firstAt) || !rows[1].LastUsedAt.Equal(now) {
		t.Fatal("aliases shared request activity as well as their credential budget", rows)
	}
	releaseFirst()
	rows = p.Snapshot(accounts)
	if len(rows) != 2 || rows[0].Inflight != 0 || rows[1].Inflight != 1 || !rows[0].LastUsedAt.Equal(firstAt) || !rows[1].LastUsedAt.Equal(now) {
		t.Fatal("one alias release changed the other's activity", rows)
	}
}
