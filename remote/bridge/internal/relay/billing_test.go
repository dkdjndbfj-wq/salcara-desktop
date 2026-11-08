package relay

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func billingFixture(now time.Time) string {
	return fmt.Sprintf(`{"object":"sub2api.key_billing","schema_version":1,"billing_scope":"token","group_rate_multiplier":0.8,"user_rate_multiplier":0.5,"resolved_rate_multiplier":0.5,"effective_rate_multiplier":0.5,"peak_rate_enabled":false,"observed_at":%q}`, now.Format(time.RFC3339Nano))
}

func TestKeyBillingContractAndValidation(t *testing.T) {
	now := time.Date(2026, 10, 13, 10, 10, 20, 0, time.UTC)
	valid := billingFixture(now)
	got, err := parseKeyBilling([]byte(valid), now)
	if err != nil || got.EffectiveMultiplier != .5 || got.BaseMultiplier != .5 || got.FreshUntil-now.UnixMilli() != 60000 {
		t.Fatalf("billing=%+v error=%v", got, err)
	}
	for _, body := range []string{
		valid + `{}`, strings.Replace(valid, `"schema_version":1`, `"schema_version":2`, 1),
		strings.Replace(valid, `"billing_scope":"token"`, `"billing_scope":"images"`, 1),
		strings.Replace(valid, `"resolved_rate_multiplier":0.5`, `"resolved_rate_multiplier":0.8`, 1),
		strings.Replace(valid, `"effective_rate_multiplier":0.5`, `"effective_rate_multiplier":null`, 1),
		strings.Replace(valid, `"effective_rate_multiplier":0.5`, `"effective_rate_multiplier":101`, 1),
		strings.Replace(valid, `"effective_rate_multiplier":0.5`, `"effective_rate_multiplier":-1`, 1),
		strings.Replace(valid, `"object":`, `"Object":`, 1),
		strings.Replace(valid, `"object":`, `"object":"sub2api.key_billing","object":`, 1),
		billingFixture(now.Add(-2 * time.Minute)), billingFixture(now.Add(time.Minute)),
	} {
		if _, err := parseKeyBilling([]byte(body), now); err == nil {
			t.Fatalf("accepted invalid body: %s", body)
		}
	}
	zero := strings.ReplaceAll(strings.ReplaceAll(valid, `0.8`, `0`), `0.5`, `0`)
	if got, err := parseKeyBilling([]byte(zero), now); err != nil || got.EffectiveMultiplier != 0 {
		t.Fatal("explicit zero must remain valid", got, err)
	}
}

func TestKeyBillingPeakBoundary(t *testing.T) {
	now := time.Date(2026, 10, 13, 10, 10, 20, 0, time.UTC)
	body := strings.Replace(billingFixture(now), `"peak_rate_enabled":false`, `"peak_rate_enabled":true,"peak_start":"10:00","peak_end":"11:00","peak_rate_multiplier":2,"applied_peak_multiplier":2,"timezone":"UTC"`, 1)
	body = strings.Replace(body, `"effective_rate_multiplier":0.5`, `"effective_rate_multiplier":1`, 1)
	got, err := parseKeyBilling([]byte(body), now)
	if err != nil || got.EffectiveMultiplier != 1 || got.FreshUntil != now.Truncate(time.Minute).Add(time.Minute).UnixMilli() {
		t.Fatal(got, err)
	}
	for _, bad := range []string{strings.Replace(body, `"peak_end":"11:00"`, `"peak_end":"10:00"`, 1), strings.Replace(body, `"timezone":"UTC"`, `"timezone":"not-a-zone"`, 1), strings.Replace(body, `"applied_peak_multiplier":2`, `"applied_peak_multiplier":1`, 1)} {
		if _, err := parseKeyBilling([]byte(bad), now); err == nil {
			t.Fatal("accepted inconsistent peak")
		}
	}
}

func TestBillingTransportAndCardJoin(t *testing.T) {
	var traps atomic.Int32
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { traps.Add(1) }))
	defer trap.Close()
	for _, status := range []int{200, 302, 404} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer synthetic-card-key" {
				t.Error("wrong credential")
			}
			switch r.URL.Path {
			case "/prefix/v1/usage":
				fmt.Fprint(w, `{"balance":4,"unit":"USD","_salcara_billing":{"effectiveMultiplier":999}}`)
			case "/prefix/v1/sub2api/billing":
				w.Header().Set("Location", trap.URL)
				w.WriteHeader(status)
				fmt.Fprint(w, billingFixture(time.Now()))
			default:
				t.Error("wrong API path", r.URL.Path)
			}
		}))
		got, err := UsageCardDays(context.Background(), srv.URL+"/prefix", "synthetic-card-key", 1)
		srv.Close()
		if err != nil || got["balance"] != float64(4) {
			t.Fatal("billing failure hid usage", got, err)
		}
		bill, ok := got["_salcara_billing"].(*KeyBilling)
		if (status == 200) != ok {
			t.Fatal("forged or missing typed billing", bill)
		}
	}
	if traps.Load() != 0 {
		t.Fatal("credential redirect followed")
	}
}

func TestKeyBillingClockSkewCannotCrossPeakBoundary(t *testing.T) {
	for _, clock := range []string{"2026-10-13T10:59:45Z", "2026-10-13T11:59:45Z", "2026-10-13T23:59:45Z"} {
		now, _ := time.Parse(time.RFC3339, clock)
		for _, skew := range []time.Duration{-25 * time.Second, 25 * time.Second} {
			at := now.Add(skew)
			start, end := "11:00", "12:00"
			if now.Hour() == 23 {
				start, end = "00:00", "01:00"
			}
			in := (at.Hour() == 11 && start == "11:00") || (at.Hour() == 0 && start == "00:00")
			applied, effective := "1", "0.5"
			if in {
				applied, effective = "2", "1"
			}
			body := strings.Replace(billingFixture(at), `"peak_rate_enabled":false`, fmt.Sprintf(`"peak_rate_enabled":true,"peak_start":%q,"peak_end":%q,"peak_rate_multiplier":2,"applied_peak_multiplier":%s,"timezone":"UTC"`, start, end, applied), 1)
			body = strings.Replace(body, `"effective_rate_multiplier":0.5`, `"effective_rate_multiplier":`+effective, 1)
			got, err := parseKeyBilling([]byte(body), now)
			if skew > 0 && err == nil {
				t.Fatal("future rate crossed boundary", clock, got)
			}
			if skew < 0 && (err != nil || got.FreshUntil > now.Truncate(time.Minute).Add(time.Minute).UnixMilli()) {
				t.Fatal("same-period skew invalid", clock, got, err)
			}
		}
	}
}

func TestBillingRejectsOversizedAndInvalidResponses(t *testing.T) {
	for _, body := range []string{strings.Repeat(" ", 64<<10) + billingFixture(time.Now()), `{"object":`, `[]`, billingFixture(time.Now()) + `{}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		_, err := KeyBillingInfo(context.Background(), srv.URL, "synthetic")
		srv.Close()
		if err == nil {
			t.Fatal("accepted malformed billing response")
		}
	}
}

func TestBillingTimeoutDoesNotDiscardSuccessfulUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/usage" {
			fmt.Fprint(w, `{"balance":4,"unit":"USD"}`)
			return
		}
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	got, err := UsageCardDays(ctx, srv.URL, "synthetic", 1)
	if ctx.Err() != nil || err != nil || got["balance"] != float64(4) || got["_salcara_billing"] != nil {
		t.Fatal("optional billing hid available usage", got, err, ctx.Err())
	}
}
