package hubclient

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/hubingress"
)

// Real TLS exchanges exercise the discovery -> signed alias -> scoped policy
// -> device request chain, while synthetic public names keep credentials and
// pins completely independent of any installed or deployed Hub.
func TestSignedHubDiscoveryReconnectAndResetFencesCredentials(t *testing.T) {
	const canonical = "https://hub.fixture.example/tenant/salcara-hub/v1"
	const alias = "https://edge.fixture.example/remote/v1"
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	hubID := strings.Repeat("a", 32)
	var mu sync.Mutex
	policyRequests, deviceRequests := 0, 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		now := time.Now().UnixMilli()
		if strings.HasSuffix(r.URL.Path, "/ping") || strings.Contains(r.URL.Path, "/ingress/") {
			for _, h := range []string{"Authorization", "Cookie", "X-Salcara-Device-Id", "X-Salcara-Device-Secret", "X-Salcara-Enrollment-Claim"} {
				if r.Header.Get(h) != "" {
					t.Error("anonymous identity discovery leaked credential")
				}
			}
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/ping"):
			_ = json.NewEncoder(w).Encode(map[string]any{"service": "salcara-hub", "protocol": "salcara-remote", "protocolVersion": 1, "authModes": []string{"device-pairing"}, "capabilities": []string{"pair.qr.v1", "pair.revoke.v1", "device.identity.v1", "session.remote.v1", "station.guard.v1", "discovery.policy.v1", "ingress.signed-manifest.v1"}, "remoteAccess": policyFixture()})
		case strings.HasSuffix(r.URL.Path, "/ingress/manifest"):
			m := hubingress.Manifest{Version: 1, HubInstanceID: hubID, IdentityPublicKey: base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey)), CanonicalURL: canonical, Revision: 1, IssuedAt: now, ExpiresAt: now + 86400000, Endpoints: []hubingress.Endpoint{{ID: "canonical", URL: canonical, Priority: 100}, {ID: strings.Repeat("b", 32), URL: alias, Priority: 10}}}
			payload, _ := json.Marshal(m)
			m.SignedPayload = base64.StdEncoding.EncodeToString(payload)
			m.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, append([]byte("salcara-ingress-manifest-v1\n"), payload...)))
			_ = json.NewEncoder(w).Encode(m)
		case strings.HasSuffix(r.URL.Path, "/ingress/identity"):
			nonce := r.URL.Query().Get("nonce")
			message := []byte(fmt.Sprintf("salcara-ingress-proof-v1\n%s\n%s\n%d\n%d", hubID, nonce, now, now+30000))
			_ = json.NewEncoder(w).Encode(map[string]any{"version": 1, "hub_instance_id": hubID, "nonce": nonce, "issued_at": now, "expires_at": now + 30000, "signature": base64.StdEncoding.EncodeToString(ed25519.Sign(key, message))})
		case strings.HasSuffix(r.URL.Path, "/discovery/policy"):
			policyRequests++
			if r.Header.Get("X-Salcara-Device-Id") == "" || r.Header.Get("X-Salcara-Device-Secret") == "" || r.Header.Get("Authorization") != "" {
				t.Error("wrong policy credential scope")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "scope": "device", "remoteAccess": policyFixture()})
		case strings.HasSuffix(r.URL.Path, "/bridge/pair/status"):
			deviceRequests++
			if r.Host != "edge.fixture.example" || r.URL.Path != "/remote/v1/bridge/pair/status" || r.Header.Get("X-Salcara-Device-Secret") == "" || r.Header.Get("Authorization") != "" {
				t.Error("alias lost full path or device-only authorization")
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	target, _ := url.Parse(srv.URL)
	raw := srv.Client().Transport
	mapped := ingressClientRoundTrip(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "hub.fixture.example" && req.URL.Host != "edge.fixture.example" {
			return nil, errors.New("unexpected test destination")
		}
		next := req.Clone(req.Context())
		address := *req.URL
		next.Host = req.URL.Host
		address.Scheme, address.Host = target.Scheme, target.Host
		next.URL = &address
		return raw.RoundTrip(next)
	})
	previous := http.DefaultTransport
	http.DefaultTransport = mapped
	t.Cleanup(func() { http.DefaultTransport = previous })
	store, err := config.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Update(func(c *config.Config) error {
		c.ConnectRemote("https://hub.fixture.example", config.NormalizeHubURL(canonical))
		return nil
	})
	ctx := context.Background()
	info, err := Discover(ctx, canonical, canonical)
	if err != nil {
		t.Fatal(err)
	}
	info, saved, err := ResolveDiscoveryConnection(ctx, info, store.Get())
	if err != nil || saved == nil || info.Identity == nil {
		t.Fatalf("initial exact identity: %v", err)
	}
	_ = store.Update(func(c *config.Config) error {
		c.RemoteConnections[0].HubInstanceID = info.Identity.HubInstanceID
		c.RemoteConnections[0].IdentityPublicKey = info.Identity.IdentityPublicKey
		return nil
	})
	before := store.Get()
	logicalIdentity := config.RemoteIdentity(before)
	info, err = Discover(ctx, alias, alias)
	if err != nil {
		t.Fatal(err)
	}
	info, saved, err = ResolveDiscoveryConnection(ctx, info, before)
	if err != nil || saved == nil || saved.DeviceID != before.DeviceID {
		t.Fatalf("verified alias did not reuse enrolled computer: %v", err)
	}
	info, err = DiscoverDevicePolicy(ctx, info, saved.DeviceID, saved.DeviceSecret, "")
	if err != nil || !info.RemoteAccess.Available() {
		t.Fatalf("scoped alias policy failed: %v", err)
	}
	_ = store.Update(func(c *config.Config) error { c.RemoteConnections[0].IngressURL = alias; return nil })
	client := New(Options{Store: store, HTTP: &http.Client{Transport: mapped}})
	req, err := client.newRequest(ctx, http.MethodGet, "/bridge/pair/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.o.HTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if config.RemoteIdentity(store.Get()) != logicalIdentity {
		t.Fatal("verified routing changed logical pairing identity")
	}
	mu.Lock()
	key = ed25519.NewKeyFromSeed(bytesForReset())
	hubID = strings.Repeat("f", 32)
	mu.Unlock()
	resetInfo, err := Discover(ctx, canonical, canonical)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = ResolveDiscoveryConnection(ctx, resetInfo, store.Get())
	if !errors.Is(err, ErrHubIdentityChanged) {
		t.Fatal("replacement Hub was trusted")
	}
	req, err = client.newRequest(ctx, http.MethodGet, "/bridge/pair/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err = client.o.HTTP.Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("retired credential sent after Hub reset")
	}
	mu.Lock()
	defer mu.Unlock()
	if policyRequests != 1 || deviceRequests != 1 {
		t.Fatal("reset sent another credential request")
	}
}

func bytesForReset() []byte { return []byte(strings.Repeat("x", ed25519.SeedSize)) }
