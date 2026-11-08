package codexaccounts

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func subscriptionTestFetcher(server *httptest.Server) *officialSubscriptionFetcher {
	return &officialSubscriptionFetcher{client: server.Client(), allowLoopback: true,
		accountsEndpoint:      server.URL + "/backend-api/accounts/check/v4-2023-04-27",
		subscriptionsEndpoint: server.URL + "/backend-api/subscriptions"}
}

func subscriptionTestAuth() *coreauth.Auth {
	return &coreauth.Auth{Provider: "codex", Metadata: map[string]any{"access_token": "synthetic-subscription-secret", "account_id": "selected-account"}}
}

func TestOfficialSubscriptionExactSelectionAndFallback(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(map[bool]string{false: "entitlement", true: "subscription_fallback"}[fallback], func(t *testing.T) {
			var calls atomic.Int32
			expiry := "2032-04-05T06:07:08Z"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer synthetic-subscription-secret" || r.Header.Get("ChatGPT-Account-Id") != "selected-account" {
					t.Error("credential request contract changed")
				}
				if r.Header.Get("originator") != "salcara" || r.Header.Get("Referer") != "" {
					t.Error("client provenance changed")
				}
				w.Header().Set("Content-Type", "application/json")
				if strings.HasPrefix(r.URL.Path, "/backend-api/accounts/check/") {
					if r.URL.RawQuery != "" {
						t.Error("account check accepted query")
					}
					term := `,"entitlement":{"subscription_plan":"team","expires_at":"` + expiry + `"}`
					if fallback {
						term = ""
					}
					_, _ = w.Write([]byte(`{"accounts":{"default":{"account":{"account_id":"another-personal-account","is_default":true},"entitlement":{"expires_at":"2098-01-01T00:00:00Z"}},"selected-org":{"account":{"account_id":"selected-account"}` + term + `}}}`))
					return
				}
				if r.URL.Path != "/backend-api/subscriptions" || r.URL.Query().Get("account_id") != "selected-account" || len(r.URL.Query()) != 1 {
					t.Error("fallback did not pin selected account")
				}
				_, _ = w.Write([]byte(`{"account_id":"selected-account","active_until":"` + expiry + `"}`))
			}))
			defer server.Close()
			result, err := subscriptionTestFetcher(server).FetchSubscription(context.Background(), subscriptionTestAuth())
			wantCalls := int32(1)
			if fallback {
				wantCalls = 2
			}
			if err != nil || result.ProviderAccountID != "selected-account" || result.ActiveUntil == nil || result.ActiveUntil.Format(time.RFC3339) != expiry || calls.Load() != wantCalls {
				t.Fatalf("subscription result=%+v calls=%d error=%v", result, calls.Load(), err)
			}
		})
	}
}

func TestOfficialSubscriptionUnknownMalformedAndIdentityFailures(t *testing.T) {
	cases := []struct{ name, accountBody, subscriptionBody, code string }{
		{"no_default_fallback", `{"accounts":{"default":{"account":{"account_id":"other","is_default":true},"entitlement":{"expires_at":"2032-01-01T00:00:00Z"}}}}`, "", "identity_mismatch"},
		{"key_identity_disagrees", `{"accounts":{"selected-account":{"account":{"account_id":"other"},"entitlement":{"expires_at":"2032-01-01T00:00:00Z"}}}}`, "", "identity_mismatch"},
		{"ambiguous", `{"accounts":{"one":{"account":{"account_id":"selected-account"}},"two":{"account":{"id":"selected-account"}}}}`, "", "identity_mismatch"},
		{"duplicate", `{"accounts":{},"accounts":{}}`, "", "invalid_response"},
		{"invalid_expiry", `{"accounts":{"selected-account":{"entitlement":{"expires_at":false}}}}`, "", "invalid_response"},
		{"generic_expiry_is_not_subscription", `{"accounts":{"selected-account":{"account":{"expires_at":"2099-01-01T00:00:00Z"}}}}`, `{"expires_at":"2099-01-01T00:00:00Z","exp":4070908800}`, "no_expiry"},
		{"missing", `{"accounts":{"selected-account":{"account":{}}}}`, `{}`, "no_expiry"},
		{"free_entitlement_is_not_a_paid_term", `{"accounts":{"selected-account":{"entitlement":{"subscription_plan":"free","expires_at":"2099-01-01T00:00:00Z"}}}}`, `{}`, "no_expiry"},
		{"free_subscription_has_no_paid_term", `{"accounts":{"selected-account":{"account":{}}}}`, `{"subscription_plan":"chatgptfreeplan","active_until":"2099-01-01T00:00:00Z"}`, "no_expiry"},
		{"fallback_wrong_identity", `{"accounts":{"selected-account":{"account":{}}}}`, `{"account_id":"other","active_until":"2032-01-01T00:00:00Z"}`, "identity_mismatch"},
		{"fallback_bad_date", `{"accounts":{"selected-account":{"account":{}}}}`, `{"active_until":"not-a-date synthetic-subscription-secret"}`, "invalid_response"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				body := tc.accountBody
				if r.URL.Path == "/backend-api/subscriptions" {
					body = tc.subscriptionBody
					if body == "" {
						t.Error("unexpected fallback after conclusive failure")
					}
				}
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			result, err := subscriptionTestFetcher(server).FetchSubscription(context.Background(), subscriptionTestAuth())
			if err == nil || subscriptionErrorCode(err) != tc.code || result.ActiveUntil != nil {
				t.Fatalf("result=%+v err=%v code=%q", result, err, subscriptionErrorCode(err))
			}
			if strings.Contains(err.Error(), "synthetic") || strings.Contains(err.Error(), "selected-account") {
				t.Fatal("private provider data leaked in error")
			}
		})
	}
}

func TestOfficialSubscriptionHTTPBoundary(t *testing.T) {
	var redirected atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	defer sink.Close()
	for _, tc := range []struct {
		name, contentType, body string
		status                  int
		needsLogin              bool
	}{
		{"401_json", "application/json", `{"error":{"message":"synthetic-provider-secret"}}`, 401, true},
		{"401_html", "text/html", `synthetic-provider-secret`, 401, false},
		{"403", "application/json", `{}`, 403, false},
		{"429", "application/json", `{}`, 429, false},
		{"500", "application/json", `{}`, 500, false},
		{"redirect", "application/json", `{}`, 302, false},
		{"html_success", "text/html", `{}`, 200, false},
		{"oversize", "application/json", strings.Repeat("x", maxSubscriptionBodyBytes+1), 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.Header().Set("Location", sink.URL)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			_, err := subscriptionTestFetcher(server).FetchSubscription(context.Background(), subscriptionTestAuth())
			if err == nil || errors.Is(err, ErrNeedsLogin) != tc.needsLogin {
				t.Fatalf("error=%v", err)
			}
			if strings.Contains(err.Error(), "synthetic") {
				t.Fatal("HTTP error leaked provider content")
			}
		})
	}
	if redirected.Load() != 0 {
		t.Fatal("redirect forwarded credential")
	}
	for _, endpoint := range []string{"https://example.invalid/backend-api/subscriptions", officialSubscriptionsURL + "?account_id=other", officialSubscriptionsURL + "#fragment", "https://user@chatgpt.com/backend-api/subscriptions", "https://chatgpt.com:443/backend-api/subscriptions", "http://127.0.0.1:1234/backend-api/subscriptions"} {
		if validSubscriptionEndpoint(endpoint, officialSubscriptionsURL, false) {
			t.Errorf("unsafe endpoint accepted: %s", endpoint)
		}
	}
	if !validSubscriptionEndpoint(officialSubscriptionsURL, officialSubscriptionsURL, false) {
		t.Fatal("fixed official endpoint rejected")
	}
}

func TestSubscriptionNamedExpiryRejectsCredentialTimestampAndAcceptsNumericTerm(t *testing.T) {
	root := map[string]any{"active_until": json.Number("1964758028"), "exp": json.Number("4070908800")}
	expiry, err := subscriptionNamedExpiry(root, "active_until")
	if err != nil || expiry == nil || expiry.Unix() != 1964758028 {
		t.Fatalf("explicit term rejected: %v", err)
	}
	delete(root, "active_until")
	if expiry, err = subscriptionNamedExpiry(root, "active_until"); err != nil || expiry != nil {
		t.Fatal("credential timestamp became subscription expiry")
	}
}
