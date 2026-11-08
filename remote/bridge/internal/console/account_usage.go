package console

import (
	"context"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/relay"
)

const accountUsageCacheLimit = 256

// Only these sanitized fields cross the card API. A provider may return arbitrary
// fields (including credentials); none of its raw payload or errors is forwarded.
type accountUsageDTO struct {
	ID             string               `json:"id"`
	SourceRevision string               `json:"sourceRevision"`
	ObservedAt     int64                `json:"observedAt"`
	FreshUntil     int64                `json:"freshUntil"`
	Balance        *accountUsageBalance `json:"balance"`
	Today          *accountUsageToday   `json:"today"`
	Billing        *relay.KeyBilling    `json:"billing,omitempty"`
	Group          *accountUsageGroup   `json:"group,omitempty"`
}
type accountUsageBalance struct {
	Amount   *float64 `json:"amount"`
	Currency string   `json:"currency"`
}
type accountUsageToday struct {
	Date       string               `json:"date"`
	Basis      string               `json:"basis"`
	Requests   *int64               `json:"requests"`
	Tokens     *int64               `json:"tokens"`
	ActualCost *accountUsageBalance `json:"actualCost,omitempty"`
}
type accountUsageTarget struct {
	id, revision, sourceEpoch, root, day string
}
type accountUsageEntry struct {
	target accountUsageTarget
	done   chan struct{}
	cancel context.CancelFunc
	data   accountUsageDTO // immutable after done closes
	status int
	at     time.Time
}
type accountUsageCache struct {
	mu      sync.Mutex
	entries map[string]*accountUsageEntry
	slots   chan struct{}
	now     func() time.Time
	fetch   func(context.Context, string, string, int) (map[string]any, error)
}

func newAccountUsageCache() *accountUsageCache {
	return &accountUsageCache{entries: map[string]*accountUsageEntry{}, slots: make(chan struct{}, 4), now: time.Now, fetch: relay.UsageCardDays}
}

func accountUsageSource(c config.Config, id string, now time.Time) (accountUsageTarget, bool) {
	a, ok := c.LocalAccount(id)
	if !ok || a.CredentialRef != "" || a.CredentialEpoch != 0 || strings.TrimSpace(a.Key) == "" {
		return accountUsageTarget{}, false
	}
	switch a.SourceType {
	case "", "api_key", "reverse_proxy":
	default:
		return accountUsageTarget{}, false
	}
	root := config.NormalizeRelayRoot(a.BaseURL)
	u, err := url.Parse(root)
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return accountUsageTarget{}, false
	}
	revision := c.AccountSourceRevision(a)
	if revision == "" { // An unarmed process must not publish a reusable card observation.
		return accountUsageTarget{}, false
	}
	return accountUsageTarget{id: id, revision: revision, sourceEpoch: c.UsageSourceRevision, root: root, day: now.Format("2006-01-02")}, true
}

func (s *Server) accountUsageCurrent(target accountUsageTarget, now time.Time) bool {
	if s.d.Store == nil {
		return false
	}
	current, ok := accountUsageSource(s.d.Store.Get(), target.id, now)
	return ok && current == target
}

func (s *Server) handleAccountUsage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(q) != 1 || len(q["id"]) != 1 || q.Get("id") == "" || len(q.Get("id")) > 160 {
		writeErr(w, http.StatusBadRequest, "读取参数有误")
		return
	}
	s.readAccountUsage(w, r, q.Get("id"), "")
}

func (s *Server) readAccountUsage(w http.ResponseWriter, r *http.Request, id, expectedSourceRevision string) {
	w.Header().Set("Cache-Control", "no-store")
	cache := s.accountUsage
	if s.d.Store == nil || cache == nil {
		writeErr(w, http.StatusServiceUnavailable, "用量暂不可用")
		return
	}
	target, ok := accountUsageSource(s.d.Store.Get(), id, cache.now())
	if !ok || expectedSourceRevision != "" && target.revision != expectedSourceRevision {
		writeErr(w, http.StatusConflict, "账号来源已变化或不支持用量查询")
		return
	}
	cache.mu.Lock()
	now := cache.now()
	entry := cache.entries[target.id]
	if entry != nil && (entry.target != target || !entry.at.IsZero() && (!now.Before(time.UnixMilli(entry.data.FreshUntil)) || entry.status != http.StatusOK)) {
		entry.cancel()
		delete(cache.entries, target.id)
		entry = nil
	}
	if entry == nil {
		if len(cache.entries) >= accountUsageCacheLimit {
			// Evict one oldest completed entry; active flights retain ownership and
			// remain bounded rather than spawning an unbounded background queue.
			var oldest *accountUsageEntry
			for _, candidate := range cache.entries {
				if !candidate.at.IsZero() && (oldest == nil || candidate.at.Before(oldest.at)) {
					oldest = candidate
				}
			}
			if oldest == nil {
				cache.mu.Unlock()
				writeErr(w, http.StatusServiceUnavailable, "用量查询繁忙，请稍后重试")
				return
			}
			delete(cache.entries, oldest.target.id)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		entry = &accountUsageEntry{target: target, done: make(chan struct{}), cancel: cancel}
		cache.entries[target.id] = entry
		go s.fetchAccountUsage(ctx, cache, entry)
	}
	cache.mu.Unlock()
	waitCtx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	select {
	case <-waitCtx.Done():
		writeErr(w, http.StatusServiceUnavailable, "用量查询尚未完成，请稍后重试")
	case <-entry.done:
		if entry.status != http.StatusOK || !s.accountUsageCurrent(target, cache.now()) {
			writeErr(w, http.StatusConflict, "账号来源或日期已变化，请重新读取")
			return
		}
		writeJSON(w, entry.data)
	}
}

func (s *Server) fetchAccountUsage(ctx context.Context, cache *accountUsageCache, entry *accountUsageEntry) {
	defer entry.cancel()
	var raw map[string]any
	select {
	case cache.slots <- struct{}{}:
		// Resolve the credential only for this dispatch. Cache entries retain
		// the opaque revision, never a deleted/rotated account's plaintext key.
		cfg := s.d.Store.Get()
		current, ok := accountUsageSource(cfg, entry.target.id, cache.now())
		if ok && current == entry.target && ctx.Err() == nil {
			account, _ := cfg.LocalAccount(entry.target.id)
			var err error
			raw, err = cache.fetch(ctx, entry.target.root, strings.TrimSpace(account.Key), 1)
			if err != nil || ctx.Err() != nil {
				raw = nil
			}
		}
		<-cache.slots
	case <-ctx.Done():
	}
	now := cache.now()
	data := normalizeAccountUsage(raw, entry.target.id, entry.target.revision, now)
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry.status = http.StatusConflict
	if cache.entries[entry.target.id] == entry && s.accountUsageCurrent(entry.target, now) {
		entry.data, entry.status = data, http.StatusOK
	}
	entry.at = now
	close(entry.done)
}

func normalizeAccountUsage(raw map[string]any, id, revision string, now time.Time) accountUsageDTO {
	endOfDay := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
	fresh := now.Add(time.Minute)
	if endOfDay.Before(fresh) {
		fresh = endOfDay
	}
	out := accountUsageDTO{ID: id, SourceRevision: revision, ObservedAt: now.UnixMilli(), FreshUntil: fresh.UnixMilli()}
	if raw == nil {
		return out
	}
	if billing, ok := raw["_salcara_billing"].(*relay.KeyBilling); ok && billing != nil && billing.FreshUntil > now.UnixMilli() {
		copy := *billing
		out.Billing = &copy
		if copy.FreshUntil < out.FreshUntil {
			out.FreshUntil = copy.FreshUntil
		}
	}
	out.Group = normalizeAccountUsageGroup(raw, out.Billing, now, out.FreshUntil)
	// The card deliberately shows the provider balance, which may be shared
	// across this provider's keys. Global/site/all-keys scope is valid for this
	// field, but never for today's per-key traffic below. Still query/cache each
	// credential independently: one hostname may host different billing accounts.
	// No USD default and no quota/credits-to-money conversion.
	currency := accountUsageCurrency(raw["currency"])
	if currency == "" {
		currency = accountUsageCurrency(raw["unit"])
	}
	if currency != "" {
		amount := accountUsageNumber(raw["balance"])
		if amount == nil {
			// Sub2API remaining in quota/subscription mode is not the supplier
			// wallet. Never label a per-key cap as a provider-wide balance.
			_, subscription := raw["subscription"]
			_, quota := raw["quota"]
			if raw["mode"] != "quota_limited" && !subscription && !quota && raw["planName"] == nil {
				amount = accountUsageNumber(raw["remaining"])
			}
		}
		if amount != nil {
			out.Balance = &accountUsageBalance{Amount: amount, Currency: currency}
		}
	}
	usage, _ := raw["usage"].(map[string]any)
	if !accountUsageScoped(raw) || !accountUsageScoped(usage) {
		return out
	}
	day := now.Format("2006-01-02")
	for _, parent := range []map[string]any{usage, raw} {
		if today, ok := parent["today"].(map[string]any); ok && accountUsageScoped(today) {
			// Explicit "today" means the provider's day, not a fabricated local
			// timezone claim. Date binds this DTO to its local read/cache day.
			if accountUsageMatchesDay(today, day, true) {
				out.Today = normalizeAccountUsageToday(today, day, "provider_today", currency)
			}
			return out
		}
	}
	for _, parent := range []map[string]any{raw, usage} {
		for _, field := range []string{"daily_usage", "daily"} {
			rows, ok := parent[field].([]any)
			if !ok {
				continue
			}
			var matched map[string]any
			for _, value := range rows {
				row, _ := value.(map[string]any)
				if accountUsageMatchesDay(row, day, false) {
					if matched != nil { // Ambiguous multiple summaries are not additive.
						return out
					}
					matched = row
				}
			}
			if matched != nil && accountUsageScoped(matched) {
				out.Today = normalizeAccountUsageToday(matched, day, "provider_daily", currency)
				return out
			}
		}
	}
	return out
}

func normalizeAccountUsageToday(row map[string]any, day, basis, parentCurrency string) *accountUsageToday {
	out := &accountUsageToday{Date: day, Basis: basis, Requests: accountUsageCount(row, "requests", "request_count"), Tokens: accountUsageCount(row, "total_tokens", "tokens", "totalTokens")}
	currency := accountUsageCurrency(row["currency"])
	if currency == "" {
		currency = accountUsageCurrency(row["unit"])
	}
	if currency == "" {
		currency = parentCurrency
	}
	amount := accountUsageNumber(row["actual_cost"])
	if usd, exists := row["actual_cost_usd"]; exists {
		explicit := accountUsageNumber(usd)
		if explicit == nil || amount != nil && (*amount != *explicit || currency != "" && currency != "USD") {
			return out
		}
		amount, currency = explicit, "USD"
	}
	if currency != "" && amount != nil && *amount >= 0 {
		out.ActualCost = &accountUsageBalance{Amount: amount, Currency: currency}
	}
	return out
}
func accountUsageMatchesDay(row map[string]any, day string, allowUndated bool) bool {
	present := false
	for _, field := range []string{"date", "day"} {
		if raw, exists := row[field]; exists {
			present = true
			if value, ok := raw.(string); !ok || value != day {
				return false
			}
		}
	}
	return present || allowUndated
}
func accountUsageNumber(raw any) *float64 {
	var n float64
	switch v := raw.(type) {
	case float64:
		n = v
	case string:
		var err error
		if strings.TrimSpace(v) == "" || len(v) > 64 {
			return nil
		}
		n, err = strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return nil
		}
	default:
		return nil
	}
	if math.IsNaN(n) || math.IsInf(n, 0) || math.Abs(n) > 9007199254740991 {
		return nil
	}
	return &n
}
func accountUsageCount(row map[string]any, names ...string) *int64 {
	for _, field := range names {
		if value, exists := row[field]; exists {
			n := accountUsageNumber(value)
			if n == nil || *n < 0 || math.Trunc(*n) != *n {
				return nil
			}
			count := int64(*n)
			return &count
		}
	}
	return nil
}
func accountUsageCurrency(raw any) string {
	v, _ := raw.(string)
	v = strings.ToUpper(strings.TrimSpace(v))
	switch v {
	case "USD", "CNY", "EUR", "GBP", "JPY", "KRW", "AUD", "CAD", "CHF", "HKD", "SGD", "NZD", "INR", "RUB", "BRL", "TWD", "THB", "MYR", "IDR", "VND", "MXN", "ZAR", "AED", "SAR", "TRY", "PLN", "SEK", "NOK", "DKK":
		return v
	}
	return ""
}

func accountUsageScoped(row map[string]any) bool {
	for _, field := range []string{"scope", "usage_scope", "stats_scope"} {
		if raw, exists := row[field]; exists {
			v, ok := raw.(string)
			if !ok {
				return false
			}
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "key", "api_key", "token":
			default: // Explicit account/site/global/unknown scope is not per-Key usage.
				return false
			}
		}
	}
	for _, field := range []string{"is_global", "all_keys", "account_wide"} {
		if raw, exists := row[field]; exists && raw != false {
			return false
		}
	}
	return true
}
