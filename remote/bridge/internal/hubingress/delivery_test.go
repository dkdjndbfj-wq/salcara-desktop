package hubingress

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestPinnedDeliveryClassificationNeverReplaysMutation(t *testing.T) {
	for _, phase := range []string{"identity", "identity-cancelled", "data", "nested-before-send"} {
		t.Run(phase, func(t *testing.T) {
			f := newSignedFixture(t)
			sent := 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tr := New(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if strings.Contains(r.URL.Path, "/ingress/") {
					if phase == "identity" {
						return nil, errors.New("synthetic identity unavailable")
					}
					if phase == "identity-cancelled" {
						cancel()
						return nil, ctx.Err()
					}
					return f.RoundTrip(r)
				}
				sent++
				if phase == "nested-before-send" {
					return nil, &requestNotSentError{cause: errors.New("nested transport refused")}
				}
				return nil, errors.New("synthetic reply lost after dispatch")
			}), "")
			client := &http.Client{Transport: tr, Timeout: time.Second}
			_, err := client.Do(pinnedLatencyRequest(t, f, ctx))
			wantUnsent := strings.HasPrefix(phase, "identity")
			if err == nil || RequestWasNotSent(err) != wantUnsent {
				t.Fatalf("phase %s: delivery classification wrong: %v", phase, err)
			}
			wantSent := 1
			if wantUnsent {
				wantSent = 0
			}
			if sent != wantSent {
				t.Fatalf("mutation sends=%d want=%d", sent, wantSent)
			}
		})
	}
	if RequestWasNotSent(&url.Error{Op: "Post", URL: "https://synthetic.invalid", Err: context.DeadlineExceeded}) {
		t.Fatal("ordinary deadline was treated as proof of non-delivery")
	}
}
