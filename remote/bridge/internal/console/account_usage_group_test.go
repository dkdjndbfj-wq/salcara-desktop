package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/relay"
)

func groupUsageRaw(t *testing.T, text string) map[string]any {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestAccountUsageGroupExplicitFieldsAndConflict(t *testing.T) {
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ name, json, want string }{
		{"flat", `{"group_name":" 编程组 "}`, "编程组"},
		{"nested", `{"group":{"name":"coding","scope":"key"}}`, "coding"},
		{"same", `{"group_name":" coding ","group":{"name":"coding"}}`, "coding"},
		{"conflict", `{"group_name":"coding","group":{"name":"images"}}`, ""},
		{"malformed nested", `{"group_name":"coding","group":"images"}`, ""},
		{"invalid alternative", `{"group_name":null,"group":{"name":"coding"}}`, ""},
		{"global", `{"group_name":"coding","scope":"account_wide"}`, ""},
		{"global nested", `{"group":{"name":"coding","all_keys":true}}`, ""},
		{"invalid key", `{"group_name":"coding","isValid":false}`, ""},
		{"error", `{"group_name":"coding","error":"failed"}`, ""},
		{"control", `{"group_name":"coding\nimages"}`, ""},
		{"bidi", `{"group_name":"coding\u202e"}`, ""},
		{"empty", `{"group_name":"  "}`, ""},
		{"numeric", `{"group_name":123}`, ""},
		{"historic not current", `{"model_stats":[{"group_name":"old"}],"daily":[{"group_name":"old"}]}`, ""},
		{"model not group", `{"models":["claude-opus"],"planName":"gpt-5"}`, ""},
		{"wallet explicit group valid", `{"balance":3,"planName":"钱包余额","group_name":"API分组"}`, "API分组"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := normalizeAccountUsage(groupUsageRaw(t, tc.json), "card", "revision", now)
			if tc.want == "" {
				if out.Group != nil {
					t.Fatalf("invented group: %+v", out.Group)
				}
				return
			}
			if out.Group == nil || out.Group.Name != tc.want || out.Group.Source != "provider_key_usage" || out.Group.FreshUntil != out.FreshUntil {
				t.Fatalf("wrong group projection: %+v", out.Group)
			}
			b, _ := json.Marshal(out.Group)
			var keys map[string]any
			json.Unmarshal(b, &keys)
			if len(keys) != 3 {
				t.Fatal("group DTO leaked provider fields")
			}
		})
	}
	for _, n := range []int{120, 121} {
		out := normalizeAccountUsage(map[string]any{"group_name": strings.Repeat("编", n)}, "card", "revision", now)
		if (out.Group != nil) != (n == 120) {
			t.Fatal("group limit must use Unicode characters")
		}
	}
}

func TestAccountUsageGroupSub2APIFingerprintOnly(t *testing.T) {
	now := time.Now()
	base := func() map[string]any {
		return map[string]any{"mode": "unrestricted", "isValid": true, "unit": "USD", "planName": "订阅编程组", "_salcara_billing": &relay.KeyBilling{Source: "sub2api.key_billing", Scope: "token", ObservedAt: now.UnixMilli(), FreshUntil: now.Add(25 * time.Second).UnixMilli()}}
	}
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
		want   bool
	}{
		{"subscription context absent", func(map[string]any) {}, true},
		{"explicit subscription", func(raw map[string]any) {
			raw["subscription"] = map[string]any{"daily_usage_usd": float64(2), "daily_limit_usd": float64(10), "weekly_limit_usd": nil}
		}, true},
		{"same explicit group", func(raw map[string]any) { raw["group_name"] = "订阅编程组" }, true},
		{"conflict explicit group", func(raw map[string]any) { raw["group_name"] = "different" }, false},
		{"wallet", func(raw map[string]any) { raw["balance"] = float64(3) }, false},
		{"null balance is ambiguous", func(raw map[string]any) { raw["balance"] = nil }, false},
		{"wallet label", func(raw map[string]any) { raw["planName"] = "钱包余额" }, false},
		{"quota", func(raw map[string]any) { raw["quota"] = map[string]any{} }, false},
		{"quota mode", func(raw map[string]any) { raw["mode"] = "quota_limited" }, false},
		{"invalid", func(raw map[string]any) { raw["isValid"] = false }, false},
		{"unknown currency", func(raw map[string]any) { raw["unit"] = "credits" }, false},
		{"malformed subscription", func(raw map[string]any) { raw["subscription"] = "active" }, false},
		{"global subscription", func(raw map[string]any) { raw["subscription"] = map[string]any{"account_wide": true} }, false},
		{"negative subscription usage", func(raw map[string]any) { raw["subscription"] = map[string]any{"daily_usage_usd": float64(-1)} }, false},
		{"billing absent", func(raw map[string]any) { delete(raw, "_salcara_billing") }, false},
		{"billing JSON spoof", func(raw map[string]any) {
			raw["_salcara_billing"] = map[string]any{"source": "sub2api.key_billing", "scope": "token", "freshUntil": now.Add(time.Minute).UnixMilli()}
		}, false},
		{"billing expired", func(raw map[string]any) {
			raw["_salcara_billing"].(*relay.KeyBilling).FreshUntil = now.Add(-time.Second).UnixMilli()
		}, false},
		{"wrong billing source", func(raw map[string]any) { raw["_salcara_billing"].(*relay.KeyBilling).Source = "unrelated" }, false},
		{"wrong billing scope", func(raw map[string]any) { raw["_salcara_billing"].(*relay.KeyBilling).Scope = "account" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := base()
			tc.change(raw)
			out := normalizeAccountUsage(raw, "card", "revision", now)
			if (out.Group != nil) != tc.want {
				t.Fatalf("group eligibility = %+v", out.Group)
			}
			if out.Group != nil && (out.Group.Name != "订阅编程组" || out.Group.FreshUntil != now.Add(25*time.Second).UnixMilli()) {
				t.Fatal("wrong subscription name or billing-clamped TTL")
			}
		})
	}
}

func TestAccountUsageGroupExactKeyCacheAndUpstreamGroupRefresh(t *testing.T) {
	s, h := accountUsageTestServer(t, 2)
	base := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	var millis atomic.Int64
	millis.Store(base.UnixMilli())
	s.accountUsage.now = func() time.Time { return time.UnixMilli(millis.Load()) }
	var generation, calls atomic.Int32
	s.accountUsage.fetch = func(_ context.Context, _, key string, _ int) (map[string]any, error) {
		calls.Add(1)
		name := "分组A"
		if key == "synthetic-key-1" {
			name = "分组B"
		} else if key != "synthetic-key-0" {
			t.Error("unexpected exact API key")
		}
		if generation.Load() > 0 {
			name += "-new"
		}
		return map[string]any{"group_name": name, "credential": "not-forwarded"}, nil
	}
	first := accountUsageDecode(t, accountUsageRequest(s, h, "card-0"))
	second := accountUsageDecode(t, accountUsageRequest(s, h, "card-1"))
	if first.Group == nil || second.Group == nil || first.Group.Name != "分组A" || second.Group.Name != "分组B" || calls.Load() != 2 {
		t.Fatal("same-host different keys mixed groups")
	}
	generation.Store(1)
	cached := accountUsageDecode(t, accountUsageRequest(s, h, "card-0"))
	if cached.Group.Name != first.Group.Name || calls.Load() != 2 {
		t.Fatal("cache unexpectedly re-queried")
	}
	millis.Store(base.Add(time.Minute).UnixMilli())
	updated := accountUsageDecode(t, accountUsageRequest(s, h, "card-0"))
	if updated.Group.Name != "分组A-new" || updated.SourceRevision != first.SourceRevision || calls.Load() != 3 {
		t.Fatal("unchanged key did not learn upstream group change after TTL")
	}
}

func TestAccountUsageGroupLateResponseCannotCrossSourceReplacement(t *testing.T) {
	s, h := accountUsageTestServer(t, 1)
	entered, release := make(chan struct{}), make(chan struct{})
	s.accountUsage.fetch = func(context.Context, string, string, int) (map[string]any, error) {
		close(entered)
		<-release
		return map[string]any{"group_name": "stale-private-group"}, nil
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- accountUsageRequest(s, h, "card-0") }()
	<-entered
	if err := s.d.Store.Update(func(c *config.Config) error { c.LocalAccounts[0].Key = "synthetic-new-key"; return nil }); err != nil {
		t.Fatal(err)
	}
	close(release)
	w := <-done
	if w.Code != http.StatusConflict || strings.Contains(w.Body.String(), "stale-private-group") {
		t.Fatal("stale source group reached replacement card")
	}
}
