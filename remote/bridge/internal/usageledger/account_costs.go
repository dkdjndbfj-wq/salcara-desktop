package usageledger

import (
	"fmt"
	"sync"
	"time"
)

// AccountCostScope is an internal, non-serialized credential generation. Legacy
// records without an epoch must never be reassigned to a newly imported login.
type AccountCostScope struct {
	ID    string
	Epoch uint64
}

type AccountCostRecords struct {
	Records           []Record
	Incomplete        bool
	Coverage          uint64
	CoverageChangedAt time.Time
	LatestActivity    time.Time
	ScopeKey          string // consumer-owned, in-memory source-set fence (never serialized)
	PriceKey          string
}

type accountCostActivity struct {
	active int
	at     time.Time
}

func (l *Ledger) invalidateCostCoverageLocked() {
	l.costCoverage++
	l.costCoverageAt = time.Now()
	l.calibrations = nil
}

func priceKey(book PriceBook) string {
	return fmt.Sprintf("user:%d/reference:%d", book.Version, OfficialReferenceVersion)
}

// BeginAccountUsage is paired with finish after the final observation is
// queued. It covers streams not yet represented in the ledger's write queue.
func (l *Ledger) BeginAccountUsage(id string, epoch uint64) func() {
	if l == nil || id == "" || epoch == 0 {
		return func() {}
	}
	l.mu.Lock()
	if l.costActivity == nil {
		l.costActivity = map[AccountCostScope]accountCostActivity{}
	}
	key := AccountCostScope{l.anonymous("acct-", id), epoch}
	if len(l.costActivity) >= MaxRecords {
		for k, v := range l.costActivity {
			if v.active == 0 {
				delete(l.costActivity, k)
			}
		}
		l.invalidateCostCoverageLocked()
	}
	if _, exists := l.costActivity[key]; !exists && len(l.costActivity) >= MaxRecords {
		// Defensive bound even if a caller bypasses all normal admission caps.
		l.costOverflow++
		l.mu.Unlock()
		var once sync.Once
		return func() {
			once.Do(func() { l.mu.Lock(); l.costOverflow--; l.invalidateCostCoverageLocked(); l.mu.Unlock() })
		}
	}
	v := l.costActivity[key]
	v.active++
	v.at = time.Now()
	l.costActivity[key] = v
	l.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			v := l.costActivity[key]
			v.active--
			v.at = time.Now()
			l.costActivity[key] = v
		})
	}
}

// AccountCosts takes one bounded, consistent in-memory snapshot for all cards.
// It does no disk/network IO and never returns other accounts' records. Estimates
// cover retained local requests only, not provider bills or account-wide usage.
func (l *Ledger) AccountCosts(scopes []AccountCostScope) map[AccountCostScope]AccountCostRecords {
	out := make(map[AccountCostScope]AccountCostRecords, len(scopes))
	if l == nil {
		return out
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	lookup := make(map[AccountCostScope]AccountCostScope, len(scopes))
	for _, scope := range scopes {
		if scope.ID == "" || scope.Epoch == 0 {
			continue
		}
		lookup[AccountCostScope{l.anonymous("acct-", scope.ID), scope.Epoch}] = scope
		activity := l.costActivity[AccountCostScope{l.anonymous("acct-", scope.ID), scope.Epoch}]
		out[scope] = AccountCostRecords{Records: []Record{}, Coverage: l.costCoverage, CoverageChangedAt: l.costCoverageAt, LatestActivity: activity.at, Incomplete: activity.active > 0, PriceKey: priceKey(l.prices)}
	}
	rows := prune(l.records, l.retention, time.Now())
	incomplete := l.costOverflow > 0 || l.pending != 0 || l.dropped != 0 || l.unsaved || l.blockedRead || l.closed || l.saveError != "" || l.readError != "" || len(rows) >= l.retention.MaxRecords || len(rows) != len(l.records)
	for _, r := range rows {
		if scope, ok := lookup[AccountCostScope{r.AccountID, r.AccountEpoch}]; ok {
			group := out[scope]
			group.Records = append(group.Records, cloneRecord(r))
			out[scope] = group
		}
	}
	for scope, group := range out {
		group.Incomplete = group.Incomplete || incomplete
		out[scope] = group
	}
	return out
}
