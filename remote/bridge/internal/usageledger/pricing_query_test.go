package usageledger

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func token(n int64) *int64    { return &n }
func rate(n float64) *float64 { return &n }
func priceFixture() ModelPrice {
	return ModelPrice{Model: "gpt-fixture", InputTokenBasis: "includes_cache", CacheCreationMode: "included_input", Rates: Rates{Input: rate(1), CachedInput: rate(.1), Output: rate(2)}}
}
func pricedObservation() Observation {
	o := observedFixture()
	o.UsageFinal = true
	o.Tokens = Tokens{Input: token(100), CachedInput: token(20), Output: token(10), Total: token(110)}
	return o
}
func readQuery(t *testing.T, l *Ledger, f QueryFilter) QueryResult {
	t.Helper()
	out, err := l.Query(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPricingUnknownZeroTierLongContextAndProviderSeparation(t *testing.T) {
	book := PriceBook{Version: 2, Entries: []ModelPrice{priceFixture()}}
	makeRecord := func() Record {
		return Record{Model: "gpt-fixture", UsageFinal: true, Tokens: pricedObservation().Tokens, ProviderCost: &Money{Amount: 12, Currency: "USD", Source: "provider", Version: 1}}
	}
	cases := []struct {
		name   string
		alter  func(*Record, *PriceBook)
		amount *float64
		reason string
	}{
		{"ordinary", func(*Record, *PriceBook) {}, rate(.000102), "estimated"},
		{"zero", func(r *Record, b *PriceBook) {
			r.Tokens = Tokens{Input: token(0), Output: token(0)}
			b.Entries[0].Rates = Rates{}
		}, rate(0), "estimated"},
		{"unknown model", func(r *Record, b *PriceBook) { r.Model = "unknown" }, nil, "price_unknown"},
		{"partial tokens", func(r *Record, b *PriceBook) { r.Tokens.Output = nil }, nil, "tokens_unknown"},
		{"missing cache", func(r *Record, b *PriceBook) { r.Tokens.CachedInput = nil }, nil, "cache_tokens_unknown"},
		{"required creation unknown", func(r *Record, b *PriceBook) {
			b.Entries[0].CacheCreationMode = "separate"
			b.Entries[0].Rates.CacheCreation = rate(2)
		}, nil, "cache_tokens_unknown"},
		{"missing cached rate", func(r *Record, b *PriceBook) { b.Entries[0].Rates.CachedInput = nil }, nil, "rate_unknown"},
		{"nonfinal", func(r *Record, b *PriceBook) { r.UsageFinal = false }, nil, "usage_not_final"},
		{"inconsistent", func(r *Record, b *PriceBook) { r.Tokens.CachedInput = token(101) }, nil, "tokens_inconsistent"},
		{"tier unknown", func(r *Record, b *PriceBook) { b.Entries[0].Tier = "priority" }, nil, "price_unknown"},
		{"exact priority", func(r *Record, b *PriceBook) {
			r.ServiceTier = "priority"
			p := priceFixture()
			p.Tier = "priority"
			p.Rates.Output = rate(4)
			b.Entries = append(b.Entries, p)
		}, rate(.000122), "estimated"},
		{"long", func(r *Record, b *PriceBook) {
			b.Entries[0].LongContextThreshold = token(99)
			rates := Rates{Input: rate(2), CachedInput: rate(.2), Output: rate(4)}
			b.Entries[0].LongRates = &rates
		}, rate(.000204), "estimated"},
		{"excluding caches", func(r *Record, b *PriceBook) {
			b.Entries[0].InputTokenBasis = "excludes_cache"
			r.Tokens.CacheCreationInput = token(0)
		}, rate(.000122), "estimated"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, b := makeRecord(), cloneBook(book)
			tc.alter(&r, &b)
			priceRecord(&r, b)
			if r.EstimateReason != tc.reason || (r.EstimatedCost == nil) != (tc.amount == nil) {
				t.Fatalf("wrong truth: %+v", r)
			}
			if tc.amount != nil && math.Abs(r.EstimatedCost.Amount-*tc.amount) > 1e-12 {
				t.Fatal(r.EstimatedCost)
			}
			if r.ProviderCost.Amount != 12 || r.ProviderCost.Source != "provider" {
				t.Fatal("provider observation was repriced")
			}
		})
	}
}

func TestPricingPersistsAndHistoricalRepriceIsExplicitAtomicAndVersioned(t *testing.T) {
	l := testLedger(t)
	ctx := context.Background()
	l.Append(pricedObservation())
	flushLedger(t, l)
	first := ledgerSnapshot(t, l).Records[0]
	if first.EstimatedCost != nil || first.EstimateReason != "" {
		t.Fatal(first)
	}
	if err := l.ReplacePricing(ctx, 1, []ModelPrice{priceFixture()}); err != nil {
		t.Fatal(err)
	}
	if ledgerSnapshot(t, l).Records[0].EstimatedCost != nil {
		t.Fatal("saving pricebook silently repriced history")
	}
	if err := l.Reprice(ctx, 1); !errors.Is(err, ErrRevision) {
		t.Fatal("stale editor repriced", err)
	}
	if err := l.Reprice(ctx, 2); err != nil {
		t.Fatal(err)
	}
	row := ledgerSnapshot(t, l).Records[0]
	if row.EstimatedCost == nil || row.EstimatedCost.Version != 2 {
		t.Fatal(row)
	}
	raw, _ := os.ReadFile(l.path)
	oldBook, _ := l.Pricing()
	setLedgerWriter(l, func(diskState) error { return errors.New("synthetic disk failure") })
	altered := priceFixture()
	altered.Rates.Input = rate(3)
	if l.ReplacePricing(ctx, 2, []ModelPrice{altered}) == nil {
		t.Fatal("save falsely succeeded")
	}
	after, _ := l.Pricing()
	if !reflect.DeepEqual(oldBook, after) {
		t.Fatal("failed price save changed live version")
	}
	bytes, _ := os.ReadFile(l.path)
	if string(raw) != string(bytes) {
		t.Fatal("failed price save changed durable data")
	}
	setLedgerWriter(l, l.storage.write)
	flushLedger(t, l)
	account := l.AnonymousAccountID("synthetic-id")
	if l.Clear(ctx) != nil {
		t.Fatal("clear failed")
	}
	book, _ := l.Pricing()
	if book.Version != 2 || len(book.Entries) != 1 || l.AnonymousAccountID("synthetic-id") != account {
		t.Fatal("clear discarded settings or salt")
	}
	if l.Close(ctx) != nil {
		t.Fatal("close failed")
	}
	reopened := Open(l.path)
	defer reopened.Close(ctx)
	saved, _ := reopened.Pricing()
	if !reflect.DeepEqual(book, saved) {
		t.Fatal("pricebook was not durable")
	}
}

func TestCreationPricingRequiresExplicitModeAndKeepsUnknownCounters(t *testing.T) {
	l := testLedger(t)
	ctx := context.Background()
	p := priceFixture()
	p.CacheCreationMode = ""
	if !errors.Is(l.ReplacePricing(ctx, 1, []ModelPrice{p}), ErrPricing) {
		t.Fatal("missing creation pricing policy was accepted")
	}
	p = priceFixture()
	p.CacheCreationMode = "separate"
	p.Rates.CacheCreation = rate(0)
	if err := l.ReplacePricing(ctx, 1, []ModelPrice{p}); err != nil {
		t.Fatal(err)
	}
	o := pricedObservation()
	o.ProviderCost = &Money{Amount: 0, Currency: "USD"}
	l.Append(o)
	flushLedger(t, l)
	r := ledgerSnapshot(t, l).Records[0]
	if r.EstimatedCost != nil || r.Tokens.CacheCreationInput != nil || r.ProviderCost == nil || r.ProviderCost.Amount != 0 {
		t.Fatal("missing counter was turned into zero or estimate", r)
	}
	p.CacheCreationMode = "included_input"
	if l.ReplacePricing(ctx, 2, []ModelPrice{p}) != nil || l.Reprice(ctx, 3) != nil {
		t.Fatal("explicit included input reprice failed")
	}
	r = ledgerSnapshot(t, l).Records[0]
	if r.EstimatedCost == nil || r.EstimatedCost.Version != 3 || r.Tokens.CacheCreationInput != nil || r.ProviderCost == nil || r.ProviderCost.Amount != 0 || r.ProviderCost.Source != "provider" {
		t.Fatal("explicit pricing rewrote true provider data or unknown token", r)
	}
}

func TestSQLiteTransactionFailureKeepsDurableSettingsAndRecords(t *testing.T) {
	l := testLedger(t)
	ctx := context.Background()
	l.Append(pricedObservation())
	flushLedger(t, l)
	before := readQuery(t, l, QueryFilter{})
	book, _ := l.Pricing()
	l.storage.mu.Lock()
	_, err := l.storage.db.Exec(`CREATE TRIGGER synthetic_reject_meta BEFORE UPDATE ON ledger_meta BEGIN SELECT RAISE(ABORT, 'synthetic failure'); END;`)
	l.storage.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if l.ReplacePricing(ctx, 1, []ModelPrice{priceFixture()}) == nil || l.Clear(ctx) == nil {
		t.Fatal("SQLite rejected transaction reported success")
	}
	current, _ := l.Pricing()
	after := readQuery(t, l, QueryFilter{})
	if !reflect.DeepEqual(book, current) || after.Total != before.Total || after.Records[0].ID != before.Records[0].ID || !after.Status.Unsaved || after.Status.SaveError == "" {
		t.Fatal("failed transaction changed live or durable data", after)
	}
	l.storage.mu.Lock()
	_, err = l.storage.db.Exec("DROP TRIGGER synthetic_reject_meta")
	l.storage.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	flushLedger(t, l)
}

func TestQueryFiltersPagingAndUnknownAggregates(t *testing.T) {
	l := testLedger(t)
	for i := 0; i < 3; i++ {
		o := observedFixture()
		o.StartedAt = time.Now().Add(time.Duration(i-3) * time.Minute)
		o.DurationMS = int64(3 - i)
		if i == 0 {
			o.Tokens.Input = token(0)
			o.ProviderCost = &Money{Amount: 0, Currency: "USD"}
		}
		if i == 1 {
			o.Model = "gpt_other"
			o.State = "failed"
			o.HTTPStatus = 429
			o.Tokens.Input = token(7)
			o.ProviderCost = &Money{Amount: 2, Currency: "EUR"}
		}
		l.Append(o)
	}
	flushLedger(t, l)
	all := readQuery(t, l, QueryFilter{Limit: 1})
	if all.Total != 3 || len(all.Records) != 1 || len(all.ByModel) != 2 || len(all.Trend) != 1 || !all.DurableOnly {
		t.Fatal(all)
	}
	summary := all.Summary
	if summary.InputTokens.Sum == nil || *summary.InputTokens.Sum != 7 || summary.InputTokens.KnownRecords != 2 || summary.InputTokens.UnknownRecords != 1 || summary.InputTokens.Complete || summary.TotalTokens.Sum != nil || len(summary.ProviderCosts.Currencies) != 0 || len(summary.EstimatedCosts.Currencies) != 0 {
		t.Fatal(summary)
	}
	page := readQuery(t, l, QueryFilter{Limit: 1, Offset: 1, Revision: all.Revision})
	if page.Records[0].ID == all.Records[0].ID || page.Revision != all.Revision {
		t.Fatal("unstable paging")
	}
	failed := readQuery(t, l, QueryFilter{State: "failed", HTTPStatus: func() *int { v := 429; return &v }(), Sort: "duration", Direction: "asc"})
	if failed.Total != 1 || failed.Records[0].Model != "gpt_other" {
		t.Fatal(failed)
	}
	literal := readQuery(t, l, QueryFilter{ModelContains: "gpt_"})
	if literal.Total != 1 {
		t.Fatal("LIKE wildcard not escaped", literal.Total)
	}
	byAccount := readQuery(t, l, QueryFilter{AccountID: all.Records[0].AccountID})
	if byAccount.Total != 3 {
		t.Fatal("anonymous account filter failed")
	}
	for _, f := range []QueryFilter{{Offset: 1}, {Sort: "time; DELETE FROM request_records"}, {Limit: 201}, {Model: "sk-secret"}, {AccountID: "raw-account"}, {ModelContains: "%' OR 1=1"}} {
		if _, err := l.Query(context.Background(), f); !errors.Is(err, ErrQuery) {
			t.Fatal("unsafe query accepted", f, err)
		}
	}
	if err := l.Clear(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Query(context.Background(), QueryFilter{Revision: all.Revision}); !errors.Is(err, ErrRevision) {
		t.Fatal("retired query revision returned rows", err)
	}
}

func TestQueryTracksDurableCommitAndProcessEpoch(t *testing.T) {
	l := testLedger(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	setLedgerWriter(l, func(state diskState) error {
		once.Do(func() { close(entered); <-release })
		return l.storage.write(state)
	})
	l.Append(observedFixture())
	<-entered
	before := readQuery(t, l, QueryFilter{})
	if before.Total != 0 || !before.Status.Unsaved || before.Status.Count != 1 {
		t.Fatal("durable and unsaved states conflated", before)
	}
	close(release)
	flushLedger(t, l)
	if _, err := l.Query(context.Background(), QueryFilter{Revision: before.Revision}); !errors.Is(err, ErrRevision) {
		t.Fatal("commit revived old page", err)
	}
	after := readQuery(t, l, QueryFilter{})
	if after.Total != 1 {
		t.Fatal(after)
	}
	l.Close(context.Background())
	reopened := Open(l.path)
	defer reopened.Close(context.Background())
	if _, err := reopened.Query(context.Background(), QueryFilter{Revision: after.Revision}); !errors.Is(err, ErrRevision) {
		t.Fatal("old process cursor accepted", err)
	}
}

func TestSQLiteRejectsCorruptionRatherThanReturningEmptySuccess(t *testing.T) {
	l := testLedger(t)
	l.Append(observedFixture())
	flushLedger(t, l)
	l.storage.mu.Lock()
	_, err := l.storage.db.Exec("UPDATE request_records SET payload=?", `{"id":"hidden","prompt":"synthetic-secret"}`)
	l.storage.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = l.Query(context.Background(), QueryFilter{}); !errors.Is(err, ErrRead) {
		t.Fatal("corrupt database returned successful rows", err)
	}
	path := l.path
	l.Close(context.Background())
	raw, _ := os.ReadFile(path)
	reopened := Open(path)
	defer reopened.Close(context.Background())
	if ledgerSnapshot(t, reopened).ReadError == "" {
		t.Fatal("corrupt payload accepted")
	}
	current, _ := os.ReadFile(path)
	if string(raw) != string(current) {
		t.Fatal("open overwrote unreadable database")
	}
	if reopened.Clear(context.Background()) != nil {
		t.Fatal("explicit SQLite recovery failed")
	}
}

func TestSQLiteCreatesMissingParentsAndNeverLeaksRawIdentifiers(t *testing.T) {
	l := Open(filepath.Join(t.TempDir(), "new", "nested", "ledger.json"))
	defer l.Close(context.Background())
	l.Append(observedFixture())
	flushLedger(t, l)
	raw, err := os.ReadFile(l.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, rawID := range []string{"raw-fixture-account", "raw-fixture-client", "codex/v1/responses"} {
		if strings.Contains(string(raw), rawID) {
			t.Fatal("raw identity leaked")
		}
	}
}
