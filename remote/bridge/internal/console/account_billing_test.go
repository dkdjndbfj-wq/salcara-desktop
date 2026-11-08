package console

import (
	"encoding/json"
	"salcara/bridge/internal/relay"
	"testing"
	"time"
)

func TestAccountActualCostNeverMultipliedOrConfusedWithWallet(t *testing.T) {
	now := time.Now()
	for _, mode := range []string{`"mode":"quota_limited","quota":{"remaining":20}`, `"mode":"unrestricted","subscription":{"daily_limit_usd":20}`, `"planName":"Subscription"`} {
		var raw map[string]any
		if err := json.Unmarshal([]byte(`{`+mode+`,"remaining":20,"unit":"USD","usage":{"today":{"actual_cost":1.5,"cost":15,"requests":3,"total_tokens":20}}}`), &raw); err != nil {
			t.Fatal(err)
		}
		raw["_salcara_billing"] = &relay.KeyBilling{Source: "sub2api.key_billing", Scope: "token", EffectiveMultiplier: .1, FreshUntil: now.Add(time.Minute).UnixMilli()}
		got := normalizeAccountUsage(raw, "id", "rev", now)
		if got.Balance != nil || got.Today == nil || got.Today.ActualCost == nil || *got.Today.ActualCost.Amount != 1.5 || got.Today.ActualCost.Currency != "USD" || got.Billing == nil {
			t.Fatalf("wrong charge projection: %+v", got)
		}
	}
}

func TestAccountChargeUnknownIsNotCostTimesCurrentRate(t *testing.T) {
	for _, fields := range []string{`"cost":20`, `"actual_cost":null`, `"actual_cost":-1`, `"actual_cost":1,"actual_cost_usd":2`} {
		var raw map[string]any
		json.Unmarshal([]byte(`{"unit":"USD","usage":{"today":{`+fields+`}}}`), &raw)
		raw["_salcara_billing"] = map[string]any{"effectiveMultiplier": .1}
		got := normalizeAccountUsage(raw, "id", "rev", time.Now())
		if got.Today == nil || got.Today.ActualCost != nil || got.Billing != nil {
			t.Fatal("invented charge or trusted raw billing", got)
		}
	}
}
