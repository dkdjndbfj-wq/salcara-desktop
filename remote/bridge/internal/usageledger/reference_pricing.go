package usageledger

// Independently transcribed factual rates, fetched 2026-10-13. No Cockpit
// implementation is copied. This is a versioned, offline reference: provider
// markups, tools, residency uplifts and subscription balances are NOT included.
const OfficialReferenceVersion uint64 = 20261013
const OfficialReferenceURL = "https://developers.openai.com/api/docs/pricing"

type ReferencePriceBook struct {
	Version    uint64       `json:"version"`
	Source     string       `json:"source"`
	URL        string       `json:"url"`
	VerifiedAt string       `json:"verifiedAt"`
	Entries    []ModelPrice `json:"entries"`
}

func OfficialReferencePricing() *ReferencePriceBook {
	return &ReferencePriceBook{OfficialReferenceVersion, "official_reference", OfficialReferenceURL, "2026-10-13", cloneBook(PriceBook{Entries: officialPrices}).Entries}
}

// EstimateReferenceCost is the pure, explicitly reference-only helper for a
// request-time billing adapter. A caller must retain its own multiplier snapshot
// separately and must never apply it again to an upstream actual-cost field.
func EstimateReferenceCost(model, tier string, tokens Tokens) (*Money, string) {
	r := Record{Model: model, ServiceTier: tier, Tokens: sanitizeTokens(tokens), UsageFinal: true}
	priceRecord(&r, PriceBook{Version: 1})
	return cloneMoney(r.EstimatedCost), r.EstimateReason
}

var officialPrices = buildOfficialPrices()

func buildOfficialPrices() []ModelPrice {
	var out []ModelPrice
	rate := func(v float64) *float64 {
		if v < 0 {
			return nil
		}
		return &v
	}
	add := func(model string, input, cached, created, output float64, long bool, tier string) {
		p := ModelPrice{Model: model, Tier: tier, InputTokenBasis: "includes_cache", CacheCreationMode: "included_input", Rates: Rates{rate(input), rate(cached), rate(created), rate(output)}}
		if created >= 0 {
			p.CacheCreationMode = "separate"
		}
		if long {
			n := int64(272000)
			p.LongContextThreshold = &n
			p.LongRates = &Rates{rate(input * 2), rate(cached * 2), rate(created * 2), rate(output * 1.5)}
		}
		out = append(out, p)
	}
	// Exact identifiers from the official pricing page. New cache-write billing
	// requires an explicit creation counter; absence remains unknown, not free.
	for _, p := range []struct {
		model                 string
		input, cached, output float64
	}{
		{"gpt-6-astra", 10, 1, 50}, {"gpt-6.1-sol", 2, .1, 10}, {"gpt-6-luna", .1, .01, .5},
		{"gpt-6-sol", 2, .2, 10}, {"gpt-5.6-sol", 4, .4, 20}, {"gpt-5.6-terra", 2, .2, 12}, {"gpt-5.6-luna", .2, .02, 1.2},
	} {
		for _, tier := range []struct {
			name   string
			factor float64
		}{{"standard", 1}, {"flex", .5}, {"fast", 2}, {"priority", 2}} {
			add(p.model, p.input*tier.factor, p.cached*tier.factor, p.input*1.25*tier.factor, p.output*tier.factor, true, tier.name)
		}
		if p.model == "gpt-6-astra" || p.model == "gpt-6.1-sol" {
			add(p.model, p.input*6, p.cached*6, p.input*7.5, p.output*6, true, "ultrafast")
		}
	}
	for _, p := range []struct {
		model                 string
		input, cached, output float64
		long                  bool
	}{
		{"gpt-5.5", 5, .5, 30, true}, {"gpt-5.5-pro", 30, -1, 180, true}, {"gpt-5.4", 2.5, .25, 15, true}, {"gpt-5.4-pro", 30, -1, 180, true},
		{"gpt-5.4-mini", .75, .075, 4.5, false}, {"gpt-5.4-nano", .2, .02, 1.25, false},
		{"gpt-5.2", 1.75, .175, 14, false}, {"gpt-5.2-pro", 21, -1, 168, false}, {"gpt-5.1", 1.25, .125, 10, false}, {"gpt-5", 1.25, .125, 10, false},
		{"gpt-5-mini", .25, .025, 2, false}, {"gpt-5-nano", .05, .005, .4, false},
		{"gpt-5.3-codex", 1.75, .175, 14, false}, {"gpt-5.2-codex", 1.75, .175, 14, false}, {"gpt-5.1-codex", 1.25, .125, 10, false},
		{"gpt-4.1", 2, .5, 8, false}, {"gpt-4.1-mini", .4, .1, 1.6, false},
	} {
		add(p.model, p.input, p.cached, -1, p.output, p.long, "standard")
	}
	// Only explicitly published Fast rates; do not invent unsupported tiers.
	for _, p := range []struct {
		model                 string
		input, cached, output float64
	}{
		{"gpt-5.5", 12.5, 1.25, 75}, {"gpt-5.4", 5, .5, 30}, {"gpt-5.4-mini", 1.5, .15, 9},
		{"gpt-5.2", 3.5, .35, 28}, {"gpt-5.1", 2.5, .25, 20}, {"gpt-5", 2.5, .25, 20}, {"gpt-5-mini", .45, .045, 3.6}, {"gpt-5.3-codex", 3.5, .35, 28},
	} {
		add(p.model, p.input, p.cached, -1, p.output, false, "fast")
		add(p.model, p.input, p.cached, -1, p.output, false, "priority")
	}
	return out
}

func officialReferencePrice(model, tier string) *ModelPrice {
	// Empty/default is the normal API service tier. "auto" is a request choice,
	// not a measured billing tier; it must be resolved by the upstream response.
	if tier == "" || tier == "default" {
		tier = "standard"
	}
	for i := range officialPrices {
		if officialPrices[i].Model == model && officialPrices[i].Tier == tier {
			p := officialPrices[i]
			if (tier == "fast" || tier == "priority") && (model == "gpt-5.4" || model == "gpt-5.5") {
				// The published Fast table has no long-context rate for these IDs.
				n := int64(272000)
				p.LongContextThreshold = &n
				p.LongRates = &Rates{}
			}
			return &p
		}
	}
	return nil
}

func validEstimateMoney(v *Money) bool {
	return v == nil || (v.Source == "user_pricebook" || v.Source == "official_reference") && validMoney(v, v.Source)
}
