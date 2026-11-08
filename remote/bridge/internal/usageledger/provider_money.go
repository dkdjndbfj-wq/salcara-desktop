package usageledger

import "encoding/json"

// Only explicit upstream currency-bearing amounts are observed. An unlabeled
// numeric "cost", provider prices or request metadata cannot manufacture money.
// This is an upstream observation, not proof of a charge or account balance.
func (c *Collector) metadata(v map[string]json.RawMessage) {
	if tier := textField(v, "service_tier"); tier != "" && validTier(tier) {
		c.serviceTier = tier
	}
	var selected *Money
	ambiguous := false
	accept := func(m *Money) {
		if m == nil {
			return
		}
		if selected != nil && (selected.Amount != m.Amount || selected.Currency != m.Currency) {
			ambiguous = true
		}
		selected = m
	}
	for _, key := range []string{"actual_cost_usd", "cost_usd"} {
		var amount float64
		if raw, ok := v[key]; ok && string(raw) != "null" && json.Unmarshal(raw, &amount) == nil {
			accept(providerMoney(&Money{Amount: amount, Currency: "USD"}))
		}
	}
	cost := object(v["cost"])
	if cost != nil {
		var amount float64
		raw, hasAmount := cost["amount"]
		currency := normalizeCurrency(textField(cost, "currency"))
		if hasAmount && string(raw) != "null" && json.Unmarshal(raw, &amount) == nil {
			accept(providerMoney(&Money{Amount: amount, Currency: currency}))
		}
	}
	if selected != nil && !ambiguous {
		if c.providerCost != nil && (c.providerCost.Amount != selected.Amount || c.providerCost.Currency != selected.Currency) {
			// Two upstream envelopes disagree; retaining either would be a
			// fabricated billing claim, so make the observation unknown.
			c.providerCost = nil
			return
		}
		c.providerCost = selected
	}
}
