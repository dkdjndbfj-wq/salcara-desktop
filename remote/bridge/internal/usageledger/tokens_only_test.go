package usageledger

import (
	"context"
	"testing"
)

func TestTokenOnlyRecordingAndQueryKeepHistoricalFieldsWithoutNewEstimates(t *testing.T) {
	l := testLedger(t)
	o := observedFixture()
	o.Model, o.UsageFinal = "gpt-5.3-codex", true
	o.Tokens = Tokens{Input: refInt(1000), Output: refInt(100), CachedInput: refInt(200)}
	l.Append(o)
	flushLedger(t, l)
	// Seed a legacy priced record explicitly. New appends must stay Token-only
	// even with a known model and an already-used reference pricebook.
	if err := l.Reprice(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	l.Append(o)
	flushLedger(t, l)
	result, err := l.Query(context.Background(), QueryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 2 || result.Summary.InputTokens.Sum == nil || *result.Summary.InputTokens.Sum != 2000 || result.Summary.TotalTokens.Sum != nil {
		t.Fatal("Token counters changed or missing total became known", result.Summary)
	}
	priced := 0
	for _, record := range result.Records {
		if record.EstimatedCost != nil {
			priced++
		}
	}
	if priced != 1 {
		t.Fatal("new request was priced or historical value was erased", priced)
	}
	for _, summary := range []Summary{result.Summary, result.ByModel[0].Summary, result.ByAccount[0].Summary, result.ByRoute[0].Summary, result.Trend[0].Summary} {
		if len(summary.EstimatedCosts.Currencies) != 0 || len(summary.ProviderCosts.Currencies) != 0 {
			t.Fatal("ordinary Token query still computes money", summary)
		}
	}
}
