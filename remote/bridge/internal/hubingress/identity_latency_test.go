package hubingress

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func delayedIdentityFixture(f *signedFixture, delay time.Duration) http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/ingress/") {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		return f.RoundTrip(r)
	})
}

func TestIdentityAllowsOrdinaryWANLatencyWithoutReplayingMutation(t *testing.T) {
	// Each anonymous response exceeds the old 3s client/1s pinned probe limits;
	// the full exchange also exceeds the old 6s recovery flight.
	for _, mode := range []string{"anonymous", "pinned", "explicit-endpoint", "pinned-post"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			f := newSignedFixture(t)
			tr := New(delayedIdentityFixture(f, 3200*time.Millisecond), "")
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			var err error
			switch mode {
			case "anonymous":
				_, err = tr.ResolveIdentity(ctx, canonicalTest)
			case "pinned":
				_, _, err = tr.ResolvePinnedIdentity(ctx, canonicalTest, f.m.HubInstanceID, f.m.IdentityPublicKey, canonicalTest)
			case "explicit-endpoint":
				_, err = tr.VerifyPinnedEndpoint(ctx, canonicalTest, f.m.HubInstanceID, f.m.IdentityPublicKey, canonicalTest)
			case "pinned-post":
				// The peer may accept this mutation and lose its reply. Longer
				// identity checks must not authorize an automatic second send.
				f.dataError = true
				_, err = tr.RoundTrip(pinnedLatencyRequest(t, f, ctx))
				if err == nil || len(f.data) != 1 {
					t.Fatalf("mutation delivery changed: err=%v sent=%v", err, f.data)
				}
				return
			}
			if err != nil || f.proofs != 1 || len(f.data) != 0 {
				t.Fatalf("ordinary latency rejected or credential sent: %v proofs=%d data=%v", err, f.proofs, f.data)
			}
		})
	}
}

// Preserve the production identity bindings while supplying a caller deadline.
func pinnedLatencyRequest(t *testing.T, f *signedFixture, ctx context.Context) *http.Request {
	t.Helper()
	req := pinnedRequest(t, f, "POST", canonicalTest)
	return BindIdentity(Bind(req.WithContext(ctx), canonicalTest), f.m.HubInstanceID, f.m.IdentityPublicKey, canonicalTest)
}

func TestIdentityLongerBudgetStillRejectsBadSignature(t *testing.T) {
	t.Parallel()
	f := newSignedFixture(t)
	f.mutate = func(m Manifest) Manifest { m.Signature = "invalid"; return m }
	tr := New(delayedIdentityFixture(f, 3200*time.Millisecond), "")
	if _, err := tr.ResolveIdentity(context.Background(), canonicalTest); err == nil || f.proofs != 0 || len(f.data) != 0 {
		t.Fatalf("invalid signature accepted: %v proofs=%d data=%v", err, f.proofs, f.data)
	}
}

func TestIdentityLongerBudgetHonorsCallerCancellation(t *testing.T) {
	for _, mode := range []string{"anonymous", "pinned", "explicit-endpoint", "pinned-post"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			f := newSignedFixture(t)
			entered := make(chan struct{})
			tr := New(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if !strings.Contains(r.URL.Path, "/ingress/") || r.Header.Get("Authorization") != "" || r.Header.Get("X-Salcara-Device-Secret") != "" {
					t.Error("canceled identity flight reached credential delivery")
				}
				close(entered)
				<-r.Context().Done()
				return nil, r.Context().Err()
			}), "")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				var err error
				switch mode {
				case "anonymous":
					_, err = tr.ResolveIdentity(ctx, canonicalTest)
				case "pinned":
					_, _, err = tr.ResolvePinnedIdentity(ctx, canonicalTest, f.m.HubInstanceID, f.m.IdentityPublicKey, canonicalTest)
				case "explicit-endpoint":
					_, err = tr.VerifyPinnedEndpoint(ctx, canonicalTest, f.m.HubInstanceID, f.m.IdentityPublicKey, canonicalTest)
				case "pinned-post":
					_, err = tr.RoundTrip(pinnedLatencyRequest(t, f, ctx))
				}
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("anonymous request did not start")
			}
			cancel()
			select {
			case err := <-done:
				if err == nil || f.proofs != 0 || len(f.data) != 0 {
					t.Fatalf("cancellation accepted or dispatched data: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("caller cancellation waited for identity budget")
			}
		})
	}
}

func TestPinnedRefreshResumesCandidateWithExhaustedFlightBudget(t *testing.T) {
	f := newSignedFixture(t)
	const pinEndpoint = "https://pin.example.com/salcara-hub/v1"
	var manifests []string
	tr := New(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/manifest") {
			manifests = append(manifests, r.URL.Host)
			if r.URL.Host != "edge.example.com" {
				return nil, errors.New("synthetic retired anchor")
			}
			timer := time.NewTimer(200 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		return f.RoundTrip(r)
	}), "")
	tr.state(canonicalTest).manifest = expiredSignedManifest(f, f.m.Revision)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := tr.ResolvePinnedIdentity(ctx, canonicalTest, f.m.HubInstanceID, f.m.IdentityPublicKey, pinEndpoint); err == nil {
		t.Fatal("flight did not exhaust the final candidate's partial budget")
	}
	if got := strings.Join(manifests, ","); got != "pin.example.com,hub.example.com,edge.example.com" {
		t.Fatal("unexpected first probe order", got)
	}
	_, endpoint, err := tr.ResolvePinnedIdentity(context.Background(), canonicalTest, f.m.HubInstanceID, f.m.IdentityPublicKey, pinEndpoint)
	if err != nil || endpoint != backupTest || strings.Join(manifests, ",") != "pin.example.com,hub.example.com,edge.example.com,edge.example.com" || f.proofs != 1 || len(f.data) != 0 {
		t.Fatal("partial candidate was not resumed with a full budget", endpoint, err, manifests, f.proofs, f.data)
	}
}
