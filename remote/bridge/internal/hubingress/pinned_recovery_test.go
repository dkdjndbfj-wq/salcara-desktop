package hubingress

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func expiredSignedManifest(f *signedFixture, revision int64) Manifest {
	m := f.m
	m.Revision = revision
	m.IssuedAt = time.Now().Add(-25 * time.Hour).UnixMilli()
	m.ExpiresAt = m.IssuedAt + 86400000
	return signManifest(m, f.key)
}

func TestPinnedExpiredCacheRecoversAfterRestartWithoutChangingLogicalConnection(t *testing.T) {
	f := newSignedFixture(t)
	request := pinnedRequest(t, f, "POST", canonicalTest)
	logical := request.URL.String()
	dir := t.TempDir()
	New(f, dir).persistAs(canonicalTest, expiredSignedManifest(f, 7))
	// The old logical address is no longer a member. It still anchors local
	// credentials/history; only fresh signed members can receive the request.
	f.m.CanonicalURL = "https://new.example.com/salcara-hub/v1"
	f.m.Endpoints[0].URL = f.m.CanonicalURL
	f.m.Revision = 8
	var manifests []string
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/manifest") {
			manifests = append(manifests, r.URL.Host)
		}
		if r.URL.Host == "hub.example.com" {
			return nil, errors.New("old address offline")
		}
		return f.RoundTrip(r)
	})
	tr := New(base, dir)
	resp, err := tr.RoundTrip(request)
	if err != nil {
		t.Fatal("signed expired backup hint was lost on restart", err)
	}
	resp.Body.Close()
	if request.URL.String() != logical || len(f.data) != 1 || !strings.Contains(f.data[0], backupTest+"/bridge/events?") {
		t.Fatal("migration changed the logical request or sent to a removed member", f.data)
	}
	if strings.Join(manifests, ",") != "hub.example.com,edge.example.com" || f.proofs != 1 {
		t.Fatal("recovery did not anonymously refresh then prove the live member", manifests, f.proofs)
	}
	cached := New(base, dir).state(canonicalTest).manifest
	if cached.Revision != 8 || cached.CanonicalURL != f.m.CanonicalURL {
		t.Fatal("new manifest was not retained under the original connection")
	}
}

func TestPinnedExpiredHintsCannotAuthorizeTrafficOrRollBackRevision(t *testing.T) {
	for _, mode := range []string{"still-expired", "rollback", "unsigned-cache", "tampered-cache"} {
		t.Run(mode, func(t *testing.T) {
			f := newSignedFixture(t)
			dir := t.TempDir()
			cached := expiredSignedManifest(f, 20)
			if mode == "unsigned-cache" {
				cached.SignedPayload, cached.Signature = "", ""
			}
			if mode == "tampered-cache" {
				cached.Signature = base64.StdEncoding.EncodeToString(make([]byte, 64))
			}
			New(f, dir).persistAs(canonicalTest, cached)
			if mode == "still-expired" {
				f.m.IssuedAt, f.m.ExpiresAt = cached.IssuedAt, cached.ExpiresAt
			}
			f.m.Revision = 19
			var probes []string
			base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				probes = append(probes, r.URL.Host+r.URL.Path)
				if r.URL.Host == "hub.example.com" {
					return nil, errors.New("old address offline")
				}
				return f.RoundTrip(r)
			})
			tr := New(base, dir)
			for attempt := 0; attempt < 2; attempt++ {
				before := len(probes)
				resp, err := tr.RoundTrip(pinnedRequest(t, f, "POST", canonicalTest))
				if resp != nil {
					resp.Body.Close()
				}
				if err == nil || f.proofs != 0 || len(f.data) != 0 {
					t.Fatal("stale or untrusted cache authorized credential traffic")
				}
				if attempt == 1 && len(probes) != before {
					t.Fatal("failed refresh bypassed the cooldown", probes)
				}
			}
			want := 2
			if mode == "unsigned-cache" || mode == "tampered-cache" {
				want = 1
			}
			if len(probes) != want {
				t.Fatal("unverified cache was used as a locator", probes)
			}
		})
	}
}

func TestPinnedRecoverySkipsReplacedPrimaryAndRequiresFreshProof(t *testing.T) {
	f := newSignedFixture(t)
	foreign := newSignedFixture(t)
	foreign.reset = true
	dir := t.TempDir()
	New(f, dir).persistAs(canonicalTest, expiredSignedManifest(f, 1))
	tr := New(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "hub.example.com" {
			return foreign.RoundTrip(r)
		}
		return f.RoundTrip(r)
	}), dir)
	m, endpoint, err := tr.ResolvePinnedIdentity(context.Background(), canonicalTest, f.m.HubInstanceID, f.m.IdentityPublicKey, canonicalTest)
	if err != nil || endpoint != backupTest || m.HubInstanceID != f.m.HubInstanceID || len(foreign.data) != 0 || len(f.data) != 0 || f.proofs != 1 {
		t.Fatal("a replaced candidate prevented recovery through the genuine Hub", endpoint, err)
	}
	f.replay = true
	if _, _, err := tr.ResolvePinnedIdentity(context.Background(), canonicalTest, f.m.HubInstanceID, f.m.IdentityPublicKey, canonicalTest); err == nil || errors.Is(err, ErrIdentityChanged) {
		t.Fatal("proof failure was accepted or retained a resolved replacement alarm", err)
	}
}

func TestPinnedRefreshIsCoalescedAndWaitingRequestCanCancel(t *testing.T) {
	f := newSignedFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var count atomic.Int32
	tr := New(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/manifest") {
			if count.Add(1) == 1 {
				close(entered)
			}
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		return f.RoundTrip(r)
	}), "")
	resolve := func(ctx context.Context) error {
		_, _, err := tr.ResolvePinnedIdentity(ctx, canonicalTest, f.m.HubInstanceID, f.m.IdentityPublicKey, canonicalTest)
		return err
	}
	first := make(chan error, 1)
	go func() { first <- resolve(context.Background()) }()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	second := make(chan error, 1)
	go func() { second <- resolve(ctx) }()
	cancel()
	select {
	case err := <-second:
		if err == nil {
			t.Fatal("cancelled waiter succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled waiter was blocked by another network request")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := resolve(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if count.Load() != 1 || f.proofs != 9 || len(f.data) != 0 {
		t.Fatal("refresh was duplicated or proofs were shared", count.Load(), f.proofs)
	}
}

func TestPinnedRecoveryDistinguishesReplacedInstanceFromOffline(t *testing.T) {
	for _, replaced := range []bool{false, true} {
		f := newSignedFixture(t)
		id, key := f.m.HubInstanceID, f.m.IdentityPublicKey
		f.reset = replaced
		tr := New(roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if !replaced {
				return nil, errors.New("network offline")
			}
			return f.RoundTrip(r)
		}), "")
		_, _, err := tr.ResolvePinnedIdentity(context.Background(), canonicalTest, id, key, canonicalTest)
		if err == nil || errors.Is(err, ErrIdentityChanged) != replaced || len(f.data) != 0 {
			t.Fatalf("wrong recovery outcome: replaced=%v err=%v", replaced, err)
		}
	}
}

func TestPinnedRefreshCandidateLimitAndExpiryDeadline(t *testing.T) {
	f := newSignedFixture(t)
	for i := 0; i < 6; i++ {
		f.m.Endpoints = append(f.m.Endpoints, Endpoint{ID: fmt.Sprintf("%032x", i+1), URL: fmt.Sprintf("https://backup%d.example.com/v1", i)})
	}
	dir := t.TempDir()
	New(f, dir).persistAs(canonicalTest, expiredSignedManifest(f, 1))
	var probes int
	tr := New(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		probes++
		return nil, errors.New("offline")
	}), dir)
	_, _, err := tr.ResolvePinnedIdentity(context.Background(), canonicalTest, f.m.HubInstanceID, f.m.IdentityPublicKey, "https://last.example.com/v1")
	if err == nil || probes != 8 {
		t.Fatal("locator set is not bounded to eight candidates", probes, err)
	}

	f = newSignedFixture(t)
	f.m.IssuedAt = time.Now().Add(-24*time.Hour + time.Minute).UnixMilli()
	f.m.ExpiresAt = f.m.IssuedAt + 86400000
	tr = New(f, "")
	if _, _, err := tr.ResolvePinnedIdentity(context.Background(), canonicalTest, f.m.HubInstanceID, f.m.IdentityPublicKey, canonicalTest); err != nil {
		t.Fatal(err)
	}
	e := tr.state(canonicalTest)
	if e.refreshAt.After(time.UnixMilli(f.m.ExpiresAt)) {
		t.Fatal("refresh delay outlives the signed authorization")
	}
}

func TestPinnedRefreshRotatesOldAnchorsToReachTheLastSignedBackup(t *testing.T) {
	f := newSignedFixture(t)
	f.m.CanonicalURL = "https://current.example.com/salcara-hub/v1"
	f.m.Endpoints = []Endpoint{{ID: "canonical", URL: f.m.CanonicalURL, Priority: 100}}
	for i := 1; i <= 7; i++ {
		f.m.Endpoints = append(f.m.Endpoints, Endpoint{ID: fmt.Sprintf("%032x", i), URL: fmt.Sprintf("https://edge%d.example.com/salcara-hub/v1", i), Priority: 100 + i})
	}
	dir := t.TempDir()
	New(f, dir).persistAs(canonicalTest, expiredSignedManifest(f, 1))
	f.m.Revision = 2
	var manifests []string
	tr := New(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/manifest") {
			manifests = append(manifests, r.URL.Host)
		}
		if r.URL.Host != "edge7.example.com" {
			return nil, errors.New("obsolete endpoint offline")
		}
		return f.RoundTrip(r)
	}), dir)
	for attempt := 0; attempt < 3; attempt++ {
		before := len(manifests)
		_, endpoint, err := tr.ResolvePinnedIdentity(context.Background(), canonicalTest, f.m.HubInstanceID, f.m.IdentityPublicKey, "https://pin.example.com/salcara-hub/v1")
		if len(manifests)-before > 8 {
			t.Fatal("refresh exceeded its candidate budget", manifests)
		}
		if err == nil {
			if endpoint != "https://edge7.example.com/salcara-hub/v1" {
				t.Fatal("wrong recovered endpoint", endpoint)
			}
			return
		}
		e := tr.state(canonicalTest)
		e.mu.Lock()
		e.refreshAt = time.Time{} // advance past the production failure cooldown
		e.mu.Unlock()
	}
	t.Fatal("a reachable signed backup was starved by old logical anchors", manifests)
}

func TestExplicitPinnedEndpointRejectsRollbackAndDoesNotTryOtherAddresses(t *testing.T) {
	f := newSignedFixture(t)
	dir := t.TempDir()
	New(f, dir).persistAs(canonicalTest, expiredSignedManifest(f, 10))
	var addresses []string
	tr := New(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		addresses = append(addresses, r.URL.Host)
		return f.RoundTrip(r)
	}), dir)
	f.m.Revision = 9
	if _, err := tr.VerifyPinnedEndpoint(context.Background(), canonicalTest, f.m.HubInstanceID, f.m.IdentityPublicKey, backupTest); err == nil || f.proofs != 0 {
		t.Fatal("explicit address update rolled back an expired signed revision", err)
	}
	if tr.state(canonicalTest).manifest.Revision != 10 || strings.Join(addresses, ",") != "edge.example.com" {
		t.Fatal("failed explicit update changed the cache or used a different address", addresses)
	}
	f.m.Revision = 11
	addresses = nil
	m, err := tr.VerifyPinnedEndpoint(context.Background(), canonicalTest, f.m.HubInstanceID, f.m.IdentityPublicKey, backupTest)
	if err != nil || m.Revision != 11 || f.proofs != 1 || len(f.data) != 0 || strings.Join(addresses, ",") != "edge.example.com,edge.example.com" {
		t.Fatal("explicit address was not anonymously proved", addresses, err)
	}
	if got := New(f, dir).state(canonicalTest).manifest; got.Revision != 11 {
		t.Fatal("new endpoint did not preserve the highest revision across restart")
	}
	addresses = nil
	if _, err := tr.VerifyPinnedEndpoint(context.Background(), canonicalTest, f.m.HubInstanceID, f.m.IdentityPublicKey, "https://unlisted.example.com/v1"); err == nil || f.proofs != 1 {
		t.Fatal("unlisted explicit address was authorized or replaced by a known address", err)
	}
	if strings.Join(addresses, ",") != "unlisted.example.com" {
		t.Fatal("explicit address verification silently tried another address", addresses)
	}
}

func TestExplicitPinnedEndpointWaitsForAutomaticRefreshWithoutRollback(t *testing.T) {
	f := newSignedFixture(t)
	newAddress := "https://new.example.com/salcara-hub/v1"
	f.m.Endpoints = append(f.m.Endpoints, Endpoint{ID: strings.Repeat("c", 32), URL: newAddress})
	older := signManifest(f.m, f.key)
	f.m.Revision = 2
	entered, release := make(chan struct{}), make(chan struct{})
	tr := New(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/manifest") && r.URL.Host == "hub.example.com" {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
			return response(older, http.StatusOK), nil
		}
		return f.RoundTrip(r)
	}), t.TempDir())
	automatic := make(chan error, 1)
	go func() {
		_, _, err := tr.ResolvePinnedIdentity(context.Background(), canonicalTest, f.m.HubInstanceID, f.m.IdentityPublicKey, canonicalTest)
		automatic <- err
	}()
	<-entered
	explicit := make(chan error, 1)
	go func() {
		_, err := tr.VerifyPinnedEndpoint(context.Background(), canonicalTest, f.m.HubInstanceID, f.m.IdentityPublicKey, newAddress)
		explicit <- err
	}()
	close(release)
	if err := <-automatic; err != nil {
		t.Fatal(err)
	}
	if err := <-explicit; err != nil {
		t.Fatal(err)
	}
	if tr.state(canonicalTest).manifest.Revision != 2 || len(f.data) != 0 {
		t.Fatal("an older automatic refresh overwrote the explicit verified endpoint")
	}
}

func TestPinnedActiveEntryCannotBeEvictedIntoTwoRefreshWriters(t *testing.T) {
	f := newSignedFixture(t)
	tr := New(f, t.TempDir())
	active := tr.acquireState(canonicalTest)
	active.manifest = signManifest(f.m, f.key)
	defer tr.releaseState(active)
	var others []*entry
	for i := 0; i < 15; i++ {
		e := tr.acquireState(fmt.Sprintf("https://busy%d.example.com/v1", i))
		if e == nil {
			t.Fatal("entry capacity exhausted before the configured bound")
		}
		others = append(others, e)
	}
	if e := tr.acquireState("https://overflow.example.com/v1"); e != nil {
		tr.releaseState(e)
		t.Fatal("an active cache entry was evicted to make room for a new flight")
	}
	if same := tr.acquireState(canonicalTest); same != active {
		t.Fatal("one logical Hub obtained two active state/refresh writers")
	} else {
		tr.releaseState(same)
	}
	for _, e := range others {
		tr.releaseState(e)
	}
	for i := 0; i < 50; i++ {
		e := tr.acquireState(fmt.Sprintf("https://other%d.example.com/v1", i))
		if e == nil {
			t.Fatal("an idle slot could not be reused")
		}
		tr.releaseState(e)
	}
	if len(tr.entries) != 16 || tr.state(canonicalTest) != active {
		t.Fatal("bounded churn replaced an active logical Hub entry")
	}
}
