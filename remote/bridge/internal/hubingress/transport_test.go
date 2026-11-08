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
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const canonicalTest = "https://hub.example.com/salcara-hub/v1"
const backupTest = "https://edge.example.com/salcara-hub/v1"

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type fixture struct {
	mu                sync.Mutex
	t                 *testing.T
	key               ed25519.PrivateKey
	m                 Manifest
	canonicalDown     bool
	canonicalDataDown bool
	wrongProof        bool
	replayProof       bool
	proofRedirect     bool
	dataStatus        int
	dataError         bool
	calls             []string
}

func newFixture(t *testing.T) *fixture {
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	now := time.Now().UnixMilli()
	return &fixture{t: t, key: key, m: Manifest{Version: 1, HubInstanceID: strings.Repeat("a", 32), IdentityPublicKey: base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey)), CanonicalURL: canonicalTest, Revision: 1, IssuedAt: now, ExpiresAt: now + 86400000, Endpoints: []Endpoint{{ID: "canonical", URL: canonicalTest, Priority: 100}, {ID: strings.Repeat("b", 32), URL: backupTest, Priority: 10}}}}
}
func response(value any, status int) *http.Response {
	raw, _ := json.Marshal(value)
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(raw)))}
}
func (f *fixture) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req.Method+" "+req.URL.Host+req.URL.Path)
	if strings.Contains(req.URL.Path, "/ingress/") {
		if req.Header.Get("Authorization") != "" || req.Header.Get("X-Salcara-Device-Secret") != "" || req.Header.Get("Cookie") != "" {
			f.t.Error("discovery leaked credentials")
		}
	}
	if f.canonicalDown && req.URL.Host == "hub.example.com" {
		return nil, errors.New("offline")
	}
	if strings.HasSuffix(req.URL.Path, "/ingress/manifest") {
		if req.URL.Host != "hub.example.com" {
			f.t.Error("backup refreshed trusted manifest")
		}
		return response(f.m, 200), nil
	}
	if strings.HasSuffix(req.URL.Path, "/ingress/identity") {
		if f.proofRedirect && req.URL.Host == "edge.example.com" {
			r := response(nil, 302)
			r.Header.Set("Location", "https://thief.example.com")
			return r, nil
		}
		now := time.Now().UnixMilli()
		p := proof{Version: 1, HubInstanceID: f.m.HubInstanceID, Nonce: req.URL.Query().Get("nonce"), IssuedAt: now, ExpiresAt: now + 30000}
		if f.replayProof && req.URL.Host == "edge.example.com" {
			p.Nonce = strings.Repeat("c", 64)
		}
		p.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(f.key, []byte(fmt.Sprintf("salcara-ingress-proof-v1\n%s\n%s\n%d\n%d", p.HubInstanceID, p.Nonce, p.IssuedAt, p.ExpiresAt))))
		if f.wrongProof && req.URL.Host == "edge.example.com" {
			p.Signature = base64.StdEncoding.EncodeToString(make([]byte, 64))
		}
		return response(p, 200), nil
	}
	if f.dataError {
		return nil, errors.New("connection reset after write")
	}
	if f.canonicalDataDown && req.URL.Host == "hub.example.com" {
		return nil, errors.New("canonical request failed")
	}
	status := 200
	if f.dataStatus != 0 && req.URL.Host == "edge.example.com" {
		status = f.dataStatus
	}
	return response(map[string]string{"destination": req.URL.Host}, status), nil
}
func request(t *testing.T, method string) *http.Request {
	t.Helper()
	var body io.Reader
	if method == "POST" {
		body = strings.NewReader(`{"command":"send"}`)
	}
	r, err := http.NewRequest(method, canonicalTest+"/bridge/events?deviceId=test", body)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer private")
	r.Header.Set("X-Salcara-Device-Secret", "secret")
	return Bind(r, canonicalTest)
}
func TestTransportRoutesWithoutChangingLogicalURLOrCredentials(t *testing.T) {
	f := newFixture(t)
	tr := New(f, t.TempDir())
	req := request(t, "POST")
	oldURL := req.URL.String()
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "edge.example.com") || req.URL.String() != oldURL || req.Header.Get("Authorization") != "Bearer private" {
		t.Fatal("canonical request mutated", string(body))
	}
	if len(f.calls) != 3 || !strings.Contains(f.calls[0], "hub.example.com/salcara-hub/v1/ingress/manifest") || !strings.Contains(f.calls[1], "/ingress/identity") {
		t.Fatal(f.calls)
	}
}
func TestTransportCanonicalHostSpellingDoesNotChangeLogicalIdentity(t *testing.T) {
	f := newFixture(t)
	tr := New(f, "")
	original := "https://HUB.EXAMPLE.COM:443/salcara-hub/v1"
	req, _ := http.NewRequest("GET", original+"/ping", nil)
	req = Bind(req, original)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "edge.example.com") || req.URL.String() != original+"/ping" {
		t.Fatal("normalization changed identity or disabled HA", string(raw))
	}
}
func TestTransportCanonicalHasNoProofRoundTripAndNextMutationCanFailOver(t *testing.T) {
	f := newFixture(t)
	f.m.Endpoints[0].Priority = 1
	tr := New(f, "")
	r, err := tr.RoundTrip(request(t, "GET"))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if len(f.calls) != 2 || strings.Contains(f.calls[1], "/ingress/identity") {
		t.Fatal("canonical added an identity RTT", f.calls)
	}
	f.canonicalDataDown = true
	before := len(f.calls)
	r, err = tr.RoundTrip(request(t, "POST"))
	if r != nil {
		r.Body.Close()
	}
	if err == nil || len(f.calls) != before+1 {
		t.Fatal("first mutation was replayed", f.calls[before:])
	}
	r, err = tr.RoundTrip(request(t, "POST"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	raw, _ := io.ReadAll(r.Body)
	if !strings.Contains(string(raw), "edge.example.com") {
		t.Fatal("next mutation did not fail over", string(raw))
	}
}
func TestTransportNeverReplaysSubmittedMutation(t *testing.T) {
	for _, kind := range []string{"network_error", "503"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			f.dataError = kind == "network_error"
			f.dataStatus = 503
			tr := New(f, "")
			resp, _ := tr.RoundTrip(request(t, "POST"))
			if resp != nil {
				resp.Body.Close()
			}
			posts := 0
			for _, call := range f.calls {
				if strings.HasPrefix(call, "POST ") {
					posts++
				}
			}
			if posts != 1 {
				t.Fatal("mutation replayed", f.calls)
			}
		})
	}
}
func TestTransportRejectsUnprovedBackupBeforeSendingCredentials(t *testing.T) {
	for _, kind := range []string{"wrong_key", "replayed_nonce", "redirect"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			f.wrongProof = kind == "wrong_key"
			f.replayProof = kind == "replayed_nonce"
			f.proofRedirect = kind == "redirect"
			tr := New(f, "")
			resp, err := tr.RoundTrip(request(t, "POST"))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			for _, call := range f.calls {
				if strings.HasPrefix(call, "POST edge.example.com") || strings.Contains(call, "thief.example.com") {
					t.Fatal("unproved endpoint received traffic", f.calls)
				}
			}
			if f.calls[len(f.calls)-1] != "POST hub.example.com/salcara-hub/v1/bridge/events" {
				t.Fatal(f.calls)
			}
		})
	}
}
func TestTransportCachedManifestSurvivesCanonicalOutageButExpires(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	tr := New(f, dir)
	r, e := tr.RoundTrip(request(t, "GET"))
	if e != nil {
		t.Fatal(e)
	}
	r.Body.Close()
	f.canonicalDown = true
	tr = New(f, dir)
	r, e = tr.RoundTrip(request(t, "POST"))
	if e != nil {
		t.Fatal("restart lost valid backup cache", e)
	}
	r.Body.Close()
	path := tr.cachePath(canonicalTest)
	m := f.m
	m.IssuedAt = time.Now().Add(-25 * time.Hour).UnixMilli()
	m.ExpiresAt = m.IssuedAt + 86400000
	raw, _ := json.Marshal(m)
	os.WriteFile(path, raw, 0600)
	before := len(f.calls)
	tr = New(f, dir)
	r, e = tr.RoundTrip(request(t, "POST"))
	if r != nil {
		r.Body.Close()
	}
	if e == nil {
		t.Fatal("expired cache accepted")
	}
	for _, call := range f.calls[before:] {
		if strings.Contains(call, "edge.example.com") {
			t.Fatal("expired endpoint used", f.calls[before:])
		}
	}
}
func TestTransportSafeReadFailsOverAndUnboundRequestsStayDirect(t *testing.T) {
	f := newFixture(t)
	f.dataStatus = 503
	tr := New(f, "")
	resp, err := tr.RoundTrip(request(t, "GET"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "hub.example.com") {
		t.Fatal(string(raw))
	}
	before := len(f.calls)
	req, _ := http.NewRequest("GET", canonicalTest+"/ping", nil)
	resp, err = tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(f.calls) != before+1 || f.calls[before] != "GET hub.example.com/salcara-hub/v1/ping" {
		t.Fatal("unbound discovery routed", f.calls)
	}
}
func TestTransportRejectsUnsafeManifestAndBoundsCache(t *testing.T) {
	f := newFixture(t)
	for _, url := range []string{"http://edge.example.com/salcara-hub/v1", "https://127.0.0.1/salcara-hub/v1", "https://user:pass@edge.example.com/salcara-hub/v1", "https://edge.example.com/salcara-hub/v1?token=1", "https://edge.example.com/other", "https://localhost/salcara-hub/v1"} {
		m := f.m
		m.Endpoints = append([]Endpoint{}, m.Endpoints...)
		m.Endpoints[1].URL = url
		if validManifest(m, canonicalTest, time.Now()) {
			t.Fatal("unsafe URL accepted", url)
		}
	}
	tr := New(f, t.TempDir())
	for i := 0; i < 20; i++ {
		m := f.m
		m.CanonicalURL = fmt.Sprintf("https://hub%d.example.com/salcara-hub/v1", i)
		tr.persist(m)
	}
	files, _ := os.ReadDir(tr.dir)
	if len(files) != 16 {
		t.Fatal("unbounded cache", len(files))
	}
}
func TestTransportCancellationAndConcurrentRequests(t *testing.T) {
	f := newFixture(t)
	tr := New(f, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := request(t, "POST").WithContext(context.WithValue(ctx, canonicalKey{}, canonicalTest))
	_, _ = tr.RoundTrip(req)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := tr.RoundTrip(request(t, "GET"))
			if err != nil {
				t.Error(err)
			} else {
				r.Body.Close()
			}
		}()
	}
	wg.Wait()
}
