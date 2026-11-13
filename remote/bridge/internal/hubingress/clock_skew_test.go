package hubingress

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func signedClockProof(m Manifest, key ed25519.PrivateKey, nonce string, issued int64) proof {
	p := proof{Version: 1, HubInstanceID: m.HubInstanceID, Nonce: nonce, IssuedAt: issued, ExpiresAt: issued + 30000}
	return signClockProof(p, key)
}

func signClockProof(p proof, key ed25519.PrivateKey) proof {
	p.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, []byte(fmt.Sprintf("salcara-ingress-proof-v1\n%s\n%s\n%d\n%d", p.HubInstanceID, p.Nonce, p.IssuedAt, p.ExpiresAt))))
	return p
}

func TestClockSkewManifestRequiresSignedPinnedMembership(t *testing.T) {
	f := newSignedFixture(t)
	now := time.Now()
	for _, name := range []string{"valid", "bad-signature", "wrong-id", "wrong-key", "unlisted-endpoint", "bad-interval", "huge-time", "stale", "future-boundary"} {
		t.Run(name, func(t *testing.T) {
			m, id, key, endpoint := f.m, f.m.HubInstanceID, f.m.IdentityPublicKey, canonicalTest
			m.IssuedAt = now.Add(5 * time.Hour).UnixMilli()
			m.ExpiresAt = m.IssuedAt + 86400000
			switch name {
			case "wrong-id":
				id = strings.Repeat("d", 32)
			case "wrong-key":
				key = base64.StdEncoding.EncodeToString(make([]byte, 32))
			case "unlisted-endpoint":
				endpoint = "https://unlisted.example.com/v1"
			case "bad-interval":
				m.ExpiresAt++
			case "huge-time":
				m.IssuedAt, m.ExpiresAt = 1<<62, 1<<62+86400000
			case "stale":
				m.IssuedAt, m.ExpiresAt = now.Add(-25*time.Hour).UnixMilli(), now.Add(-time.Hour).UnixMilli()
			case "future-boundary":
				m.IssuedAt, m.ExpiresAt = now.Add(5*time.Second).UnixMilli(), now.Add(24*time.Hour+5*time.Second).UnixMilli()
			}
			wire := signManifest(m, f.key)
			if name == "bad-signature" {
				wire.Signature = base64.StdEncoding.EncodeToString(make([]byte, 64))
			}
			verified, err := verifiedManifest(wire, id, key, endpoint, now)
			details := ClockSkew(err)
			if name == "valid" {
				if details == nil || details.Direction != "local_behind" || details.OffsetMS != 18000000 || details.LocalTime != now.UnixMilli() || details.SignedTime != m.IssuedAt || details.Source != "signed_manifest" || verified.HubInstanceID != "" || !ClockSkewMatchesIdentity(err, id, key) {
					t.Fatalf("missing verified clock evidence: %+v %v", details, err)
				}
				details.OffsetMS = 0
				if ClockSkew(err).OffsetMS == 0 {
					t.Fatal("diagnostic aliases original error")
				}
			} else if details != nil {
				t.Fatalf("invalid or stale identity became clock evidence: %v", err)
			} else if name == "future-boundary" && err != nil {
				t.Fatal("existing tolerance changed", err)
			} else if name != "future-boundary" && err == nil {
				t.Fatal("invalid identity accepted")
			}
		})
	}
}

func TestClockSkewProofDistinguishesLatencyAndAuthenticatesEveryField(t *testing.T) {
	f := newSignedFixture(t)
	now, nonce := time.Now().UnixMilli(), strings.Repeat("a", 64)
	for _, name := range []string{"future", "past", "latency-only", "wrong-nonce", "wrong-id", "bad-signature", "bad-interval", "huge-time", "valid", "manifest-expired"} {
		t.Run(name, func(t *testing.T) {
			m, start := f.m, now-100
			p := signedClockProof(m, f.key, nonce, now+18000000)
			switch name {
			case "past":
				p = signedClockProof(m, f.key, nonce, now-18000000)
			case "latency-only":
				start, p = now-60000, signedClockProof(m, f.key, nonce, now-40000)
			case "wrong-nonce":
				p.Nonce = strings.Repeat("b", 64)
				p = signClockProof(p, f.key)
			case "wrong-id":
				p.HubInstanceID = strings.Repeat("b", 32)
				p = signClockProof(p, f.key)
			case "bad-signature":
				p.Signature = base64.StdEncoding.EncodeToString(make([]byte, 64))
			case "bad-interval":
				p.ExpiresAt++
				p = signClockProof(p, f.key)
			case "huge-time":
				p.IssuedAt, p.ExpiresAt = 1<<62, 1<<62+30000
				p = signClockProof(p, f.key)
			case "valid", "manifest-expired":
				p = signedClockProof(m, f.key, nonce, now)
			}
			if name == "manifest-expired" {
				m.ExpiresAt = now
			}
			err := verifiedProof(m, p, nonce, start, now)
			details := ClockSkew(err)
			if name == "future" || name == "past" {
				want := "local_behind"
				if name == "past" {
					want = "local_ahead"
				}
				if details == nil || details.Direction != want || details.Source != "fresh_proof" || details.OffsetMS != p.IssuedAt-now {
					t.Fatalf("missing fresh proof evidence: %+v %v", details, err)
				}
			} else if details != nil {
				t.Fatalf("unauthenticated or delayed proof became clock evidence: %v", err)
			} else if name == "valid" && err != nil {
				t.Fatal(err)
			} else if name != "valid" && err == nil {
				t.Fatal("invalid proof accepted")
			}
		})
	}
}

func TestClockSkewPinnedRequestSendsNoCredentialsAndRecoversWithoutRepinning(t *testing.T) {
	f := newSignedFixture(t)
	f.m.IssuedAt = time.Now().Add(5 * time.Hour).UnixMilli()
	f.m.ExpiresAt = f.m.IssuedAt + 86400000
	tr := New(f, t.TempDir())
	resp, err := tr.RoundTrip(pinnedRequest(t, f, "POST", canonicalTest))
	if resp != nil || ClockSkew(err) == nil || !RequestWasNotSent(err) || len(f.data) != 0 || f.proofs != 0 {
		t.Fatalf("future identity sent credentials: %v", err)
	}
	if _, statErr := os.Stat(tr.cachePath(canonicalTest)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("invalid manifest persisted", statErr)
	}
	f.m.IssuedAt = time.Now().UnixMilli()
	f.m.ExpiresAt = f.m.IssuedAt + 86400000
	resp, err = tr.RoundTrip(pinnedRequest(t, f, "POST", canonicalTest))
	if err != nil || resp == nil || len(f.data) != 1 || f.proofs != 1 {
		t.Fatalf("corrected clock remained cached or identity changed: %v", err)
	}
	resp.Body.Close()
}

func TestClockSkewCandidateDoesNotBlockHealthyAlternative(t *testing.T) {
	for _, stage := range []string{"manifest", "proof"} {
		t.Run(stage, func(t *testing.T) {
			f := newSignedFixture(t)
			var clockResponses atomic.Int32
			base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "hub.example.com" {
					if stage == "manifest" && strings.HasSuffix(r.URL.Path, "/manifest") {
						m := f.m
						m.IssuedAt = time.Now().Add(5 * time.Hour).UnixMilli()
						m.ExpiresAt = m.IssuedAt + 86400000
						clockResponses.Add(1)
						return response(signManifest(m, f.key), 200), nil
					}
					if stage == "proof" && strings.HasSuffix(r.URL.Path, "/identity") {
						clockResponses.Add(1)
						return response(signedClockProof(f.m, f.key, r.URL.Query().Get("nonce"), time.Now().Add(-5*time.Hour).UnixMilli()), 200), nil
					}
				}
				return f.RoundTrip(r)
			})
			tr := New(base, "")
			// Seed only an already valid signed hint so canonical failure can
			// discover the backup without trusting the future manifest.
			e := tr.acquireState(canonicalTest)
			e.manifest = signManifest(f.m, f.key)
			tr.releaseState(e)
			resp, err := tr.RoundTrip(pinnedRequest(t, f, "POST", canonicalTest))
			if err != nil || resp == nil || clockResponses.Load() == 0 || len(f.data) != 1 || stage == "proof" && !strings.Contains(f.data[0], "edge.example.com") {
				t.Fatalf("healthy signed alternative blocked: %v data=%v", err, f.data)
			}
			resp.Body.Close()
			if _, _, err := tr.ResolvePinnedIdentity(context.Background(), canonicalTest, f.m.HubInstanceID, f.m.IdentityPublicKey, backupTest); err != nil {
				t.Fatal("successful alternative poisoned cache", err)
			}
		})
	}
}

func TestClockSkewLateProofCannotPoisonSuccessfulConnection(t *testing.T) {
	f := newSignedFixture(t)
	f.m.Endpoints = f.m.Endpoints[:1]
	entered, release := make(chan struct{}), make(chan struct{})
	var proofs atomic.Int32
	tr := New(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/identity") && proofs.Add(1) == 1 {
			close(entered)
			<-release
			return response(signedClockProof(f.m, f.key, r.URL.Query().Get("nonce"), time.Now().Add(-5*time.Hour).UnixMilli()), 200), nil
		}
		return f.RoundTrip(r)
	}), "")
	oldDone := make(chan error, 1)
	go func() { _, err := tr.RoundTrip(pinnedRequest(t, f, "POST", canonicalTest)); oldDone <- err }()
	<-entered
	resp, err := tr.RoundTrip(pinnedRequest(t, f, "POST", canonicalTest))
	close(release)
	if err != nil || resp == nil {
		t.Fatal("new connection blocked by old proof", err)
	}
	resp.Body.Close()
	if err := <-oldDone; ClockSkew(err) == nil || !RequestWasNotSent(err) {
		t.Fatal("old request lost non-delivery evidence", err)
	}
	e := tr.acquireState(canonicalTest)
	e.mu.Lock()
	poisoned := e.clockSkew != nil
	e.mu.Unlock()
	tr.releaseState(e)
	if poisoned {
		t.Fatal("late proof wrote shared cached clock failure")
	}
	resp, err = tr.RoundTrip(pinnedRequest(t, f, "POST", canonicalTest))
	if err != nil || resp == nil || len(f.data) != 2 {
		t.Fatal("old proof poisoned next credential request", err)
	}
	resp.Body.Close()
}

func TestClockSkewAheadRecoveryIgnoresFutureFailureDeadlines(t *testing.T) {
	f := newSignedFixture(t)
	var skew atomic.Bool
	skew.Store(true)
	tr := New(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if skew.Load() && strings.HasSuffix(r.URL.Path, "/identity") {
			return response(signedClockProof(f.m, f.key, r.URL.Query().Get("nonce"), time.Now().Add(-5*time.Hour).UnixMilli()), 200), nil
		}
		return f.RoundTrip(r)
	}), "")
	_, err := tr.RoundTrip(pinnedRequest(t, f, "POST", canonicalTest))
	if details := ClockSkew(err); details == nil || details.Direction != "local_ahead" || !RequestWasNotSent(err) {
		t.Fatal("ahead fixture did not fail safely", err)
	}
	// Simulate retained deadlines after moving wall time backwards, without
	// touching the computer clock. Failure marks order routes, never skip them.
	e := tr.acquireState(canonicalTest)
	e.mu.Lock()
	e.refreshAt = time.Now().Add(5 * time.Hour)
	for _, endpoint := range f.m.Endpoints {
		e.failed[endpoint.URL] = time.Now().Add(5 * time.Hour)
	}
	e.mu.Unlock()
	tr.releaseState(e)
	skew.Store(false)
	resp, err := tr.RoundTrip(pinnedRequest(t, f, "POST", canonicalTest))
	if err != nil || resp == nil || len(f.data) != 1 {
		t.Fatal("clock correction trapped behind future cached deadlines", err)
	}
	resp.Body.Close()
}

func TestClockSkewNewPinDiagnosticSurvivesOldDataSuccess(t *testing.T) {
	old := newSignedFixture(t)
	next := newSignedFixture(t)
	next.key = ed25519.NewKeyFromSeed([]byte(strings.Repeat("b", 32)))
	next.m.HubInstanceID = strings.Repeat("b", 32)
	next.m.IdentityPublicKey = base64.StdEncoding.EncodeToString(next.key.Public().(ed25519.PublicKey))
	next.m.IssuedAt = time.Now().Add(5 * time.Hour).UnixMilli()
	next.m.ExpiresAt = next.m.IssuedAt + 86400000
	entered, release := make(chan struct{}), make(chan struct{})
	var changed atomic.Bool
	tr := New(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/ingress/") {
			if changed.Load() {
				return next.RoundTrip(r)
			}
			return old.RoundTrip(r)
		}
		close(entered)
		<-release
		return old.RoundTrip(r)
	}), "")
	done := make(chan error, 1)
	go func() {
		resp, err := tr.RoundTrip(pinnedRequest(t, old, "POST", canonicalTest))
		if resp != nil {
			resp.Body.Close()
		}
		done <- err
	}()
	<-entered
	changed.Store(true)
	_, err := tr.RoundTrip(pinnedRequest(t, next, "POST", canonicalTest))
	close(release)
	if ClockSkew(err) == nil || !RequestWasNotSent(err) {
		t.Fatal("new pin did not produce authenticated diagnosis", err)
	}
	if err := <-done; err != nil {
		t.Fatal("old request did not complete", err)
	}
	e := tr.acquireState(canonicalTest)
	e.mu.Lock()
	retained := ClockSkewMatchesIdentity(e.clockSkew, next.m.HubInstanceID, next.m.IdentityPublicKey)
	e.mu.Unlock()
	tr.releaseState(e)
	if !retained {
		t.Fatal("old data success erased newer pin clock diagnosis")
	}
}

func TestClockSkewCannotDowngradeSignedIdentityReplacement(t *testing.T) {
	for _, method := range []string{"resolve", "POST"} {
		t.Run(method, func(t *testing.T) {
			f := newSignedFixture(t)
			tr := New(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/manifest") {
					if r.URL.Host == "hub.example.com" {
						m := f.m
						key := ed25519.NewKeyFromSeed([]byte(strings.Repeat("b", 32)))
						m.HubInstanceID, m.IdentityPublicKey = strings.Repeat("c", 32), base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
						return response(signManifest(m, key), 200), nil
					}
					return nil, errors.New("synthetic manifest unavailable")
				}
				if strings.HasSuffix(r.URL.Path, "/identity") {
					return response(signedClockProof(f.m, f.key, r.URL.Query().Get("nonce"), time.Now().Add(-5*time.Hour).UnixMilli()), 200), nil
				}
				return f.RoundTrip(r)
			}), "")
			e := tr.acquireState(canonicalTest)
			e.manifest = signManifest(f.m, f.key)
			tr.releaseState(e)
			var err error
			if method == "resolve" {
				_, _, err = tr.ResolvePinnedIdentity(context.Background(), canonicalTest, f.m.HubInstanceID, f.m.IdentityPublicKey, canonicalTest)
			} else {
				_, err = tr.RoundTrip(pinnedRequest(t, f, method, canonicalTest))
			}
			if !errors.Is(err, ErrIdentityChanged) || ClockSkew(err) != nil || len(f.data) != 0 {
				t.Fatal("clock evidence downgraded signed replacement", err)
			}
		})
	}
}
