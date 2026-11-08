package codexaccounts

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Independently constructed from the shape emitted by pinned Cockpit commit
// be24ce6171279741c2cd141bc02bd9f380f4b67c, codexExportFormats.ts:
// buildSub2apiCredentials emits an ISO timestamp; toSub2apiAccount also emits
// its Unix-seconds equivalent when the OAuth account has no refresh token.
// No source code or live account data is used in this fixture.
func referenceSub2APIAccessOnly(t *testing.T) map[string]any {
	t.Helper()
	return map[string]any{
		"name": "Synthetic Team", "platform": "openai", "type": "oauth",
		"concurrency": 3, "priority": 50,
		"credentials": map[string]any{
			"access_token": browserIDToken(`{"exp":1800000000}`),
			"expires_at":   "2027-01-15T08:00:00.000Z", "email": "fixture@example.test",
			"chatgpt_account_id": "synthetic-workspace", "plan_type": "team",
			"subscription_expires_at": "2027-01-02T03:04:05.000Z",
		},
		"expires_at": int64(1800000000), "auto_pause_on_expired": true,
	}
}

func TestImportReferenceSub2APIAccessOnlyExpiry(t *testing.T) {
	item := referenceSub2APIAccessOnly(t)
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	for _, shape := range []string{"record", "bundle", "jsonl", "files"} {
		t.Run(shape, func(t *testing.T) {
			var p *PreparedImport
			var err error
			want := 1
			switch shape {
			case "record":
				p, err = ParseImport(raw)
			case "bundle":
				p, err = ParseImport([]byte(`{"type":"sub2api-data","version":1,"exported_at":"2026-10-13T00:00:00Z","proxies":[],"accounts":[` + string(raw) + `]}`))
			case "jsonl":
				want = 2
				p, err = ParseImport([]byte(string(raw) + "\n" + `{"access_token":"synthetic-second"}`))
			case "files":
				want = 2
				p, err = ParseImportFiles([][]byte{raw, []byte(`{"access_token":"synthetic-second"}`)}, "json")
			}
			if err != nil || p == nil {
				t.Fatalf("normal access-only OAuth export rejected: %v", err)
			}
			if len(p.records) != want {
				t.Fatalf("got %d records, want %d", len(p.records), want)
			}
			c := p.records[0]
			if !c.ExpiresAt.Equal(time.Unix(1800000000, 0)) || c.AccountID != "synthetic-workspace" || c.Plan != "team" || c.RefreshToken != "" {
				t.Fatal("expiry, explicit workspace, plan, or access-only mode changed")
			}
			if !c.SubscriptionActiveUntil.Equal(time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)) {
				t.Fatal("subscription hint was lost")
			}
			preview, err := json.Marshal(p.Preview())
			if err != nil || strings.Contains(string(preview), "synthetic-workspace") || strings.Contains(string(preview), c.AccessToken) {
				t.Fatal("preview exposed token or workspace selector")
			}
		})
	}
}

func TestImportReferenceExpiryDoesNotRelaxConflicts(t *testing.T) {
	for _, outer := range []any{
		int64(1800000001), int64(1800000000001), "2027-01-15T08:00:00.001Z", true,
		map[string]any{}, []any{}, "not-a-time",
	} {
		item := referenceSub2APIAccessOnly(t)
		item["expires_at"] = outer
		raw, err := json.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		if p, err := ParseImport(raw); err != ErrInvalidImport || p != nil {
			t.Fatal("different or malformed expiry accepted")
		}
	}
	for _, key := range []string{"account_id", "email", "plan_type"} {
		item := referenceSub2APIAccessOnly(t)
		delete(item, "expires_at")
		item[key] = int64(1800000000)
		credentials := item["credentials"].(map[string]any)
		if key == "account_id" {
			delete(credentials, "chatgpt_account_id")
		}
		credentials[key] = "1800000000"
		raw, err := json.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		if p, err := ParseImport(raw); err != ErrInvalidImport || p != nil {
			t.Fatalf("expiry compatibility relaxed %s type isolation", key)
		}
	}
}

func TestImportReferenceExpiryComparesExactInstants(t *testing.T) {
	for _, tc := range []struct {
		name            string
		outer, inner    any
		wantNanoseconds int64
	}{
		{"seconds-string", "1800000000", "2027-01-15T08:00:00.000Z", 0},
		{"milliseconds", int64(1800000000123), "2027-01-15T08:00:00.123Z", 123000000},
		{"nested-milliseconds", "2027-01-15T08:00:00.123Z", int64(1800000000123), 123000000},
		{"timezone", "2027-01-15T16:00:00.123+08:00", "2027-01-15T08:00:00.123Z", 123000000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := referenceSub2APIAccessOnly(t)
			item["expires_at"] = tc.outer
			item["credentials"].(map[string]any)["expires_at"] = tc.inner
			raw, err := json.Marshal(item)
			if err != nil {
				t.Fatal(err)
			}
			p, err := ParseImport(raw)
			if err != nil || p == nil {
				t.Fatalf("equivalent expiry rejected: %v", err)
			}
			if !p.records[0].ExpiresAt.Equal(time.Unix(1800000000, tc.wantNanoseconds)) {
				t.Fatal("equivalent expiry lost precision")
			}
		})
	}
}
