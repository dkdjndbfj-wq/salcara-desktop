package console

import (
	"math"
	"strconv"
	"strings"
	"time"

	"salcara/bridge/internal/codexaccounts"
	"salcara/bridge/internal/usageledger"
)

// These are API-equivalent local estimates, never subscription prices, credit
// balances or provider charges. Unknown is null (not a manufactured zero).
type officialCostEstimate struct {
	UsedUSD       *float64                        `json:"usedUSD"`
	LocalUsedUSD  *float64                        `json:"localUsedUSD"`
	WindowUsedUSD *float64                        `json:"windowUsedUSD"`
	TotalUSD      *float64                        `json:"totalUSD"`
	RemainingUSD  *float64                        `json:"remainingUSD"`
	Status        string                          `json:"status"`
	Reason        string                          `json:"reason"`
	ObservedAt    *time.Time                      `json:"observedAt,omitempty"`
	Scope         string                          `json:"scope"`
	FreshUntil    *time.Time                      `json:"freshUntil,omitempty"`
	WindowResetAt *time.Time                      `json:"windowResetAt,omitempty"`
	WindowSeconds int64                           `json:"windowSeconds,omitempty"`
	PriceBasis    string                          `json:"priceBasis"`
	Windows       map[string]officialCostEstimate `json:"windows,omitempty"`
}
type officialAccountDisplay struct {
	codexaccounts.Account
}
type accountCostSample struct {
	epoch      uint64
	plan       string
	reset      time.Time
	seconds    int64
	at         time.Time
	used       float64
	coverage   uint64
	scopeKey   string
	validAfter time.Time
	priceKey   string
	total      float64
	learnedAt  time.Time
}

func (s *Server) officialAccountDisplays() []officialAccountDisplay {
	out := []officialAccountDisplay{}
	if s.d.CodexAccounts == nil {
		return out
	}
	// Cards expose provider quota/subscription observations. Do not turn Token
	// records and a changing quota percentage into a monetary estimate.
	for _, row := range s.d.CodexAccounts.List() {
		out = append(out, officialAccountDisplay{Account: row})
	}
	return out
}

func costSampleKey(id, slot string) string { return id + "\x00" + slot }

// The two native quota windows are independent. Their estimates cannot share a
// baseline or be added together: both windows meter the same physical requests.
// Caller owns accountCostMu; all returned child DTOs are detached values.
func (s *Server) estimateAccountCostWindows(row codexaccounts.Account, records usageledger.AccountCostRecords, now time.Time) officialCostEstimate {
	windows := map[string]officialCostEstimate{}
	if row.Usage.RateLimit != nil {
		for _, slot := range []string{"primary", "secondary"} {
			account := row
			limit := *row.Usage.RateLimit
			if slot == "primary" {
				if limit.Primary == nil {
					continue
				}
				limit.Secondary = nil
			} else {
				if limit.Secondary == nil {
					continue
				}
				limit.Primary = nil
			}
			account.Usage.RateLimit = &limit
			key := costSampleKey(row.ID, slot)
			if _, exists := s.accountCostSamples[key]; !exists {
				if saved, ok := s.usageLedger.CostCalibration(key); ok {
					s.accountCostSamples[key] = sampleFromDisk(saved)
				}
			}
			estimate, sample := estimateOfficialCost(account, records, s.accountCostSamples[key], now)
			s.accountCostSamples[key] = sample
			s.usageLedger.QueueCostCalibration(key, sample.toDisk())
			windows[slot] = estimate
		}
	}
	var out officialCostEstimate
	if value, ok := windows["secondary"]; ok {
		out = value
	} else if value, ok := windows["primary"]; ok {
		out = value
	} else {
		out, _ = estimateOfficialCost(row, records, accountCostSample{}, now)
	}
	if len(windows) > 0 {
		out.Windows = windows
	}
	return out
}

func eligibleCostRecord(r usageledger.Record) bool {
	return r.State != "rejected" && r.State != "read_only" && r.State != "local_estimate"
}
func usdEstimate(r usageledger.Record) (float64, bool) {
	m := r.EstimatedCost
	if !r.UsageFinal || m == nil || m.Currency != "USD" || (m.Source != "user_pricebook" && m.Source != "official_reference") || m.Version == 0 || !finiteCost(m.Amount) {
		return 0, false
	}
	return m.Amount, true
}
func finiteCost(n float64) bool { return !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 && n <= 1e12 }

func costWindow(a codexaccounts.Account, now time.Time) (accountCostSample, string, bool) {
	u := a.Usage
	if !u.Verified || u.Source != codexaccounts.UsageSourceOfficial || u.Status != codexaccounts.UsageStatusFresh || u.ObservedAt == nil || u.FreshUntil == nil || !u.FreshUntil.After(now) || u.ObservedAt.After(now) || u.RateLimit == nil {
		return accountCostSample{}, "", false
	}
	w, scope := u.RateLimit.Secondary, "secondary_window"
	if w == nil {
		w, scope = u.RateLimit.Primary, "primary_window"
	}
	if w == nil || w.UsedPercent == nil || !finiteCost(*w.UsedPercent) || *w.UsedPercent > 100 || w.ResetsAt == nil || !w.ResetsAt.After(now) {
		return accountCostSample{}, "", false
	}
	seconds := int64(0)
	if w.WindowSeconds != nil {
		seconds = *w.WindowSeconds
	} else if w.WindowMinutes != nil && *w.WindowMinutes <= 60*24*31 {
		seconds = *w.WindowMinutes * 60
	}
	if seconds < 60 || seconds > 60*60*24*31 || u.ObservedAt.Before(w.ResetsAt.Add(-time.Duration(seconds)*time.Second)) {
		return accountCostSample{}, "", false
	}
	return accountCostSample{epoch: a.IdentityEpoch, plan: u.Plan.Raw, reset: *w.ResetsAt, seconds: seconds, at: *u.ObservedAt, used: *w.UsedPercent}, scope, true
}

// Learn only from two authenticated observations in the same reset window,
// with >=3 fully-priced completed local requests and >=1 percentage point of
// observed consumption. No plan-name dollar constants or credits-to-USD math.
// This projects the *sampled workload's* API value, not a guaranteed budget;
// other clients, model mix and provider metering can change the result.
func estimateOfficialCost(a codexaccounts.Account, records usageledger.AccountCostRecords, baseline accountCostSample, now time.Time) (officialCostEstimate, accountCostSample) {
	out := officialCostEstimate{Status: "unknown", Reason: "no_priced_local_records", Scope: "retained_local_requests"}
	known, unknown, sum := 0, 0, float64(0)
	basis := map[string]bool{}
	for _, r := range records.Records {
		if !eligibleCostRecord(r) {
			continue
		}
		if cost, ok := usdEstimate(r); ok {
			known++
			sum += cost
			basis[r.EstimatedCost.Source] = true
		} else {
			unknown++
		}
	}
	if known > 0 && finiteCost(sum) {
		out.UsedUSD = &sum
		out.LocalUsedUSD = &sum
		out.Status = "estimated"
		out.Reason = "learning_quota_sample"
		if unknown > 0 || records.Incomplete {
			out.Status = "partial"
			out.Reason = "incomplete_local_records"
		}
	}
	for source := range basis {
		out.PriceBasis = source
	}
	if len(basis) > 1 {
		out.PriceBasis = "mixed"
	}
	current, scope, valid := costWindow(a, now)
	current.coverage = records.Coverage
	current.scopeKey = records.ScopeKey
	current.priceKey = records.PriceKey
	if a.IdentityEpoch < baseline.epoch {
		out.Reason = "outdated_identity_sample"
		return out, baseline
	}
	if !valid {
		out.Reason = "quota_not_fresh"
		return out, baseline
	}
	out.ObservedAt = a.Usage.ObservedAt
	out.Scope = scope
	out.FreshUntil = a.Usage.FreshUntil
	if current.reset.Before(*out.FreshUntil) {
		reset := current.reset
		out.FreshUntil = &reset
	}
	reset := current.reset
	out.WindowResetAt = &reset
	out.WindowSeconds = current.seconds
	// Check chronology before reset/percentage comparisons. A stale HTTP
	// response with lower usage must not rewind the accepted baseline.
	if current.epoch == baseline.epoch && (current.at.Before(baseline.at) || current.at.Before(baseline.learnedAt)) {
		out.Reason = "outdated_quota_sample"
		return out, baseline
	}
	if records.Incomplete {
		out.Reason = "incomplete_local_records"
		return out, baseline
	}
	start := a.Usage.FetchStartedAt
	if start == nil || start.IsZero() || start.After(current.at) || records.LatestActivity.After(*start) {
		out.Reason = "quota_sample_overlaps_local_activity"
		return out, baseline
	}
	// A retained old quota snapshot cannot become a new baseline after records
	// were cleared/pruned/repriced, even if it is still inside its freshness TTL.
	if start.Before(records.CoverageChangedAt) || !baseline.validAfter.IsZero() && !start.After(baseline.validAfter) {
		out.Reason = "quota_predates_local_change"
		return out, baseline
	}
	if baseline.epoch == current.epoch && baseline.scopeKey != current.scopeKey {
		// Source-set edits have no historical timestamp in local configuration.
		// Demand a new observation begun after this first detected edit.
		current.at = time.Time{}
		current.validAfter = now
		out.Reason = "source_set_changed"
		return out, current
	}
	if baseline.epoch != current.epoch || baseline.coverage != current.coverage || baseline.priceKey != current.priceKey || baseline.scopeKey != current.scopeKey || baseline.plan != current.plan || !baseline.reset.Equal(current.reset) || baseline.seconds != current.seconds || baseline.at.IsZero() || current.used < baseline.used {
		// Cold start may show the deliberately lower-confidence Cockpit-style
		// observed-window projection. Identity/price/coverage invalidation never
		// reuses that previous identity's dollar figure.
		if baseline.epoch == 0 {
			projectObservedWindow(&out, current, records, *start)
		}
		return out, current
	}
	// A late older HTTP response cannot rewind the calibration baseline.
	if current.at.Equal(baseline.at) || current.used-baseline.used < 1 {
		if baseline.total > 0 {
			projectCostWindow(&out, current, baseline.total, "sampled_api_equivalent_not_balance")
		} else {
			projectObservedWindow(&out, current, records, *start)
		}
		return out, baseline
	}
	count, deltaCost := 0, float64(0)
	priceVersions := map[string]uint64{}
	for _, r := range records.Records {
		if !eligibleCostRecord(r) {
			continue
		}
		start, end := time.UnixMilli(r.Time), time.UnixMilli(r.Time+r.DurationMS)
		if !end.After(baseline.at) || !start.Before(current.at) {
			continue
		}
		cost, priced := usdEstimate(r)
		// Never price an overlapping in-flight turn against the wrong snapshot.
		if start.Before(baseline.at) || end.After(*a.Usage.FetchStartedAt) || !priced || r.State != "completed" {
			out.Reason = "incomplete_quota_sample"
			return out, current
		}
		if !costPriceCurrent(r, records.PriceKey) || priceVersions[r.EstimatedCost.Source] != 0 && priceVersions[r.EstimatedCost.Source] != r.EstimatedCost.Version {
			out.Reason = "mixed_price_versions"
			return out, current
		}
		priceVersions[r.EstimatedCost.Source] = r.EstimatedCost.Version
		count++
		deltaCost += cost
	}
	if count < 3 || deltaCost <= 0 || !finiteCost(deltaCost) {
		if baseline.total > 0 {
			projectCostWindow(&out, current, baseline.total, "sampled_api_equivalent_not_balance")
		} else {
			projectObservedWindow(&out, current, records, *start)
		}
		return out, baseline
	}
	total := deltaCost / (current.used - baseline.used) * 100
	if finiteCost(total) {
		baseline.total = total
		baseline.learnedAt = current.at
		projectCostWindow(&out, current, total, "sampled_api_equivalent_not_balance")
	}
	return out, baseline
}

func costPriceCurrent(r usageledger.Record, key string) bool {
	if key == "" {
		return true
	} // pure estimator callers have no live price fence
	if r.EstimatedCost == nil {
		return false
	}
	if r.EstimatedCost.Source == "official_reference" {
		return r.EstimatedCost.Version == usageledger.OfficialReferenceVersion
	}
	return strings.HasPrefix(key, "user:"+strconv.FormatUint(r.EstimatedCost.Version, 10)+"/")
}

func projectCostWindow(out *officialCostEstimate, current accountCostSample, total float64, reason string) {
	if !finiteCost(total) || total <= 0 {
		return
	}
	used, remaining := total*current.used/100, total*(100-current.used)/100
	if !finiteCost(used) || !finiteCost(remaining) {
		return
	}
	out.TotalUSD, out.WindowUsedUSD, out.RemainingUSD = &total, &used, &remaining
	out.Reason = reason
	if out.Status == "unknown" {
		out.Status = "estimated"
	}
}

func projectObservedWindow(out *officialCostEstimate, current accountCostSample, records usageledger.AccountCostRecords, fetchStart time.Time) {
	if records.Incomplete || current.used <= 0 {
		return
	}
	windowStart := current.reset.Add(-time.Duration(current.seconds) * time.Second)
	count, sum := 0, float64(0)
	for _, r := range records.Records {
		start, end := time.UnixMilli(r.Time), time.UnixMilli(r.Time+r.DurationMS)
		if !eligibleCostRecord(r) || end.Before(windowStart) || start.After(current.at) {
			continue
		}
		cost, ok := usdEstimate(r)
		if !ok || !costPriceCurrent(r, records.PriceKey) || r.State != "completed" || start.Before(windowStart) || end.After(fetchStart) {
			return
		}
		count++
		sum += cost
	}
	if count == 0 || sum <= 0 || !finiteCost(sum) {
		return
	}
	projectCostWindow(out, current, sum/current.used*100, "observed_window_api_equivalent")
	if out.TotalUSD != nil {
		out.Status = "partial"
	}
}

func (v accountCostSample) toDisk() usageledger.CostCalibration {
	return usageledger.CostCalibration{Epoch: v.epoch, Plan: v.plan, Reset: v.reset, Seconds: v.seconds, At: v.at, Used: v.used, Coverage: v.coverage, ScopeHash: v.scopeKey, ValidAfter: v.validAfter, PriceKey: v.priceKey, Total: v.total, LearnedAt: v.learnedAt}
}
func sampleFromDisk(v usageledger.CostCalibration) accountCostSample {
	return accountCostSample{epoch: v.Epoch, plan: v.Plan, reset: v.Reset, seconds: v.Seconds, at: v.At, used: v.Used, coverage: v.Coverage, scopeKey: v.ScopeHash, validAfter: v.ValidAfter, priceKey: v.PriceKey, total: v.Total, learnedAt: v.LearnedAt}
}
