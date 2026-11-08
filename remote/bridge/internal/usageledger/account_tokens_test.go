package usageledger

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestVisitAccountTokensUsesOneScopedSnapshotBeyondPageLimit(t *testing.T) {
	l := testLedger(t)
	ctx := context.Background()
	start := time.Now().Add(-2 * time.Hour).Truncate(time.Millisecond)
	end := start.Add(time.Hour)
	appendRow := func(account string, epoch uint64, at time.Time, total *int64) {
		t.Helper()
		o := observedFixture()
		o.AccountID, o.AccountEpoch, o.StartedAt = account, epoch, at
		o.UsageFinal = true
		o.Tokens.Total = total
		if !l.Append(o) {
			t.Fatal("synthetic record was not accepted")
		}
	}
	for i := range 205 {
		appendRow("chosen", 9, start.Add(time.Duration(i)*time.Millisecond), token(1))
		if i%32 == 0 {
			flushLedger(t, l)
		}
	}
	appendRow("chosen", 9, end, token(7))
	appendRow("chosen", 9, start.Add(time.Minute), nil)
	appendRow("chosen", 9, start.Add(-time.Millisecond), token(1000))
	appendRow("chosen", 9, end.Add(time.Millisecond), token(1000))
	appendRow("other", 9, start, token(1000))
	appendRow("chosen", 8, start, token(1000))
	appendRow("chosen", 0, start, token(1000))
	flushLedger(t, l)
	filter := AccountTokenFilter{AccountID: l.AnonymousAccountID("chosen"), AccountEpoch: 9, From: start.UnixMilli(), To: end.UnixMilli()}
	visited, unknown, total := 0, 0, int64(0)
	readStarted := time.Now()
	status, err := l.VisitAccountTokens(ctx, filter, func(record AccountTokenRecord) {
		if !record.UsageFinal {
			t.Fatal("terminal usage marker was lost")
		}
		visited++
		if record.Tokens == nil {
			unknown++
		} else {
			total += *record.Tokens
		}
		if visited == 1 {
			// The complete read and its revision check must finish before any
			// callback. A paginated re-read would now lose rows or fail midway.
			if err := l.Clear(ctx); err != nil {
				t.Fatal(err)
			}
		}
	})
	if err != nil || visited != 207 || unknown != 1 || total != 212 || status.Count != 212 || status.Unsaved {
		t.Fatal("account/epoch/bounds/nullable Tokens or snapshot consistency lost", visited, unknown, total, status, err)
	}
	retentionAge := -time.Duration(status.Retention.Days) * 24 * time.Hour
	if status.RetentionCutoff < readStarted.Add(retentionAge).UnixMilli() || status.RetentionCutoff > time.Now().Add(retentionAge).UnixMilli() {
		t.Fatal("retention cutoff does not describe the read snapshot", status.RetentionCutoff)
	}
	if current := readQuery(t, l, QueryFilter{}); current.Total != 0 {
		t.Fatal("fixture clear did not commit", current.Total)
	}
}

func TestVisitAccountTokensRequiresBoundedScopeAndPropagatesReadFailure(t *testing.T) {
	l := testLedger(t)
	ctx := context.Background()
	start := time.Now().Add(-time.Hour).UnixMilli()
	valid := AccountTokenFilter{AccountID: l.AnonymousAccountID("chosen"), From: start, To: start + time.Hour.Milliseconds()}
	called := false
	visit := func(AccountTokenRecord) { called = true }
	for _, filter := range []AccountTokenFilter{
		{},
		{AccountID: "raw-account", From: valid.From, To: valid.To},
		{AccountID: valid.AccountID, To: valid.To},
		{AccountID: valid.AccountID, From: valid.From},
		{AccountID: valid.AccountID, From: valid.To, To: valid.From},
		{AccountID: valid.AccountID, From: valid.From, To: valid.From + (91 * 24 * time.Hour).Milliseconds() + 1},
	} {
		if _, err := l.VisitAccountTokens(ctx, filter, visit); !errors.Is(err, ErrQuery) || called {
			t.Fatal("unscoped or unbounded visit accepted", filter, err)
		}
	}
	if _, err := l.VisitAccountTokens(ctx, valid, nil); !errors.Is(err, ErrQuery) {
		t.Fatal("nil visitor accepted", err)
	}
	if status, err := l.VisitAccountTokens(ctx, valid, visit); err != nil || called || status.Count != 0 {
		t.Fatal("readable empty account did not remain distinguishable", status, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := l.VisitAccountTokens(canceled, valid, visit); !errors.Is(err, context.Canceled) || called {
		t.Fatal("cancellation turned into readable empty history", err)
	}
	if err := l.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := l.VisitAccountTokens(ctx, valid, visit); !errors.Is(err, ErrRead) || called {
		t.Fatal("closed ledger returned readable empty history", err)
	}
	if _, err := (*Ledger)(nil).VisitAccountTokens(ctx, valid, visit); !errors.Is(err, ErrRead) || called {
		t.Fatal("missing ledger returned readable empty history", err)
	}
}

func TestVisitAccountTokensKeepsPendingAndCorruptRecordsExplicit(t *testing.T) {
	l := testLedger(t)
	ctx := context.Background()
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce, writeOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	setLedgerWriter(l, func(state diskState) error {
		writeOnce.Do(func() { close(entered); <-release })
		return l.storage.write(state)
	})
	o := observedFixture()
	o.Tokens.Total = token(4)
	l.Append(o)
	<-entered
	filter := AccountTokenFilter{AccountID: l.AnonymousAccountID(o.AccountID), From: time.Now().Add(-time.Hour).UnixMilli(), To: time.Now().Add(time.Hour).UnixMilli()}
	count := 0
	status, err := l.VisitAccountTokens(ctx, filter, func(AccountTokenRecord) { count++ })
	if err != nil || count != 0 || !status.Unsaved || status.Count != 1 {
		t.Fatal("unsaved observations became a complete durable history", count, status, err)
	}
	releaseOnce.Do(func() { close(release) })
	flushLedger(t, l)
	status, err = l.VisitAccountTokens(ctx, filter, func(AccountTokenRecord) { count++ })
	if err != nil || count != 1 || status.Unsaved {
		t.Fatal("committed observation missing", count, status, err)
	}
	l.storage.mu.Lock()
	_, err = l.storage.db.Exec("UPDATE request_records SET payload=?", `{"id":"corrupt","unexpected":"synthetic"}`)
	l.storage.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.VisitAccountTokens(ctx, filter, func(AccountTokenRecord) { count++ }); !errors.Is(err, ErrRead) || count != 1 {
		t.Fatal("corruption returned empty success or invoked a visitor", count, err)
	}
}
