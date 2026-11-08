package hubclient

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/hubingress"
)

type ingressClientRoundTrip func(*http.Request) (*http.Response, error)

func (f ingressClientRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestIngressHTTPAndStreamPreservePairingIdentity(t *testing.T) {
	store, err := config.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	store.Update(func(c *config.Config) error {
		c.HubURL = "https://hub.example.com/salcara-hub"
		c.DeviceID = "computer"
		c.DeviceSecret = strings.Repeat("a", 64)
		c.AccountKey = "private-key"
		return nil
	})
	before := store.Get()
	identity := config.RemoteIdentity(before)
	canonical := before.EffectiveHubURL() + "/v1"
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	hubID := strings.Repeat("b", 32)
	data := 0
	base := ingressClientRoundTrip(func(req *http.Request) (*http.Response, error) {
		var payload any
		now := time.Now().UnixMilli()
		if strings.HasSuffix(req.URL.Path, "/manifest") {
			if req.URL.Host != "hub.example.com" || req.Header.Get("Authorization") != "" {
				t.Fatal("discovery was not canonical and anonymous")
			}
			payload = hubingress.Manifest{Version: 1, HubInstanceID: hubID, IdentityPublicKey: base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey)), CanonicalURL: canonical, Revision: 1, IssuedAt: now, ExpiresAt: now + 86400000, Endpoints: []hubingress.Endpoint{{ID: "canonical", URL: canonical, Priority: 100}, {ID: strings.Repeat("c", 32), URL: "https://backup.example.com/salcara-hub/v1", Priority: 10}}}
		} else if strings.HasSuffix(req.URL.Path, "/identity") {
			nonce := req.URL.Query().Get("nonce")
			sig := ed25519.Sign(key, []byte(fmt.Sprintf("salcara-ingress-proof-v1\n%s\n%s\n%d\n%d", hubID, nonce, now, now+30000)))
			payload = map[string]any{"version": 1, "hub_instance_id": hubID, "nonce": nonce, "issued_at": now, "expires_at": now + 30000, "signature": base64.StdEncoding.EncodeToString(sig)}
		} else {
			data++
			if req.URL.Host != "backup.example.com" || req.Header.Get("Authorization") != "Bearer private-key" || req.Header.Get("X-Salcara-Device-Secret") != before.DeviceSecret {
				t.Fatal("incorrect destination or credentials")
			}
			payload = map[string]bool{"ok": true}
		}
		raw, _ := json.Marshal(payload)
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
	})
	provided := &http.Client{Transport: base}
	c := New(Options{Store: store, HTTP: provided})
	for _, stream := range []bool{false, true} {
		path := "/bridge/pair/status"
		if stream {
			path = "/bridge/stream"
		}
		req, err := c.newRequest(context.Background(), "GET", path, nil)
		if err != nil {
			t.Fatal(err)
		}
		client := c.o.HTTP
		if stream {
			client = c.streamHTTP
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if req.URL.String() != canonical+path {
			t.Fatal("logical request URL changed")
		}
	}
	if data != 2 || store.Get().EffectiveHubURL() != before.EffectiveHubURL() || config.RemoteIdentity(store.Get()) != identity {
		t.Fatal("transport switched pairing identity")
	}
	if _, ok := provided.Transport.(*hubingress.Transport); ok {
		t.Fatal("mutated caller HTTP client")
	}
}
