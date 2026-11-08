package usageledger

import (
	"context"
	"time"
)

// AccountTokenFilter requires an exact anonymous account and inclusive time
// bounds. A nonzero epoch excludes legacy or replaced official identities.
type AccountTokenFilter struct {
	AccountID    string
	AccountEpoch uint64
	From, To     int64
}

// AccountTokenRecord exposes only the fields needed by local Token charts.
type AccountTokenRecord struct {
	Time       int64
	Model      string
	Tokens     *int64
	UsageFinal bool
}

// AccountTokenStatus includes the earliest millisecond allowed by the durable
// read's retention policy, independent of the caller's requested date range.
type AccountTokenStatus struct {
	Snapshot
	RetentionCutoff int64
}

// VisitAccountTokens visits one detached durable snapshot, capped at MaxRecords.
// It shares Query's read/revision fence without rebuilding its paginated
// aggregates. The required scope and 91-day cap cover a 90-day local-calendar
// chart even across DST; callers cannot use this as an unscoped export API.
// Status describes the snapshot before callbacks run. Errors make the entire
// visit unusable, including any callbacks already invoked on cancellation.
func (l *Ledger) VisitAccountTokens(ctx context.Context, f AccountTokenFilter, visit func(AccountTokenRecord)) (AccountTokenStatus, error) {
	if f.AccountID == "" || f.From <= 0 || f.To <= 0 || f.To-f.From > (91*24*time.Hour).Milliseconds() || visit == nil {
		return AccountTokenStatus{}, ErrQuery
	}
	filter, err := normalizeFilter(QueryFilter{AccountID: f.AccountID, From: f.From, To: f.To, Sort: "time", Direction: "asc"})
	if err != nil {
		return AccountTokenStatus{}, err
	}
	read, err := l.readQueryRecords(ctx, filter)
	if err != nil {
		return AccountTokenStatus{}, err
	}
	for _, record := range read.records {
		if err := ctx.Err(); err != nil {
			return AccountTokenStatus{}, err
		}
		if record.AccountID == f.AccountID && (f.AccountEpoch == 0 || record.AccountEpoch == f.AccountEpoch) {
			visit(AccountTokenRecord{Time: record.Time, Model: record.Model, Tokens: record.Tokens.Total, UsageFinal: record.UsageFinal})
		}
	}
	if err := ctx.Err(); err != nil {
		return AccountTokenStatus{}, err
	}
	return AccountTokenStatus{Snapshot: read.status, RetentionCutoff: read.cutoff}, nil
}
