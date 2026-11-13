package hubclient

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/hubingress"
)

type clockFixtureTransport func(*http.Request) (*http.Response, error)

func (f clockFixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClockSkewStatusLifecycleCopyAndLateFailureFence(t *testing.T) {
	c, store := newTestClient(t, "https://one.test", "synthetic", nil)
	cfg, epoch := store.Get(), c.connectionEpoch
	clock := &hubingress.ClockSkewError{Details: hubingress.ClockSkewDetails{Direction: "local_behind", OffsetMS: 18000000, LocalTime: 100000000, SignedTime: 118000000, Source: "signed_manifest"}}
	err := fmt.Errorf("synthetic wrapper: %w", clock)
	if RemoteErrorCode(err) != "clock_skew" {
		t.Fatal("local typed code lost")
	}
	state, published := c.publishConnectionFailure(cfg, epoch, err)
	if !published || state != StateConnecting || c.Status().ErrorCode != "clock_skew" || c.Status().ClockSkew == nil || strings.Contains(c.Status().Error, "synthetic wrapper") {
		t.Fatal("clock evidence lost or private error leaked", c.Status())
	}
	snapshot := c.Status()
	snapshot.ClockSkew.OffsetMS = 0
	clock.Details.OffsetMS = 1
	if c.Status().ClockSkew.OffsetMS != 18000000 {
		t.Fatal("status aliases error or caller snapshot")
	}
	if err := c.acceptStreamConnection(context.Background(), cfg, epoch); err != nil {
		t.Fatal(err)
	}
	if c.Status().ClockSkew != nil || c.Status().ErrorCode != "" {
		t.Fatal("success retained clock warning")
	}
	c.publishConnectionFailure(cfg, epoch, err)
	c.Kick()
	if c.Status().ClockSkew != nil || c.Status().ErrorCode != "" {
		t.Fatal("retry retained old warning")
	}
	if _, published := c.publishConnectionFailure(cfg, epoch, err); published || c.Status().ClockSkew != nil {
		t.Fatal("retired clock failure repainted new connection")
	}
	c.publishConnectionFailure(store.Get(), c.connectionEpoch, err)
	c.setStateCode(StateConnecting, "ordinary retry", "remote_unavailable")
	if c.Status().ClockSkew != nil {
		t.Fatal("ordinary network failure retained clock evidence")
	}
}

func TestClockSkewCannotBeAssertedByRemoteJSON(t *testing.T) {
	resp := &http.Response{StatusCode: 409, Body: io.NopCloser(strings.NewReader(`{"code":"clock_skew","error":"synthetic-secret","clockSkew":{"offsetMs":18000000}}`))}
	err := checkResp(resp)
	_, code, message := remoteFailure(err, config.Config{})
	if code == "clock_skew" || RemoteErrorCode(err) == "clock_skew" || hubingress.ClockSkew(err) != nil || strings.Contains(message, "synthetic-secret") {
		t.Fatal("untrusted HTTP JSON asserted clock evidence")
	}
	clock := &hubingress.ClockSkewError{}
	wrapped := &RemoteHTTPError{Code: "enrollment_not_sent", StatusCode: 409, cause: clock}
	if RemoteErrorCode(wrapped) != "enrollment_not_sent" || hubingress.ClockSkew(wrapped) == nil {
		t.Fatal("known-unsent semantics were replaced by diagnostic")
	}
}

func TestClockSkewDiscoveryRetainsExactSavedPin(t *testing.T) {
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	pub := base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
	base := "https://clock.example.com/salcara-hub"
	issued := time.Now().Add(5 * time.Hour).UnixMilli()
	m := hubingress.Manifest{Version: 1, HubInstanceID: strings.Repeat("a", 32), IdentityPublicKey: pub, CanonicalURL: base + "/v1", Revision: 1, IssuedAt: issued, ExpiresAt: issued + 86400000, Endpoints: []hubingress.Endpoint{{ID: "canonical", URL: base + "/v1", Priority: 1}}}
	payload, _ := json.Marshal(m)
	m.SignedPayload = base64.StdEncoding.EncodeToString(payload)
	m.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, append([]byte("salcara-ingress-manifest-v1\n"), payload...)))
	wire, _ := json.Marshal(m)
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = clockFixtureTransport(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/ingress/manifest") || r.Header.Get("Authorization") != "" || r.Header.Get("X-Salcara-Device-Secret") != "" {
			t.Fatal("clock discovery sent credentials or unnecessary proof")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(wire)))}, nil
	})
	info := Discovery{HubURL: base, Capabilities: []string{"ingress.signed-manifest.v1"}}
	for _, mode := range []string{"new", "exact", "wrong-id", "wrong-key", "partial"} {
		t.Run(mode, func(t *testing.T) {
			cfg := config.Config{}
			if mode != "new" {
				pin := config.RemoteConnection{HubURL: base, HubInstanceID: m.HubInstanceID, IdentityPublicKey: pub, DeviceID: "synthetic-device", DeviceSecret: "synthetic-secret"}
				switch mode {
				case "wrong-id":
					pin.HubInstanceID = strings.Repeat("b", 32)
				case "wrong-key":
					pin.IdentityPublicKey = "replacement"
				case "partial":
					pin.IdentityPublicKey = ""
				}
				cfg.RemoteConnections = []config.RemoteConnection{pin}
			}
			got, _, err := ResolveDiscoveryConnection(context.Background(), info, cfg)
			want := mode == "new" || mode == "exact"
			if (hubingress.ClockSkew(err) != nil) != want || got.Identity != nil {
				t.Fatalf("clock diagnostic bypassed saved pin: %s %v", mode, err)
			}
		})
	}
}
