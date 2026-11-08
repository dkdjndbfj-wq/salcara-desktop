package usageledger

import (
	"context"
	"errors"
	"math"
	"strings"
)

var ErrPricing = errors.New("模型价表无效")
var ErrRevision = errors.New("本机记录或价表已更新，请重新读取")

const maxPriceVersion = 1<<53 - 1

// Money never conflates provider observations with user-authored estimates.
// Version is 1 for the observed-money schema, or the user price-book version.
type Money struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
	Source   string  `json:"source"`
	Version  uint64  `json:"version"`
}

type Rates struct {
	Input         *float64 `json:"inputUsdPerMillion"`
	CachedInput   *float64 `json:"cachedInputUsdPerMillion"`
	CacheCreation *float64 `json:"cacheCreationUsdPerMillion"`
	Output        *float64 `json:"outputUsdPerMillion"`
}
type ModelPrice struct {
	Model                string `json:"model"`
	Tier                 string `json:"tier"`
	InputTokenBasis      string `json:"inputTokenBasis"`
	CacheCreationMode    string `json:"cacheCreationMode"`
	Rates                Rates  `json:"rates"`
	LongContextThreshold *int64 `json:"longContextThreshold"`
	LongRates            *Rates `json:"longRates"`
}
type PriceBook struct {
	Version   uint64              `json:"version"`
	UpdatedAt int64               `json:"updatedAt"`
	Entries   []ModelPrice        `json:"entries"`
	Reference *ReferencePriceBook `json:"reference,omitempty"`
}

func validTier(tier string) bool {
	return tier == "" || tier == "standard" || tier == "default" || tier == "priority" || tier == "fast" || tier == "ultrafast" || tier == "flex" || tier == "auto"
}
func validMoney(v *Money, source string) bool {
	if v == nil {
		return true
	}
	if v.Source != source || v.Version == 0 || v.Version > maxPriceVersion || source == "provider" && v.Version != 1 || math.IsNaN(v.Amount) || math.IsInf(v.Amount, 0) || v.Amount < 0 || v.Amount > 1e12 || len(v.Currency) != 3 {
		return false
	}
	for _, r := range v.Currency {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}
func cloneMoney(v *Money) *Money {
	if v == nil {
		return nil
	}
	copy := *v
	return &copy
}
func providerMoney(v *Money) *Money {
	if v == nil {
		return nil
	}
	copy := *v
	copy.Source, copy.Version = "provider", 1
	if !validMoney(&copy, "provider") {
		return nil
	}
	return &copy
}
func copyRate(v *float64) *float64 {
	if v == nil {
		return nil
	}
	copy := *v
	return &copy
}
func cloneRates(v Rates) Rates {
	return Rates{copyRate(v.Input), copyRate(v.CachedInput), copyRate(v.CacheCreation), copyRate(v.Output)}
}
func cloneBook(v PriceBook) PriceBook {
	v.Entries = append([]ModelPrice{}, v.Entries...)
	for i := range v.Entries {
		p := &v.Entries[i]
		p.Rates = cloneRates(p.Rates)
		if p.LongContextThreshold != nil {
			n := *p.LongContextThreshold
			p.LongContextThreshold = &n
		}
		if p.LongRates != nil {
			rates := cloneRates(*p.LongRates)
			p.LongRates = &rates
		}
	}
	return v
}
func validRates(v Rates) bool {
	for _, rate := range []*float64{v.Input, v.CachedInput, v.CacheCreation, v.Output} {
		if rate != nil && (math.IsNaN(*rate) || math.IsInf(*rate, 0) || *rate < 0 || *rate > 1e6) {
			return false
		}
	}
	return true
}
func validatePrices(entries []ModelPrice) error {
	if len(entries) > 512 {
		return ErrPricing
	}
	seen := map[string]bool{}
	for _, p := range entries {
		key := p.Model + "\x00" + p.Tier
		if !ValidateModel(p.Model) || !validTier(p.Tier) || seen[key] || p.InputTokenBasis != "includes_cache" && p.InputTokenBasis != "excludes_cache" || p.CacheCreationMode != "included_input" && p.CacheCreationMode != "separate" || !validRates(p.Rates) {
			return ErrPricing
		}
		seen[key] = true
		if (p.LongContextThreshold == nil) != (p.LongRates == nil) {
			return ErrPricing
		}
		if p.LongRates != nil && (!validRates(*p.LongRates) || *p.LongContextThreshold < 1 || *p.LongContextThreshold > 1e12) {
			return ErrPricing
		}
	}
	return nil
}

func (l *Ledger) Pricing() (PriceBook, error) {
	if l == nil {
		return PriceBook{}, ErrSave
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.blockedRead {
		return PriceBook{}, ErrRead
	}
	book := cloneBook(l.prices)
	book.Reference = OfficialReferencePricing()
	return book, nil
}

// ReplacePricing does not silently rewrite historical estimates. A caller must
// request Reprice explicitly; provider observations are never rewritten.
func (l *Ledger) ReplacePricing(ctx context.Context, expected uint64, entries []ModelPrice) error {
	if expected == 0 || expected >= maxPriceVersion {
		return ErrPricing
	}
	if err := validatePrices(entries); err != nil {
		return err
	}
	book := cloneBook(PriceBook{Entries: entries})
	return l.request(ctx, command{kind: "pricing", prices: book, expectedVersion: expected})
}
func (l *Ledger) Reprice(ctx context.Context, expected uint64) error {
	if expected == 0 || expected > maxPriceVersion {
		return ErrPricing
	}
	return l.request(ctx, command{kind: "reprice", expectedVersion: expected})
}

func priceRecord(r *Record, book PriceBook) {
	r.EstimatedCost, r.PricingVersion, r.EstimateReason = nil, book.Version, "price_unknown"
	if !r.UsageFinal {
		r.EstimateReason = "usage_not_final"
		return
	}
	var chosen *ModelPrice
	source, version := "user_pricebook", book.Version
	for i := range book.Entries {
		p := &book.Entries[i]
		if p.Model == r.Model && (p.Tier == "" || p.Tier == r.ServiceTier) {
			if chosen == nil || p.Tier != "" {
				chosen = p
			}
		}
	}
	if chosen == nil {
		// Published OpenAI rates are an API-equivalent reference, not the
		// provider's bill. No prefix/date/other-model fallback is allowed.
		chosen = officialReferencePrice(r.Model, r.ServiceTier)
		if chosen == nil {
			return
		}
		source, version = "official_reference", OfficialReferenceVersion
		r.PricingVersion = version
	}
	t := r.Tokens
	if t.Input == nil || t.Output == nil {
		r.EstimateReason = "tokens_unknown"
		return
	}
	rates := chosen.Rates
	if chosen.LongContextThreshold != nil && *t.Input > *chosen.LongContextThreshold {
		rates = *chosen.LongRates
	}
	// Without cache counters an input total cannot be split into pricing classes.
	// An explicit input zero proves that its included cache counters are zero.
	cached, created := int64(0), int64(0)
	if t.CachedInput != nil {
		cached = *t.CachedInput
	} else if *t.Input != 0 || chosen.InputTokenBasis == "excludes_cache" {
		r.EstimateReason = "cache_tokens_unknown"
		return
	}
	// The user explicitly chooses whether creation is included in ordinary
	// input pricing. Missing creation counters are never converted to zero.
	if chosen.CacheCreationMode == "separate" {
		if t.CacheCreationInput == nil && (*t.Input != 0 || chosen.InputTokenBasis == "excludes_cache") {
			r.EstimateReason = "cache_tokens_unknown"
			return
		}
		if t.CacheCreationInput != nil {
			created = *t.CacheCreationInput
		}
	}
	input := *t.Input
	if chosen.InputTokenBasis == "includes_cache" {
		if cached+created > input {
			r.EstimateReason = "tokens_inconsistent"
			return
		}
		input -= cached + created
	}
	cost := float64(0)
	for _, component := range []struct {
		tokens int64
		rate   *float64
	}{{input, rates.Input}, {cached, rates.CachedInput}, {created, rates.CacheCreation}, {*t.Output, rates.Output}} {
		if component.tokens == 0 {
			continue
		}
		if component.rate == nil {
			r.EstimateReason = "rate_unknown"
			return
		}
		cost += float64(component.tokens) * *component.rate / 1e6
	}
	if math.IsNaN(cost) || math.IsInf(cost, 0) || cost > 1e12 {
		r.EstimateReason = "amount_out_of_range"
		return
	}
	r.EstimatedCost = &Money{Amount: cost, Currency: "USD", Source: source, Version: version}
	r.EstimateReason = "estimated"
}

func validEstimateReason(reason string) bool {
	switch reason {
	case "", "price_unknown", "usage_not_final", "tokens_unknown", "cache_tokens_unknown", "tokens_inconsistent", "rate_unknown", "amount_out_of_range", "estimated":
		return true
	}
	return false
}

// Used only by the provider collector: generic unlabeled cost is deliberately
// excluded because its unit and meaning are ambiguous.
func normalizeCurrency(v string) string { return strings.ToUpper(strings.TrimSpace(v)) }
