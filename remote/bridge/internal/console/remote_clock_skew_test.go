package console

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/hubclient"
	"salcara/bridge/internal/hubingress"
)

func TestClockSkewConsoleSafeDiagnosticAndEnrollmentPrecedence(t *testing.T) {
	err := fmt.Errorf("synthetic-secret-url: %w", &hubingress.ClockSkewError{Details: hubingress.ClockSkewDetails{Direction: "local_behind", OffsetMS: 18000000, LocalTime: 100000000, SignedTime: 118000000, Source: "signed_manifest"}})
	for _, code := range []string{"", "remote_unavailable", "clock_skew", "enrollment_result_unknown", "hub_identity_changed", "source_changed"} {
		t.Run(code, func(t *testing.T) {
			w := httptest.NewRecorder()
			writeRemoteDiscoveryError(w, 503, code, err, hubclient.Discovery{})
			var got struct {
				Code      string                       `json:"code"`
				Error     string                       `json:"error"`
				ClockSkew *hubingress.ClockSkewDetails `json:"clockSkew"`
			}
			if json.Unmarshal(w.Body.Bytes(), &got) != nil {
				t.Fatal("invalid JSON")
			}
			clock := code == "" || code == "remote_unavailable" || code == "clock_skew"
			if clock {
				if w.Code != 409 || got.Code != "clock_skew" || got.ClockSkew == nil || strings.Contains(got.Error, "synthetic-secret") {
					t.Fatalf("unsafe or missing diagnostic: %d %s", w.Code, w.Body)
				}
			} else if got.Code != code || got.ClockSkew != nil || w.Code != 503 {
				t.Fatal("diagnostic replaced prior result", w.Body)
			}
		})
	}
}

// Real signed, nonce-bound proof through the HTTP route, never an asserted
// remote error code. Only synthetic fixtures are involved.
func installEnrollmentClockProof(t *testing.T, f *enrollmentFixture, afterGrant int) func() {
	t.Helper()
	base := http.DefaultTransport
	http.DefaultTransport = enrollmentFixtureTransport(func(r *http.Request) (*http.Response, error) {
		f.mu.Lock()
		active := f.issuerCount >= afterGrant
		f.mu.Unlock()
		if active && strings.HasSuffix(r.URL.Path, "/ingress/identity") {
			if r.Header.Get("Authorization") != "" || r.Header.Get("X-Salcara-Device-Secret") != "" {
				t.Error("anonymous proof leaked credentials")
			}
			nonce, id, issued := r.URL.Query().Get("nonce"), strings.Repeat("a", 32), time.Now().Add(-5*time.Hour).UnixMilli()
			key := ed25519.NewKeyFromSeed(make([]byte, 32))
			sig := ed25519.Sign(key, []byte(fmt.Sprintf("salcara-ingress-proof-v1\n%s\n%s\n%d\n%d", id, nonce, issued, issued+30000)))
			body, _ := json.Marshal(map[string]any{"version": 1, "hub_instance_id": id, "nonce": nonce, "issued_at": issued, "expires_at": issued + 30000, "signature": base64.StdEncoding.EncodeToString(sig)})
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
		}
		return base.RoundTrip(r)
	})
	return func() { http.DefaultTransport = base }
}

func TestClockSkewDiscoveryPreflightDoesNotCreateEnrollment(t *testing.T) {
	f := newEnrollmentFixture(t)
	s, h := newTestServer(t)
	_, body := enrollmentTestRequest(t, s, h, f.apiKey)
	restore := installEnrollmentClockProof(t, f, 0)
	defer restore()
	w := localRequest(s, h, "POST", "/api/remote/discover", body)
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"code":"clock_skew"`) || !strings.Contains(w.Body.String(), `"clockSkew"`) || f.issuerCount != 0 || f.registerCount != 0 || len(s.d.Store.Get().RemoteEnrollmentAttempts) != 0 {
		t.Fatalf("preflight turned into registration: %d %s", w.Code, w.Body)
	}
}

func TestClockSkewUnsentEnrollmentKeepsIdentityAndRecovers(t *testing.T) {
	f := newEnrollmentFixture(t)
	s, h := newTestServer(t)
	_, body := enrollmentTestRequest(t, s, h, f.apiKey)
	restore := installEnrollmentClockProof(t, f, 1)
	defer restore()
	w := localRequest(s, h, "POST", "/api/remote/discover", body)
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"enrollment_not_sent"`) || !strings.Contains(w.Body.String(), `"clockSkew"`) || !strings.Contains(w.Body.String(), `"fresh_proof"`) {
		t.Fatalf("not-sent clock evidence lost: %d %s", w.Code, w.Body)
	}
	pending := s.d.Store.Get().RemoteEnrollmentAttempts
	if len(pending) != 1 || pending[0].Submitted || f.registerCount != 0 {
		t.Fatal("known-unsent identity became unknown")
	}
	identity := pending[0].Connection
	restore()
	w = localRequest(s, h, "POST", "/api/remote/discover", body)
	if w.Code != 200 || f.registerCount != 1 || s.d.Store.Get().DeviceID != identity.DeviceID || s.d.Store.Get().DeviceSecret != identity.DeviceSecret {
		t.Fatalf("corrected clock failed same-identity retry: %d %s", w.Code, w.Body)
	}
}

func TestClockSkewRetryNeverClearsEarlierUnknownEnrollment(t *testing.T) {
	f := newEnrollmentFixture(t)
	f.dropAck, f.noCommit = true, true
	s, h := newTestServer(t)
	_, body := enrollmentTestRequest(t, s, h, f.apiKey)
	w := localRequest(s, h, "POST", "/api/remote/discover", body)
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"enrollment_result_unknown"`) {
		t.Fatal("fixture did not lose acknowledgement")
	}
	first := s.d.Store.Get().RemoteEnrollmentAttempts[0]
	restore := installEnrollmentClockProof(t, f, 2)
	defer restore()
	body["retryEnrollment"] = true
	w = localRequest(s, h, "POST", "/api/remote/discover", body)
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"enrollment_result_unknown"`) || strings.Contains(w.Body.String(), `"clockSkew"`) {
		t.Fatalf("clock replaced earlier unknown: %d %s", w.Code, w.Body)
	}
	pending := s.d.Store.Get().RemoteEnrollmentAttempts
	if len(pending) != 1 || !pending[0].Submitted || pending[0].Connection.DeviceID != first.Connection.DeviceID || f.registerCount != 1 {
		t.Fatal("clock retry discarded uncertain identity")
	}
}

func TestClockSkewIngressRecoveryDoesNotOverwriteSavedConnection(t *testing.T) {
	f := newEnrollmentFixture(t)
	s, h := newTestServer(t)
	const root = "https://old.fixture.example"
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.ConnectRemoteWithBotBinding(root, root+"/salcara-hub", "group-a", "saved-invite", "saved-signature")
		c.RemoteConnections[0].HubInstanceID = strings.Repeat("a", 32)
		c.RemoteConnections[0].IdentityPublicKey = f.publicKey
		c.ConnectRemote("https://active.fixture.example", "https://active.fixture.example/salcara-hub")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := s.d.Store.Get()
	restore := installEnrollmentClockProof(t, f, 0)
	defer restore()
	w := localRequest(s, h, "POST", "/api/remote/ingress", map[string]any{"hubUrl": root + "/salcara-hub", "groupId": "group-a", "url": enrollmentFixtureHub})
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"code":"clock_skew"`) || !strings.Contains(w.Body.String(), `"clockSkew"`) || !reflect.DeepEqual(before, s.d.Store.Get()) || f.registerCount != 0 {
		t.Fatalf("failed recovery overwrote connection: %d %s", w.Code, w.Body)
	}
}

func TestClockSkewDiscoveryReplacesFutureLocalObservation(t *testing.T) {
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remoteSupportFixturePing(w, remoteSupportObservation(upstream.URL, true), true)
	}))
	defer upstream.Close()
	s, h := newTestServer(t)
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.ConnectRemote(upstream.URL, upstream.URL+"/salcara-hub")
		future := &config.StationObservation{Mode: "unknown", CheckedAt: time.Now().Add(5 * time.Hour).UnixMilli()}
		c.RemoteConnections[0].RemoteSupport = config.CloneStationObservation(future)
		c.RemoteStationObservation = config.CloneStationObservation(future)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := s.d.Store.Get()
	w := localRequest(s, h, "POST", "/api/remote/discover", map[string]any{"url": upstream.URL, "connect": true, "requireOfficial": true})
	after := s.d.Store.Get()
	if w.Code != 200 || !after.RemoteStationObservation.Available() || !after.RemoteConnections[0].RemoteSupport.Available() || config.RemoteIdentity(before) != config.RemoteIdentity(after) {
		t.Fatalf("future unknown observation blocked corrected clock: %d %s", w.Code, w.Body)
	}
}
