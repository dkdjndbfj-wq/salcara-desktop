package usageledger

import (
	"context"
	"math"
	"testing"
)

func refInt(v int64) *int64       { return &v }
func refFloat(v float64) *float64 { return &v }

func TestOfficialReferencePricingColdStartExactModelAndTier(t *testing.T) {
	for _, tc := range []struct {
		model, tier string
		want        *float64
	}{
		{"gpt-5.3-codex", "default", refFloat(.002835)},
		{"gpt-5.3-codex", "fast", refFloat(.00567)},
		{"gpt-5.3-codex", "priority", refFloat(.00567)},
		{"gpt-5.3-codex", "auto", nil}, {"gpt-5.3-codex", "ultrafast", nil},
		{"gpt-5.3-codex-synthetic", "default", nil}, {"provider/gpt-5.3-codex", "default", nil},
	} {
		t.Run(tc.model+tc.tier, func(t *testing.T) {
			r := Record{Model: tc.model, ServiceTier: tc.tier, UsageFinal: true, Tokens: Tokens{Input: refInt(1000), CachedInput: refInt(200), Output: refInt(100)}}
			priceRecord(&r, PriceBook{Version: 1})
			if tc.want == nil {
				if r.EstimatedCost != nil {
					t.Fatal("invented rate", r)
				}
				return
			}
			if r.EstimatedCost == nil || math.Abs(r.EstimatedCost.Amount-*tc.want) > 1e-12 || r.EstimatedCost.Source != "official_reference" || r.PricingVersion != OfficialReferenceVersion {
				t.Fatalf("%+v", r)
			}
		})
	}
}

func TestOfficialReferenceCacheWritesAndLongContext(t *testing.T) {
	var c Collector
	c.ObserveJSON([]byte(`{"status":"completed","usage":{"input_tokens":1000,"output_tokens":100,"input_tokens_details":{"cached_tokens":200,"cache_write_tokens":300}}}`))
	r := Record{Model: "gpt-6.1-sol", UsageFinal: c.Final(), Tokens: c.Tokens(), ServiceTier: "standard"}
	priceRecord(&r, PriceBook{Version: 1})
	if r.EstimatedCost == nil || math.Abs(r.EstimatedCost.Amount-.00277) > 1e-12 {
		t.Fatal(r)
	}
	r.Tokens.CacheCreationInput = nil
	priceRecord(&r, PriceBook{Version: 1})
	if r.EstimatedCost != nil || r.EstimateReason != "cache_tokens_unknown" {
		t.Fatal("missing cache writes treated as zero", r)
	}
	r.Model = "gpt-5.4"
	r.Tokens = Tokens{Input: refInt(300000), CachedInput: refInt(0), Output: refInt(1000)}
	priceRecord(&r, PriceBook{Version: 1})
	if r.EstimatedCost == nil || math.Abs(r.EstimatedCost.Amount-1.5225) > 1e-12 {
		t.Fatal(r)
	}
	r.ServiceTier = "fast"
	priceRecord(&r, PriceBook{Version: 1})
	if r.EstimatedCost != nil {
		t.Fatal("invented long fast rates", r)
	}
}

func TestUserPricingOverridesReferenceIncludingBlankAndZero(t *testing.T) {
	r := Record{Model: "gpt-5.3-codex", UsageFinal: true, Tokens: Tokens{Input: refInt(1000), CachedInput: refInt(200), Output: refInt(100)}}
	p := ModelPrice{Model: r.Model, InputTokenBasis: "includes_cache", CacheCreationMode: "included_input"}
	book := PriceBook{Version: 7, Entries: []ModelPrice{p}}
	priceRecord(&r, book)
	if r.EstimatedCost != nil || r.EstimateReason != "rate_unknown" {
		t.Fatal("blank override lost", r)
	}
	book.Entries[0].Rates = Rates{Input: refFloat(0), CachedInput: refFloat(0), Output: refFloat(0)}
	priceRecord(&r, book)
	if r.EstimatedCost == nil || r.EstimatedCost.Amount != 0 || r.EstimatedCost.Source != "user_pricebook" || r.EstimatedCost.Version != 7 {
		t.Fatal(r)
	}
	book.Entries[0].Tier = "priority"
	priceRecord(&r, book)
	if r.EstimatedCost == nil || r.EstimatedCost.Source != "official_reference" {
		t.Fatal("unrelated tier incorrectly shadows default", r)
	}
}

func TestOfficialReferenceSurvivesLedgerPersistenceAndManualReprice(t *testing.T) {
	l := testLedger(t)
	o := observedFixture()
	o.Model = "gpt-5.3-codex"
	o.UsageFinal = true
	o.Tokens = Tokens{Input: refInt(1000), CachedInput: refInt(200), Output: refInt(100)}
	l.Append(o)
	flushLedger(t, l)
	if row := ledgerSnapshot(t, l).Records[0]; row.EstimatedCost != nil || row.PricingVersion != 0 || row.Tokens.Input == nil || *row.Tokens.Input != 1000 {
		t.Fatal("Token recording unexpectedly priced the request", row)
	}
	// An explicit legacy reprice may still populate old records. Loading them
	// after the Token-only change must preserve those historical fields.
	if err := l.Reprice(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	book, err := l.Pricing()
	if err != nil || len(book.Entries) != 0 || book.Reference == nil || book.Reference.Source != "official_reference" || len(book.Reference.Entries) == 0 {
		t.Fatal(book, err)
	}
	*book.Reference.Entries[0].Rates.Input = 999
	if *OfficialReferencePricing().Entries[0].Rates.Input == 999 {
		t.Fatal("mutable reference")
	}
	if err := l.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened := Open(l.path)
	defer reopened.Close(context.Background())
	snapshot, err := reopened.Snapshot(10, "")
	if err != nil || snapshot.ReadError != "" || len(snapshot.Records) != 1 || snapshot.Records[0].EstimatedCost == nil || snapshot.Records[0].EstimatedCost.Source != "official_reference" {
		t.Fatal(snapshot, err)
	}
}

func TestReferenceEstimateQueryDoesNotClaimUserAuthoredRates(t *testing.T) {
	a := Record{EstimatedCost: &Money{Amount: 1, Currency: "USD", Source: "official_reference", Version: OfficialReferenceVersion}}
	got := summarize([]Record{a})
	if got.EstimatedCosts.Source != "official_reference" || got.EstimatedCosts.KnownRecords != 1 {
		t.Fatal(got)
	}
	b := Record{EstimatedCost: &Money{Amount: 2, Currency: "USD", Source: "user_pricebook", Version: 3}}
	got = summarize([]Record{a, b})
	if got.EstimatedCosts.Source != "mixed" || got.EstimatedCosts.Currencies[0].Amount != 3 {
		t.Fatal(got)
	}
}
