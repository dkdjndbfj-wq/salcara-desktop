package hubingress

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func signManifest(m Manifest, key ed25519.PrivateKey) Manifest {
	m.SignedPayload, m.Signature = "", ""
	raw, _ := json.Marshal(m)
	m.SignedPayload = base64.StdEncoding.EncodeToString(raw)
	m.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, append([]byte("salcara-ingress-manifest-v1\n"), raw...)))
	return m
}

type signedFixture struct {
	mu                              sync.Mutex
	t                               *testing.T
	key                             ed25519.PrivateKey
	m                               Manifest
	mutate                          func(Manifest) Manifest
	reset, replay, stale, dataError bool
	dataStatus                      map[string]int
	proofs                          int
	data                            []string
}

func newSignedFixture(t *testing.T) *signedFixture {
	f := newFixture(t)
	return &signedFixture{t: t, key: f.key, m: f.m, dataStatus: map[string]int{}}
}

func (f *signedFixture) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, key := f.m, f.key
	if f.reset {
		key = ed25519.NewKeyFromSeed([]byte(strings.Repeat("b", 32)))
		m.HubInstanceID, m.IdentityPublicKey = strings.Repeat("c", 32), base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
	}
	if strings.Contains(r.URL.Path, "/ingress/") {
		for _, header := range []string{"Authorization", "Cookie", "X-Salcara-Device-Id", "X-Salcara-Device-Secret", "X-Salcara-Enrollment-Claim", "X-Salcara-Bot-Invite", "X-Salcara-Pair-Token"} {
			if r.Header.Get(header) != "" {
				f.t.Error("anonymous identity request carried a credential header")
			}
		}
		if strings.HasSuffix(r.URL.Path, "/manifest") {
			wire := signManifest(m, key)
			if f.mutate != nil {
				wire = f.mutate(wire)
			}
			return response(wire, 200), nil
		}
		f.proofs++
		now := time.Now().UnixMilli()
		p := proof{Version: 1, HubInstanceID: m.HubInstanceID, Nonce: r.URL.Query().Get("nonce"), IssuedAt: now, ExpiresAt: now + 30000}
		if f.replay {
			p.Nonce = strings.Repeat("c", 64)
		}
		if f.stale {
			p.IssuedAt, p.ExpiresAt = now-60000, now-30000
		}
		p.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, []byte(fmt.Sprintf("salcara-ingress-proof-v1\n%s\n%s\n%d\n%d", p.HubInstanceID, p.Nonce, p.IssuedAt, p.ExpiresAt))))
		return response(p, 200), nil
	}
	if r.Header.Get("Authorization") != "Bearer synthetic-model" || r.Header.Get("X-Salcara-Device-Secret") != "synthetic-device" {
		f.t.Error("data request credentials changed")
	}
	f.data = append(f.data, r.Method+" "+r.URL.String())
	if f.dataError {
		return nil, errors.New("synthetic network failure after send")
	}
	status := f.dataStatus[r.URL.Host]
	if status == 0 {
		status = 200
	}
	return response(map[string]string{"destination": r.URL.Host}, status), nil
}

func pinnedRequest(t *testing.T, f *signedFixture, method, endpoint string) *http.Request {
	t.Helper()
	var body io.Reader
	if method == "POST" {
		body = strings.NewReader(`{"synthetic":"command"}`)
	}
	r, err := http.NewRequest(method, f.m.CanonicalURL+"/bridge/events?deviceId=test&cursor=one", body)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer synthetic-model")
	r.Header.Set("X-Salcara-Device-Secret", "synthetic-device")
	r.Header.Set("X-Salcara-Enrollment-Claim", "synthetic-claim")
	return BindIdentity(Bind(r, f.m.CanonicalURL), f.m.HubInstanceID, f.m.IdentityPublicKey, endpoint)
}

func TestIngressURLPathGrammarAndPublicBoundary(t *testing.T) {
	for raw, want := range map[string]string{
		"https://HUB.EXAMPLE.COM:443":                         canonicalTest,
		"https://hub.example.com/":                            canonicalTest,
		"https://hub.example.com/tenant-a/salcara-hub/":       "https://hub.example.com/tenant-a/salcara-hub/v1",
		"https://hub.example.com/Tenant.A_~-1/salcara-hub/v1": "https://hub.example.com/Tenant.A_~-1/salcara-hub/v1",
		"https://hub.example.com/custom/v1":                   "https://hub.example.com/custom/v1",
		"https://hub.example.com/v1":                          "https://hub.example.com/v1",
		"https://hub.example.com/.../v1":                      "https://hub.example.com/.../v1",
	} {
		if got := normalizeURL(raw); got != want {
			t.Errorf("normalize %s: %s", raw, got)
		}
	}
	for _, raw := range []string{
		"https://hub.example.com/a/../v1", "https://hub.example.com/./v1", "https://hub.example.com/%2e%2e/v1", "https://hub.example.com/%252e%252e/v1", "https://hub.example.com/a%2fb/v1", "https://hub.example.com/a//v1", "https://hub.example.com/a\\b/v1", "https://hub.example.com/salcara-hub/v1/", "https://hub.example.com/salcara-hub-evil", "https://hub.example.com/custom", "https://hub.example.com/v1?key=synthetic", "https://hub.example.com/v1#fragment", "https://user@hub.example.com/v1", "http://hub.example.com/v1", "https://127.1/v1", "https://2130706433/v1", "https://127.0.0.1/v1", "https://10.2.3.4/v1", "https://[::ffff:127.0.0.1]/v1", "https://localhost/v1", "https://hub.local/v1", "https://hub..example.com/v1", "https://-hub.example.com/v1", "https://hub.example.com:/v1", "https://hub.example.com:65536/v1",
	} {
		if got := normalizeURL(raw); got != "" {
			t.Errorf("unsafe URL accepted: %s -> %s", raw, got)
		}
	}
}

func TestResolveIdentityUsesOnlySignedPayloadAndFreshAnonymousProof(t *testing.T) {
	f := newSignedFixture(t)
	f.mutate = func(wire Manifest) Manifest {
		wire.CanonicalURL = "https://thief.example.com/v1"
		wire.HubInstanceID = strings.Repeat("e", 32)
		wire.Endpoints = []Endpoint{{ID: "canonical", URL: wire.CanonicalURL}}
		return wire
	}
	tr := New(f, "")
	m, err := tr.ResolveIdentity(context.Background(), backupTest)
	if err != nil || m.CanonicalURL != canonicalTest || m.HubInstanceID != f.m.HubInstanceID || f.proofs != 1 || len(f.data) != 0 {
		t.Fatalf("signed identity not resolved safely: %v", err)
	}
	if _, err := tr.ResolveIdentity(context.Background(), "https://localhost/v1"); err == nil {
		t.Fatal("nonpublic identity resolved")
	}
}

func TestForwardingProxyCannotAddItselfToSignedMembership(t *testing.T) {
	for _, attack := range []string{"append-flat", "unsigned", "same-origin-other-path"} {
		t.Run(attack, func(t *testing.T) {
			f := newSignedFixture(t)
			candidate := "https://proxy.example.com/salcara-hub/v1"
			if attack == "same-origin-other-path" {
				candidate = "https://hub.example.com/other/v1"
			}
			f.mutate = func(wire Manifest) Manifest {
				wire.Endpoints = append(wire.Endpoints, Endpoint{ID: strings.Repeat("d", 32), URL: candidate})
				if attack == "unsigned" {
					wire.SignedPayload, wire.Signature = "", ""
				}
				return wire
			}
			if _, err := New(f, "").ResolveIdentity(context.Background(), candidate); err == nil || f.proofs != 0 || len(f.data) != 0 {
				t.Fatal("proxy received trust by forwarding a genuine proof")
			}
		})
	}
}

func TestSignedManifestRejectsWrongPinTamperingAndExpiry(t *testing.T) {
	f := newSignedFixture(t)
	for _, attack := range []string{"signature", "payload", "expired", "wrong-id", "wrong-key", "nested-signature"} {
		t.Run(attack, func(t *testing.T) {
			m, id, key := f.m, f.m.HubInstanceID, f.m.IdentityPublicKey
			if attack == "expired" {
				m.IssuedAt = time.Now().Add(-25 * time.Hour).UnixMilli()
				m.ExpiresAt = m.IssuedAt + 86400000
			}
			wire := signManifest(m, f.key)
			switch attack {
			case "signature":
				wire.Signature = base64.StdEncoding.EncodeToString(make([]byte, 64))
			case "payload":
				wire.SignedPayload = base64.StdEncoding.EncodeToString([]byte(`{}`))
			case "wrong-id":
				id = strings.Repeat("d", 32)
			case "wrong-key":
				key = base64.StdEncoding.EncodeToString(make([]byte, 32))
			case "nested-signature":
				raw, _ := json.Marshal(map[string]any{"version": 1, "signature": "", "signed_payload": ""})
				wire.SignedPayload = base64.StdEncoding.EncodeToString(raw)
				wire.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(f.key, append([]byte("salcara-ingress-manifest-v1\n"), raw...)))
			}
			if _, err := verifiedManifest(wire, id, key, canonicalTest, time.Now()); err == nil {
				t.Fatal("invalid signed manifest accepted")
			}
		})
	}
}

func TestPinnedCanonicalResetNeverReceivesOldCredentialsIncludingAfterRestart(t *testing.T) {
	f := newSignedFixture(t)
	dir := t.TempDir()
	tr := New(f, dir)
	r, err := tr.RoundTrip(pinnedRequest(t, f, "POST", canonicalTest))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if f.proofs != 1 || len(f.data) != 1 {
		t.Fatal("initial pin did not verify canonical")
	}
	f.reset = true
	for _, next := range []*Transport{tr, New(f, dir)} {
		r, err = next.RoundTrip(pinnedRequest(t, f, "POST", canonicalTest))
		if r != nil {
			r.Body.Close()
		}
		if err == nil || len(f.data) != 1 {
			t.Fatal("reset Hub received old device credentials")
		}
	}
}

func TestPinnedProofIsFreshPerRequestAndRejectsReplay(t *testing.T) {
	f := newSignedFixture(t)
	tr := New(f, "")
	for i := 0; i < 2; i++ {
		r, err := tr.RoundTrip(pinnedRequest(t, f, "POST", canonicalTest))
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
	}
	if f.proofs != 2 {
		t.Fatal("proof was reused between requests")
	}
	for _, attack := range []string{"nonce", "expired"} {
		f.replay, f.stale = attack == "nonce", attack == "expired"
		r, err := tr.RoundTrip(pinnedRequest(t, f, "POST", canonicalTest))
		if r != nil {
			r.Body.Close()
		}
		if err == nil || len(f.data) != 2 {
			t.Fatal("invalid proof received credentials")
		}
	}
}

func TestPinnedRoutingPreservesBothMeaningfulPathsAndQuery(t *testing.T) {
	f := newSignedFixture(t)
	f.m.CanonicalURL = "https://hub.example.com/Tenant-A/salcara-hub/v1"
	f.m.Endpoints[0].URL = f.m.CanonicalURL
	f.m.Endpoints[1].URL = "https://edge.example.com/Tenant-B/custom/v1"
	tr := New(f, "")
	req := pinnedRequest(t, f, "POST", f.m.Endpoints[1].URL)
	original := req.URL.String()
	r, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if len(f.data) != 1 || f.data[0] != "POST https://edge.example.com/Tenant-B/custom/v1/bridge/events?deviceId=test&cursor=one" || req.URL.String() != original {
		t.Fatal("prefixed endpoint was stripped or logical URL mutated", f.data)
	}
}

func TestPinnedLogicalAliasCanKeepItsURLAndCacheAcrossRestart(t *testing.T) {
	f := newSignedFixture(t)
	dir := t.TempDir()
	logical := backupTest
	tr := New(f, dir)
	for i := 0; i < 2; i++ {
		if i == 1 {
			tr = New(f, dir)
			if cached := tr.state(logical).manifest; cached.CanonicalURL != canonicalTest || cached.SignedPayload == "" {
				t.Fatal("logical alias lost signed route cache on restart")
			}
		}
		req := pinnedRequest(t, f, "POST", logical)
		logicalRequest, _ := http.NewRequest("POST", logical+"/bridge/events?deviceId=test&cursor=one", nil)
		req.URL = logicalRequest.URL
		req = Bind(req, logical)
		r, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if len(f.data) != i+1 || !strings.Contains(f.data[i], logical+"/bridge/events?") {
			t.Fatal("logical alias did not route safely", f.data)
		}
	}
}

func TestPinnedTransportNeverReplaysSubmittedMutationButRetriesSafeGET(t *testing.T) {
	for _, mode := range []string{"network-error", "503", "get"} {
		t.Run(mode, func(t *testing.T) {
			f := newSignedFixture(t)
			f.dataError = mode == "network-error"
			f.dataStatus["edge.example.com"] = 503
			method := "POST"
			if mode == "get" {
				method = "GET"
			}
			req := pinnedRequest(t, f, method, backupTest)
			r, _ := New(f, "").RoundTrip(req)
			if r != nil {
				r.Body.Close()
			}
			want := 1
			if mode == "get" {
				want = 2
			}
			if len(f.data) != want || f.proofs != want {
				t.Fatal("incorrect replay or missing fresh proof", f.data, f.proofs)
			}
		})
	}
}

func TestIncompleteIdentityBindingNeverFallsBackToLegacy(t *testing.T) {
	f := newSignedFixture(t)
	for _, pin := range []identityBinding{{id: f.m.HubInstanceID}, {key: f.m.IdentityPublicKey}, {endpoint: canonicalTest}, {id: f.m.HubInstanceID, key: f.m.IdentityPublicKey, endpoint: "https://localhost/v1"}} {
		req := pinnedRequest(t, f, "POST", canonicalTest)
		req = BindIdentity(req, pin.id, pin.key, pin.endpoint)
		r, err := New(f, "").RoundTrip(req)
		if r != nil {
			r.Body.Close()
		}
		if err == nil || len(f.data) != 0 {
			t.Fatal("invalid pin silently became legacy")
		}
	}
}

func TestPinnedTransportConcurrentRequestsEachVerifyIdentity(t *testing.T) {
	f := newSignedFixture(t)
	tr := New(f, t.TempDir())
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := tr.RoundTrip(pinnedRequest(t, f, "POST", canonicalTest))
			if err != nil {
				t.Error(err)
				return
			}
			r.Body.Close()
		}()
	}
	wg.Wait()
	if f.proofs != 12 || len(f.data) != 12 {
		t.Fatal("concurrent requests reused an identity proof")
	}
}
