package relay

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestUsageRejectsRedirectBeforeFollowingCredential(t *testing.T) {
	for _, days := range []bool{false, true} {
		name := "usage"
		if days {
			name = "usage_days"
		}
		for _, status := range []int{302, 307} {
			t.Run(name+http.StatusText(status), func(t *testing.T) {
				var calls atomic.Int32
				trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					_, _ = io.WriteString(w, `{"balance":77}`)
				}))
				defer trap.Close()
				source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Location", trap.URL+"/credential-trap")
					w.WriteHeader(status)
				}))
				defer source.Close()
				var err error
				if days {
					_, err = UsageDays(context.Background(), source.URL, "synthetic-usage-key", 14)
				} else {
					_, err = Usage(context.Background(), source.URL, "synthetic-usage-key")
				}
				if err == nil || calls.Load() != 0 {
					t.Fatalf("usage followed provider redirect: calls=%d error=%v", calls.Load(), err)
				}
			})
		}
	}
}

func TestUsageRejectsOversizedNullAndInterruptedBody(t *testing.T) {
	for _, days := range []bool{false, true} {
		name := "usage"
		limit := 2 << 20
		if days {
			name = "usage_days"
			limit = 4 << 20
		}
		for _, body := range []struct{ name, value string }{
			{"oversize", `{"balance":77}` + strings.Repeat(" ", limit)},
			{"null", "null"},
			{"interrupted", `{"balance":77}`},
		} {
			t.Run(name+"_"+body.name, func(t *testing.T) {
				source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if body.name == "interrupted" {
						w.Header().Set("Content-Length", "10000")
					}
					_, _ = io.WriteString(w, body.value)
				}))
				defer source.Close()
				var err error
				if days {
					_, err = UsageDays(context.Background(), source.URL, "synthetic-usage-key", 14)
				} else {
					_, err = Usage(context.Background(), source.URL, "synthetic-usage-key")
				}
				if err == nil {
					t.Fatal("incomplete/oversized/null provider body returned success")
				}
			})
		}
	}
}

func TestUsageFixedEndpointPreservesValidZeroAndRejectedKey(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/usage" {
			t.Error("unexpected usage endpoint")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") == "Bearer synthetic-rejected-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "Bearer synthetic-valid-key" {
			t.Error("usage did not send the selected credential")
		}
		if q := r.URL.Query().Get("days"); q != "" && q != "14" {
			t.Error("usage lost the requested history window")
		}
		_, _ = io.WriteString(w, `{"balance":0,"daily_usage":[{"date":"2026-10-11","tokens":5,"actual_cost":0}]}`)
	}))
	defer provider.Close()
	for _, days := range []bool{false, true} {
		var result map[string]any
		var err error
		if days {
			result, err = UsageDays(context.Background(), provider.URL, "synthetic-valid-key", 14)
		} else {
			result, err = Usage(context.Background(), provider.URL, "synthetic-valid-key")
		}
		if err != nil || result["balance"] != float64(0) {
			t.Fatalf("valid usage failed: result=%v error=%v", result, err)
		}
		if days {
			_, err = UsageDays(context.Background(), provider.URL, "synthetic-rejected-key", 14)
		} else {
			_, err = Usage(context.Background(), provider.URL, "synthetic-rejected-key")
		}
		if !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("invalid-key semantics changed: %v", err)
		}
	}
}
