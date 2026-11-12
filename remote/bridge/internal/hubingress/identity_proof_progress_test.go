package hubingress

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestPinnedProofProgressSurvivesReconnectCooldown(t *testing.T) {
	for _, mode := range []string{"resolve", "post"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			f := newSignedFixture(t)
			const slowOne = "https://slow-one.example.com/v1"
			const slowTwo = "https://slow-two.example.com/v1"
			const healthy = "https://healthy.example.com/v1"
			f.m.Endpoints = []Endpoint{
				{ID: "canonical", URL: canonicalTest, Priority: 0},
				{ID: strings.Repeat("1", 32), URL: slowOne, Priority: 1},
				{ID: strings.Repeat("2", 32), URL: slowTwo, Priority: 2},
				{ID: strings.Repeat("3", 32), URL: healthy, Priority: 3},
			}
			var proofs []string
			tr := New(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/identity") {
					if r.Header.Get("Authorization") != "" || r.Header.Get("X-Salcara-Device-Secret") != "" {
						t.Error("proof carried device credentials")
					}
					proofs = append(proofs, r.URL.Host)
					if r.URL.Host != "healthy.example.com" {
						timer := time.NewTimer(100 * time.Millisecond)
						defer timer.Stop()
						select {
						case <-timer.C:
							return nil, errors.New("synthetic slow proof failure")
						case <-r.Context().Done():
							return nil, r.Context().Err()
						}
					}
				}
				return f.RoundTrip(r)
			}), "")
			e := tr.state(canonicalTest)
			e.manifest = signManifest(f.m, f.key)
			e.refreshPin, e.refreshAt = f.m.HubInstanceID+":"+f.m.IdentityPublicKey, time.Now().Add(time.Hour)
			resolve := func(ctx context.Context) error {
				if mode == "resolve" {
					_, _, err := tr.ResolvePinnedIdentity(ctx, canonicalTest, f.m.HubInstanceID, f.m.IdentityPublicKey, canonicalTest)
					return err
				}
				response, err := tr.RoundTrip(pinnedLatencyRequest(t, f, ctx))
				if response != nil {
					response.Body.Close()
				}
				return err
			}
			// A shorter caller budget reproduces the same exhausted-flight branch
			// without sleeping for 15 seconds or the reconnect's 60 second pause.
			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()
			if err := resolve(ctx); err == nil || len(f.data) != 0 || e.proofNext != slowTwo {
				t.Fatal("partial proof did not retain progress or sent credentials", err, e.proofNext, f.data)
			}
			if got := strings.Join(proofs, ","); got != "hub.example.com,slow-one.example.com,slow-two.example.com" {
				t.Fatal("unexpected first proof order", got)
			}
			// Model the normal reconnect after all failure marks expired. Force
			// the usual manifest refresh too; same signed membership keeps progress.
			for endpoint := range e.failed {
				e.failed[endpoint] = time.Now().Add(-time.Minute)
			}
			e.refreshAt = time.Now().Add(-time.Minute)
			if mode == "post" {
				f.dataError = true
			}
			err := resolve(context.Background())
			if mode == "resolve" && err != nil || mode == "post" && (err == nil || len(f.data) != 1) {
				t.Fatal("healthy fourth candidate was starved or POST replayed", err, f.data)
			}
			if got := strings.Join(proofs, ","); got != "hub.example.com,slow-one.example.com,slow-two.example.com,slow-two.example.com,healthy.example.com" || e.proofNext != "" {
				t.Fatal("reconnect restarted exhausted prefix or failed to reset progress", got, e.proofNext)
			}
		})
	}
}

func TestPinnedProofProgressCannotRetainRemovedMemberOrRedirectExplicitProbe(t *testing.T) {
	f := newSignedFixture(t)
	removed := backupTest
	var proofs []string
	tr := New(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/identity") {
			proofs = append(proofs, r.URL.Host)
		}
		return f.RoundTrip(r)
	}), "")
	e := tr.state(canonicalTest)
	e.manifest, e.proofNext = signManifest(f.m, f.key), removed
	f.m.Revision++
	f.m.Endpoints = f.m.Endpoints[:1]
	if _, _, err := tr.ResolvePinnedIdentity(context.Background(), canonicalTest, f.m.HubInstanceID, f.m.IdentityPublicKey, canonicalTest); err != nil || strings.Join(proofs, ",") != "hub.example.com" || e.proofNext != "" {
		t.Fatal("removed member bypassed refreshed signed membership", err, proofs, e.proofNext)
	}
	// An explicit endpoint check never follows the automatic proof cursor.
	f.m.Endpoints = append(f.m.Endpoints, Endpoint{ID: strings.Repeat("b", 32), URL: backupTest, Priority: 1})
	f.m.Revision++
	e.proofNext = backupTest
	proofs = nil
	if _, err := tr.VerifyPinnedEndpoint(context.Background(), canonicalTest, f.m.HubInstanceID, f.m.IdentityPublicKey, canonicalTest); err != nil || strings.Join(proofs, ",") != "hub.example.com" || e.proofNext != "" {
		t.Fatal("explicit endpoint redirected or did not reset completed progress", err, proofs, e.proofNext)
	}
}

func TestPinnedProofProgressCancellationDoesNotSendCredentials(t *testing.T) {
	f := newSignedFixture(t)
	entered := make(chan struct{})
	tr := New(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/identity") || r.Header.Get("Authorization") != "" || r.Header.Get("X-Salcara-Device-Secret") != "" {
			t.Error("cancelled proof reached credential delivery")
		}
		close(entered)
		<-r.Context().Done()
		return nil, r.Context().Err()
	}), "")
	e := tr.state(canonicalTest)
	e.manifest = signManifest(f.m, f.key)
	e.refreshPin, e.refreshAt = f.m.HubInstanceID+":"+f.m.IdentityPublicKey, time.Now().Add(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := tr.RoundTrip(pinnedLatencyRequest(t, f, ctx))
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("fresh proof did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || e.proofNext != canonicalTest || len(f.data) != 0 {
			t.Fatal("cancelled candidate lost progress or sent data", err, e.proofNext, f.data)
		}
	case <-time.After(time.Second):
		t.Fatal("proof cancellation waited for the full budget")
	}
}
