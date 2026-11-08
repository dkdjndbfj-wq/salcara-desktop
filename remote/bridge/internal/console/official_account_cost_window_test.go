package console

import (
	"math"
	"salcara/bridge/internal/codexaccounts"
	"salcara/bridge/internal/usageledger"
	"testing"
	"time"
)

func TestOfficialCostColdWindowProjectionDoesNotMixRetainedAndWindow(t *testing.T) {
	a, r, _, now := costFixture()
	r.Records = r.Records[:1]
	old := r.Records[0]
	old.Time = now.Add(-24 * time.Hour).UnixMilli()
	old.EstimatedCost = &usageledger.Money{Amount: 99, Currency: "USD", Source: "user_pricebook", Version: 2}
	r.Records = append(r.Records, old)
	got, _ := estimateOfficialCost(a, r, accountCostSample{}, now)
	if got.LocalUsedUSD == nil || *got.LocalUsedUSD != 100 || got.TotalUSD == nil || *got.TotalUSD != 4 || got.WindowUsedUSD == nil || *got.WindowUsedUSD != 1 || got.RemainingUSD == nil || *got.RemainingUSD != 3 || got.Status != "partial" || got.Reason != "observed_window_api_equivalent" {
		t.Fatal(got)
	}
	if got.FreshUntil == nil || !got.FreshUntil.Equal(*a.Usage.FreshUntil) {
		t.Fatal("missing freshness")
	}
}

func TestOfficialCostWindowsHaveIndependentAmountsAndCalibration(t *testing.T) {
	a, r, _, now := costFixture()
	r.Records = r.Records[:1]
	primarySeconds := int64(5 * 3600)
	primaryReset := now.Add(time.Hour)
	a.Usage.RateLimit.Primary = &codexaccounts.UsageWindow{UsedPercent: costFloat(10), WindowSeconds: &primarySeconds, ResetsAt: &primaryReset}
	s := &Server{accountCostSamples: map[string]accountCostSample{}}
	out := s.estimateAccountCostWindows(a, r, now)
	p, q := out.Windows["primary"], out.Windows["secondary"]
	if p.TotalUSD == nil || *p.TotalUSD != 10 || q.TotalUSD == nil || *q.TotalUSD != 4 || p.WindowSeconds != 5*3600 || q.WindowSeconds != 7200 {
		t.Fatal(out)
	}
	if *out.TotalUSD != 4 || out.Scope != "secondary_window" {
		t.Fatal("both overlapping windows counted", out)
	}
	if len(s.accountCostSamples) != 2 || s.accountCostSamples[costSampleKey(a.ID, "primary")].used != 10 || s.accountCostSamples[costSampleKey(a.ID, "secondary")].used != 25 {
		t.Fatal("shared baseline", s.accountCostSamples)
	}
	// Resetting only the short window must not invalidate or repurpose the long
	// window's baseline; the period is data, never inferred from a display label.
	longBefore := s.accountCostSamples[costSampleKey(a.ID, "secondary")]
	reset := primaryReset.Add(5 * time.Hour)
	a.Usage.RateLimit.Primary.ResetsAt = &reset
	s.estimateAccountCostWindows(a, r, now)
	if s.accountCostSamples[costSampleKey(a.ID, "secondary")] != longBefore {
		t.Fatal("primary reset changed secondary")
	}
}

func TestOfficialCostLearnedWindowRestoresAndExpiresSafely(t *testing.T) {
	a, r, base, now := costFixture()
	_, learned := estimateOfficialCost(a, r, base, now)
	restored := sampleFromDisk(learned.toDisk())
	if restored != learned || restored.total != 60 {
		t.Fatal("lost persistence fields")
	}
	// Repeat the baseline timestamp with a stored learned total: same-window
	// estimates remain coherent without adding all-time local spending to it.
	a.Usage.ObservedAt = &now
	a.Usage.FetchStartedAt = &now
	got, _ := estimateOfficialCost(a, usageledger.AccountCostRecords{}, restored, now)
	if got.TotalUSD == nil || *got.TotalUSD != 60 || math.Abs(*got.WindowUsedUSD+*got.RemainingUSD-*got.TotalUSD) > 1e-10 {
		t.Fatal(got)
	}
	expired := now.Add(-time.Second)
	a.Usage.FreshUntil = &expired
	got, _ = estimateOfficialCost(a, r, restored, now)
	if got.TotalUSD != nil || got.RemainingUSD != nil {
		t.Fatal("expired sample displayed", got)
	}
}

func TestOfficialCostUnknownRecordsAndChangedPriceDoNotBootstrap(t *testing.T) {
	for _, kind := range []string{"unpriced", "mixed-current", "unfinished", "missing-cache", "zero-percent"} {
		t.Run(kind, func(t *testing.T) {
			a, r, _, now := costFixture()
			r.Records = r.Records[:1]
			switch kind {
			case "unpriced", "missing-cache":
				r.Records[0].EstimatedCost = nil
			case "mixed-current":
				r.PriceKey = "user:3/reference:20261013"
			case "unfinished":
				r.Records[0].State = "interrupted"
			case "zero-percent":
				a.Usage.RateLimit.Secondary.UsedPercent = costFloat(0)
			}
			got, _ := estimateOfficialCost(a, r, accountCostSample{}, now)
			if got.TotalUSD != nil || got.RemainingUSD != nil {
				t.Fatal("unproven projection", got)
			}
		})
	}
}

func TestAPIServiceDollarSummaryNeverTurnsUnknownIntoZero(t *testing.T) {
	a, r, b, now := costFixture()
	cost, _ := estimateOfficialCost(a, r, b, now)
	rows := []apiSummaryMember{{ID: "one", CostEstimate: &cost}, {ID: "unknown"}}
	got := sumAPIServiceCosts(rows, now)
	if got.TotalUSD != nil || got.WindowUsedUSD != nil || got.RemainingUSD != nil || got.KnownAccounts != 1 || got.TotalAccounts != 2 || got.Status != "partial" {
		t.Fatal(got)
	}
	other := cost
	other.WindowSeconds = 604800
	rows[1].CostEstimate = &other
	got = sumAPIServiceCosts(rows, now)
	if got.TotalUSD == nil || *got.TotalUSD != 120 || *got.WindowUsedUSD != 30 || *got.RemainingUSD != 90 || got.Scope != "account_windows" {
		t.Fatal(got)
	}
	got = sumAPIServiceCosts(rows, now.Add(time.Hour))
	if got.TotalUSD != nil || got.KnownAccounts != 0 {
		t.Fatal("expired pool sum", got)
	}
}
