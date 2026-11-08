package relay

// Independent client for the public Sub2API key-billing protocol. This is a
// token-rate observation, not a model price list or proof of a settled charge.
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
	_ "time/tzdata"
)

type KeyBilling struct {
	Source              string  `json:"source"`
	Scope               string  `json:"scope"`
	EffectiveMultiplier float64 `json:"effectiveMultiplier"`
	BaseMultiplier      float64 `json:"baseMultiplier"`
	Peak                bool    `json:"peak"`
	ObservedAt          int64   `json:"observedAt"`
	FreshUntil          int64   `json:"freshUntil"`
}

var errBilling = errors.New("上游未提供有效的倍率信息")

// KeyBillingInfo never follows a redirect with the saved API key. The caller
// binds this result to the exact key/source revision, not to a vendor hostname.
func KeyBillingInfo(ctx context.Context, root, key string) (*KeyBilling, error) {
	u, err := url.Parse(root)
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.TrimSpace(key) == "" {
		return nil, errBilling
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(root, "/")+"/v1/sub2api/billing", nil)
	if err != nil {
		return nil, errBilling
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("User-Agent", "SalcaraBridge")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, errBilling
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errBilling
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
	if err != nil || len(b) > 64<<10 {
		return nil, errBilling
	}
	return parseKeyBilling(b, time.Now())
}

func parseKeyBilling(b []byte, now time.Time) (*KeyBilling, error) {
	// Reject duplicate and differently-cased fields rather than letting JSON's
	// last-value or case-insensitive struct matching pick a billing rate.
	d := json.NewDecoder(bytes.NewReader(b))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errBilling
	}
	fields := map[string]json.RawMessage{}
	seen := map[string]bool{}
	for d.More() {
		token, err = d.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[strings.ToLower(name)] {
			return nil, errBilling
		}
		seen[strings.ToLower(name)] = true
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return nil, errBilling
		}
		fields[name] = value
	}
	if token, err = d.Token(); err != nil || token != json.Delim('}') {
		return nil, errBilling
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, errBilling
	}
	read := func(name string, dest any) bool {
		raw, ok := fields[name]
		return ok && string(raw) != "null" && json.Unmarshal(raw, dest) == nil
	}
	var object, scope, observed string
	var version int
	var group, resolved, effective float64
	var peak bool
	if !read("object", &object) || object != "sub2api.key_billing" || !read("schema_version", &version) || version != 1 || !read("billing_scope", &scope) || scope != "token" || !read("group_rate_multiplier", &group) || !read("resolved_rate_multiplier", &resolved) || !read("effective_rate_multiplier", &effective) || !read("peak_rate_enabled", &peak) || !read("observed_at", &observed) {
		return nil, errBilling
	}
	valid := func(n float64) bool { return !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 && n <= 100 }
	close := func(a, b float64) bool { return math.Abs(a-b) <= 1e-8*math.Max(1, math.Max(a, b)) }
	if !valid(group) || !valid(resolved) || !valid(effective) {
		return nil, errBilling
	}
	base := group
	if raw, exists := fields["user_rate_multiplier"]; exists && string(raw) != "null" {
		if !read("user_rate_multiplier", &base) || !valid(base) {
			return nil, errBilling
		}
	}
	if !close(base, resolved) {
		return nil, errBilling
	}
	at, err := time.Parse(time.RFC3339Nano, observed)
	if err != nil || at.After(now.Add(30*time.Second)) || at.Before(now.Add(-time.Minute)) {
		return nil, errBilling
	}
	applied := float64(1)
	if peak {
		var multiplier float64
		var start, end, zone string
		if !read("applied_peak_multiplier", &applied) || !read("peak_rate_multiplier", &multiplier) || !valid(applied) || !valid(multiplier) || !read("peak_start", &start) || !read("peak_end", &end) || !read("timezone", &zone) {
			return nil, errBilling
		}
		loc, e := time.LoadLocation(zone)
		a, e1 := time.Parse("15:04", start)
		z, e2 := time.Parse("15:04", end)
		if e != nil || e1 != nil || e2 != nil || start == end {
			return nil, errBilling
		}
		lo, hi := a.Hour()*60+a.Minute(), z.Hour()*60+z.Minute()
		// The public protocol's current implementation only supports same-day
		// windows. Unknown schedules are not a reason to guess a billing rate.
		if lo >= hi {
			return nil, errBilling
		}
		inPeak := func(value time.Time) bool {
			t := value.In(loc)
			minute := t.Hour()*60 + t.Minute()
			return minute >= lo && minute < hi
		}
		inside := inPeak(at)
		// Tolerated clock skew must not move a rate across a peak boundary.
		if inside != inPeak(now) {
			return nil, errBilling
		}
		expected := float64(1)
		if inside {
			expected = multiplier
		}
		if !close(applied, expected) {
			return nil, errBilling
		}
	}
	if !close(effective, resolved*applied) {
		return nil, errBilling
	}
	fresh := at.Add(time.Minute)
	if now.Add(time.Minute).Before(fresh) {
		fresh = now.Add(time.Minute)
	}
	// Peak boundaries occur at minute granularity. Expire before the next one
	// instead of extrapolating today's snapshot into another pricing period.
	if peak && now.Truncate(time.Minute).Add(time.Minute).Before(fresh) {
		fresh = now.Truncate(time.Minute).Add(time.Minute)
	}
	if !fresh.After(now) {
		return nil, errBilling
	}
	return &KeyBilling{Source: "sub2api.key_billing", Scope: "token", EffectiveMultiplier: effective, BaseMultiplier: resolved, Peak: peak, ObservedAt: at.UnixMilli(), FreshUntil: fresh.UnixMilli()}, nil
}

// UsageCardDays joins two read-only observations. A missing billing endpoint
// must not hide a valid balance, nor make a model-inference request as a probe.
func UsageCardDays(ctx context.Context, root, key string, days int) (map[string]any, error) {
	type result struct {
		data map[string]any
		err  error
	}
	ch := make(chan result, 1)
	go func() { data, err := UsageDays(ctx, root, key, days); ch <- result{data, err} }()
	// The optional rate endpoint must not spend the entire card-read deadline
	// and discard an otherwise successful wallet/usage response.
	billingCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	billing, _ := KeyBillingInfo(billingCtx, root, key)
	cancel()
	r := <-ch
	if r.data == nil {
		r.data = map[string]any{}
	}
	// A concrete Go type cannot be forged by the upstream JSON payload.
	delete(r.data, "_salcara_billing")
	if billing != nil {
		r.data["_salcara_billing"] = billing
		return r.data, nil
	}
	return r.data, r.err
}
