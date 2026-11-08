package usageledger

import "testing"

func TestCollectorOnlyObservesExplicitCurrencyAndUpstreamTier(t *testing.T) {
	for _, raw := range []string{`{"cost":3}`, `{"cost_usd":null}`, `{"cost":{"amount":3}}`, `{"cost":{"amount":"3","currency":"USD"}}`, `{"cost_usd":1,"actual_cost_usd":2}`, `{"cost_usd":-1}`, `{"cost":{"amount":3,"currency":"https://secret"}}`} {
		var c Collector
		c.ObserveJSON([]byte(raw))
		if c.ProviderCost() != nil {
			t.Fatal("ambiguous money became observed", raw, c.ProviderCost())
		}
	}
	var c Collector
	c.ObserveEvent([]byte(`{"type":"response.completed","response":{"status":"completed","service_tier":"priority","usage":{"input_tokens":5,"cost_usd":0}}}`))
	if c.ProviderCost() == nil || c.ProviderCost().Amount != 0 || c.ServiceTier() != "priority" {
		t.Fatal("explicit zero or tier lost")
	}
	c.ProviderCost().Amount = 99
	if c.ProviderCost().Amount != 0 {
		t.Fatal("getter aliases live cost")
	}
	var image Collector
	image.ObserveImageJSON([]byte(`{"data":[{"b64_json":"fixture"}],"cost":{"amount":0.5,"currency":"eur"}}`))
	if image.ProviderCost() == nil || image.ProviderCost().Currency != "EUR" || image.ProviderCost().Amount != .5 {
		t.Fatal("image cost observation lost")
	}
	var conflict Collector
	conflict.ObserveJSON([]byte(`{"cost_usd":1}`))
	conflict.ObserveJSON([]byte(`{"cost_usd":2}`))
	if conflict.ProviderCost() != nil {
		t.Fatal("conflicting upstream fee was treated as a charge")
	}
}
