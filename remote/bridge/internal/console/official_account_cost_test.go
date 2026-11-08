package console

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/codexaccounts"
	"salcara/bridge/internal/usageledger"
)

func costFloat(n float64) *float64 { return &n }
func costFixture() (codexaccounts.Account, usageledger.AccountCostRecords, accountCostSample, time.Time) {
	now := time.Now().UTC()
	reset, until, before, seconds := now.Add(time.Hour), now.Add(time.Minute), now.Add(-time.Hour), int64(7200)
	u := codexaccounts.UsageSnapshot{Status: "fresh", Verified: true, Source: codexaccounts.UsageSourceOfficial, ObservedAt: &now, FetchStartedAt: &now, FreshUntil: &until, Plan: codexaccounts.UsagePlan{Raw: "pro"}, RateLimit: &codexaccounts.UsageLimit{Secondary: &codexaccounts.UsageWindow{UsedPercent: costFloat(25), WindowSeconds: &seconds, ResetsAt: &reset}}}
	a := codexaccounts.Account{ID: "fixture", IdentityEpoch: 4, Usage: u}
	rows := usageledger.AccountCostRecords{}
	for i := 0; i < 3; i++ {
		rows.Records = append(rows.Records, usageledger.Record{Time: before.Add(time.Duration(i+1) * time.Minute).UnixMilli(), DurationMS: 10, State: "completed", UsageFinal: true, EstimatedCost: &usageledger.Money{Amount: 1, Currency: "USD", Source: "user_pricebook", Version: 2}})
	}
	return a, rows, accountCostSample{epoch: 4, plan: "pro", reset: reset, seconds: seconds, at: before, used: 20}, now
}

func TestOfficialAccountCostProjectsOnlyMatchedQuotaSamples(t *testing.T) {
	a, rows, base, now := costFixture()
	got, saved := estimateOfficialCost(a, rows, base, now)
	if got.UsedUSD == nil || *got.UsedUSD != 3 || got.RemainingUSD == nil || *got.RemainingUSD != 45 || got.Status != "estimated" || saved.at != base.at || saved.total != 60 || saved.learnedAt != now || got.TotalUSD == nil || *got.TotalUSD != 60 || got.WindowUsedUSD == nil || *got.WindowUsedUSD != 15 {
		t.Fatalf("bad estimate: %+v", got)
	}
	// Neither opaque credits nor subscription-price hints are used as dollars.
	a.Usage.Credits = &codexaccounts.UsageCredits{Balance: "999999"}
	a.Plan = "enterprise"
	other, _ := estimateOfficialCost(a, rows, base, now)
	if *other.RemainingUSD != 45 {
		t.Fatal("credits/plan affected estimates")
	}
}

func TestOfficialCostUnknownZeroPartialAreDistinct(t *testing.T) {
	a, rows, base, now := costFixture()
	got, _ := estimateOfficialCost(a, usageledger.AccountCostRecords{}, base, now)
	if got.UsedUSD != nil || got.RemainingUSD != nil || got.Status != "unknown" {
		t.Fatal(got)
	}
	rows.Records[0].EstimatedCost.Amount = 0
	rows.Records = rows.Records[:1]
	got, _ = estimateOfficialCost(a, rows, base, now)
	if got.UsedUSD == nil || *got.UsedUSD != 0 || got.RemainingUSD != nil {
		t.Fatal(got)
	}
	rows.Records = append(rows.Records, usageledger.Record{State: "interrupted"})
	got, _ = estimateOfficialCost(a, rows, base, now)
	if got.Status != "partial" || got.UsedUSD == nil || *got.UsedUSD != 0 {
		t.Fatal(got)
	}
}

func TestOfficialCostInvalidSamplesNeverProject(t *testing.T) {
	cases := map[string]func(*codexaccounts.Account, *usageledger.AccountCostRecords, *accountCostSample){
		"identity replaced": func(a *codexaccounts.Account, _ *usageledger.AccountCostRecords, _ *accountCostSample) {
			a.IdentityEpoch++
		},
		"quota stale": func(a *codexaccounts.Account, _ *usageledger.AccountCostRecords, _ *accountCostSample) {
			a.Usage.Status = "stale"
		},
		"unknown provenance": func(a *codexaccounts.Account, _ *usageledger.AccountCostRecords, _ *accountCostSample) {
			a.Usage.Source = "jwt_hint"
		},
		"reset": func(a *codexaccounts.Account, _ *usageledger.AccountCostRecords, _ *accountCostSample) {
			n := a.Usage.RateLimit.Secondary.ResetsAt.Add(time.Hour)
			a.Usage.RateLimit.Secondary.ResetsAt = &n
		},
		"quota regained": func(a *codexaccounts.Account, _ *usageledger.AccountCostRecords, _ *accountCostSample) {
			a.Usage.RateLimit.Secondary.UsedPercent = costFloat(10)
		},
		"missing duration": func(a *codexaccounts.Account, _ *usageledger.AccountCostRecords, _ *accountCostSample) {
			a.Usage.RateLimit.Secondary.WindowSeconds = nil
		},
		"NaN": func(a *codexaccounts.Account, _ *usageledger.AccountCostRecords, _ *accountCostSample) {
			a.Usage.RateLimit.Secondary.UsedPercent = costFloat(math.NaN())
		},
		"dropped records": func(_ *codexaccounts.Account, r *usageledger.AccountCostRecords, _ *accountCostSample) {
			r.Incomplete = true
		},
		"mixed prices": func(_ *codexaccounts.Account, r *usageledger.AccountCostRecords, _ *accountCostSample) {
			r.Records[0].EstimatedCost.Version++
		},
		"other currency": func(_ *codexaccounts.Account, r *usageledger.AccountCostRecords, _ *accountCostSample) {
			r.Records[0].EstimatedCost.Currency = "CNY"
		},
		"unfinished": func(_ *codexaccounts.Account, r *usageledger.AccountCostRecords, _ *accountCostSample) {
			r.Records[0].State = "interrupted"
		},
		"overlap baseline": func(_ *codexaccounts.Account, r *usageledger.AccountCostRecords, b *accountCostSample) {
			r.Records[0].Time = b.at.Add(-time.Second).UnixMilli()
			r.Records[0].DurationMS = 2000
		},
		"overlap current": func(a *codexaccounts.Account, r *usageledger.AccountCostRecords, _ *accountCostSample) {
			r.Records[0].DurationMS = a.Usage.ObservedAt.UnixMilli() - r.Records[0].Time + 1000
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			a, r, b, n := costFixture()
			change(&a, &r, &b)
			got, _ := estimateOfficialCost(a, r, b, n)
			if got.RemainingUSD != nil {
				t.Fatal(got)
			}
		})
	}
}

func TestOfficialCostDoesNotRewindNewerSample(t *testing.T) {
	a, r, b, n := costFixture()
	b.at = n.Add(time.Second)
	a.Usage.RateLimit.Secondary.UsedPercent = costFloat(1)
	got, saved := estimateOfficialCost(a, r, b, n)
	if got.RemainingUSD != nil || saved != b {
		t.Fatal("late response rewound estimator")
	}
}

func TestOfficialCostJSONNullAndNoCredentials(t *testing.T) {
	a, _, _, n := costFixture()
	estimate, _ := estimateOfficialCost(a, usageledger.AccountCostRecords{}, accountCostSample{}, n)
	raw, err := json.Marshal(estimate)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"usedUSD":null`) || !strings.Contains(string(raw), `"remainingUSD":null`) {
		t.Fatal(string(raw))
	}
}

func TestOfficialCostRejectsFetchRaceAndCoverageReset(t *testing.T) {
	for _, name := range []string{"clear", "inflight", "fetch-overlap", "no-bracket", "source-card-removed"} {
		t.Run(name, func(t *testing.T) {
			a, r, b, n := costFixture()
			switch name {
			case "clear":
				r.Coverage++
			case "inflight":
				r.Incomplete = true
			case "fetch-overlap":
				r.LatestActivity = n.Add(-time.Millisecond)
				start := n.Add(-time.Second)
				a.Usage.FetchStartedAt = &start
			case "no-bracket":
				a.Usage.FetchStartedAt = nil
			case "source-card-removed":
				b.scopeKey = "previous-card-id"
			}
			got, _ := estimateOfficialCost(a, r, b, n)
			if got.RemainingUSD != nil {
				t.Fatal("unproven sample projected")
			}
		})
	}
}

func TestOfficialCostDoesNotRewindNewIdentity(t *testing.T) {
	a, r, b, n := costFixture()
	b.epoch++
	got, saved := estimateOfficialCost(a, r, b, n)
	if got.RemainingUSD != nil || saved != b {
		t.Fatal("old identity reset new calibration")
	}
}

func TestOfficialCostBackgroundReadsDuringWorkDoNotEraseValidBaseline(t *testing.T) {
	a, r, b, n := costFixture()
	r.Incomplete = true
	paused, retained := estimateOfficialCost(a, r, b, n)
	if paused.RemainingUSD != nil || retained != b {
		t.Fatal("running requests erased calibration")
	}
	r.Incomplete = false
	got, _ := estimateOfficialCost(a, r, retained, n)
	if got.RemainingUSD == nil || *got.RemainingUSD != 45 {
		t.Fatal("idle refresh could not learn")
	}
}

func TestOfficialCostClearCannotRebaseCachedQuotaBeforeDeletion(t *testing.T) {
	a, r, b, n := costFixture()
	r.Coverage = 1
	r.CoverageChangedAt = n.Add(-30 * time.Minute)
	cached := a
	u := a.Usage
	u.ObservedAt = &b.at
	u.FetchStartedAt = &b.at
	cached.Usage = u
	_, kept := estimateOfficialCost(cached, r, b, n)
	if kept.coverage != 0 {
		t.Fatal("old cached quota rebound after clear")
	}
	got, newBase := estimateOfficialCost(a, r, kept, n)
	if got.RemainingUSD != nil || newBase.coverage != 1 || !newBase.at.Equal(n) {
		t.Fatal("fresh quota did not restart learning")
	}
}

func TestOfficialCostScopeChangeRequiresObservationAfterDetection(t *testing.T) {
	a, r, b, n := costFixture()
	b.scopeKey = "old-card"
	r.ScopeKey = "new-card"
	_, pending := estimateOfficialCost(a, r, b, n)
	if pending.validAfter != n || !pending.at.IsZero() {
		t.Fatal("source edit did not invalidate quota timestamp")
	}
	got, stillPending := estimateOfficialCost(a, r, pending, n.Add(time.Millisecond))
	if got.RemainingUSD != nil || stillPending != pending {
		t.Fatal("source edit reused old costs")
	}
	fresh := n.Add(time.Second)
	until := fresh.Add(time.Minute)
	a.Usage.FetchStartedAt = &fresh
	a.Usage.ObservedAt = &fresh
	a.Usage.FreshUntil = &until
	_, newBase := estimateOfficialCost(a, r, stillPending, fresh)
	if !newBase.at.Equal(fresh) || !newBase.validAfter.IsZero() {
		t.Fatal("fresh post-edit observation did not restart learning")
	}
}
