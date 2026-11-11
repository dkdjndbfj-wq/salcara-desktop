package console

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/enrollment"
	"salcara/bridge/internal/hubingress"
)

type enrollmentFixtureTransport func(*http.Request) (*http.Response, error)

func (f enrollmentFixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type enrollmentFixture struct {
	t                                          *testing.T
	mu                                         sync.Mutex
	issuerURL                                  string
	apiKey                                     string
	grantMutation                              string
	ack                                        string
	dropAck                                    bool
	noCommit                                   bool
	rejectRegister                             bool
	issuerEntered, issuerRelease               chan struct{}
	challengeCount, issuerCount, registerCount int
	devices                                    map[string]string
	challenge                                  enrollment.Challenge
	publicKey                                  string
}

const enrollmentFixtureOrigin = "https://station.fixture.example"
const enrollmentFixtureHub = enrollmentFixtureOrigin + "/salcara-hub"

func newEnrollmentFixture(t *testing.T) *enrollmentFixture {
	f := &enrollmentFixture{t: t, issuerURL: enrollmentFixtureOrigin + "/device-enrollment", apiKey: "synthetic-api-secret", devices: map[string]string{}}
	hubKey := ed25519.NewKeyFromSeed(make([]byte, 32))
	issuerKey := ed25519.NewKeyFromSeed(bytesWithValue(32, 1))
	f.publicKey = base64.StdEncoding.EncodeToString(hubKey.Public().(ed25519.PublicKey))
	hubID := strings.Repeat("a", 32)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		now := time.Now()
		anonymous := strings.HasSuffix(r.URL.Path, "/ping") || strings.Contains(r.URL.Path, "/ingress/") || strings.HasSuffix(r.URL.Path, "/enrollment/challenge")
		if anonymous {
			for _, name := range []string{"Authorization", "Cookie", "X-Api-Key", "X-Salcara-Device-Secret", "X-Salcara-Enrollment-Claim", "X-Salcara-Bot-Invite"} {
				if r.Header.Get(name) != "" {
					t.Errorf("anonymous discovery leaked %s", name)
				}
			}
		}
		switch r.URL.Path {
		case "/salcara-hub/v1/ping":
			_ = json.NewEncoder(w).Encode(map[string]any{"service": "salcara-hub", "protocol": "salcara-remote", "protocolVersion": 1, "authModes": []string{"device-pairing"}, "capabilities": []string{"pair.qr.v1", "pair.revoke.v1", "device.identity.v1", "session.remote.v1", "station.guard.v1", "discovery.policy.v1", "ingress.signed-manifest.v1", "device.enrollment.api.v1"}, "deviceEnrollment": map[string]any{"version": 1, "hubInstanceId": hubID, "identityPublicKey": f.publicKey, "audience": "fixture-hub", "policyRevision": "r1", "issuers": []map[string]any{{"id": "station", "publicKey": base64.RawURLEncoding.EncodeToString(issuerKey.Public().(ed25519.PublicKey)), "sourceTypes": []string{"site_api"}, "origins": []string{enrollmentFixtureOrigin}, "apiBases": []string{enrollmentFixtureOrigin}, "deviceEnrollment": true, "enrollmentUrl": f.issuerURL}}}})
		case "/salcara-hub/v1/ingress/manifest":
			m := hubingress.Manifest{Version: 1, HubInstanceID: hubID, IdentityPublicKey: f.publicKey, CanonicalURL: enrollmentFixtureHub + "/v1", Revision: 1, IssuedAt: now.UnixMilli(), ExpiresAt: now.Add(24 * time.Hour).UnixMilli(), Endpoints: []hubingress.Endpoint{{ID: "canonical", URL: enrollmentFixtureHub + "/v1", Priority: 1}}}
			payload, _ := json.Marshal(m)
			m.SignedPayload = base64.StdEncoding.EncodeToString(payload)
			m.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(hubKey, append([]byte("salcara-ingress-manifest-v1\n"), payload...)))
			_ = json.NewEncoder(w).Encode(m)
		case "/salcara-hub/v1/ingress/identity":
			nonce := r.URL.Query().Get("nonce")
			issued, expires := now.UnixMilli(), now.Add(30*time.Second).UnixMilli()
			message := []byte(fmt.Sprintf("salcara-ingress-proof-v1\n%s\n%s\n%d\n%d", hubID, nonce, issued, expires))
			_ = json.NewEncoder(w).Encode(map[string]any{"version": 1, "hub_instance_id": hubID, "nonce": nonce, "issued_at": issued, "expires_at": expires, "signature": base64.StdEncoding.EncodeToString(ed25519.Sign(hubKey, message))})
		case "/salcara-hub/v1/device/enrollment/challenge":
			f.challengeCount++
			var request struct {
				Version          int    `json:"version"`
				Issuer           string `json:"issuer"`
				APIBase          string `json:"apiBase"`
				DeviceID         string `json:"deviceId"`
				DeviceSecretHash string `json:"deviceSecretHash"`
				RequestID        string `json:"requestId"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			if f.devices[request.DeviceID] != "" {
				w.WriteHeader(409)
				_, _ = w.Write([]byte(`{"code":"enrollment_request_conflict"}`))
				return
			}
			f.challenge = enrollment.Challenge{Purpose: enrollment.ChallengePurpose, Version: 1, Issuer: request.Issuer, HubInstanceID: hubID, Audience: "fixture-hub", APIBase: request.APIBase, PolicyRevision: "r1", DeviceID: request.DeviceID, DeviceSecretHash: request.DeviceSecretHash, ChallengeID: "challenge-fixture", RequestID: request.RequestID, IssuedAt: now.Unix(), ExpiresAt: now.Add(120 * time.Second).Unix()}
			proof, _ := enrollment.SignEnvelope(f.challenge, enrollment.ChallengePurpose, hubKey)
			_ = json.NewEncoder(w).Encode(proof)
		case "/device-enrollment":
			f.issuerCount++
			if f.issuerEntered != nil {
				close(f.issuerEntered)
				f.mu.Unlock()
				<-f.issuerRelease
				f.mu.Lock()
			}
			var request struct {
				Credential struct {
					Type  string `json:"type"`
					Value string `json:"value"`
				} `json:"credential"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			if request.Credential.Type != "api_key" || request.Credential.Value != f.apiKey {
				w.WriteHeader(401)
				_, _ = w.Write([]byte(`{"code":"api_credentials_invalid"}`))
				return
			}
			grant := enrollment.Grant{Challenge: f.challenge, Subject: "sub2api-user:fixture", GroupID: 1}
			grant.Purpose = enrollment.GrantPurpose
			if f.grantMutation == "device" {
				grant.DeviceID = "another-device"
			}
			if f.grantMutation == "expired" {
				grant.IssuedAt, grant.ExpiresAt = now.Add(-200*time.Second).Unix(), now.Add(-100*time.Second).Unix()
			}
			proof, _ := enrollment.SignEnvelope(grant, enrollment.GrantPurpose, issuerKey)
			_ = json.NewEncoder(w).Encode(proof)
		case "/salcara-hub/v1/device/register":
			f.registerCount++
			if f.rejectRegister {
				w.WriteHeader(403)
				_, _ = w.Write([]byte(`{"code":"api_enrollment_denied"}`))
				return
			}
			id, secret := r.Header.Get("X-Salcara-Device-Id"), r.Header.Get("X-Salcara-Device-Secret")
			if r.Header.Get("Authorization") != "" || r.Header.Get("X-Salcara-Enrollment-Claim") != "" || r.Header.Get("X-Salcara-Bot-Invite") != "" {
				t.Error("enrollment mixed credential scopes")
			}
			wire, _ := base64.RawURLEncoding.Strict().DecodeString(r.Header.Get("X-Salcara-Enrollment-Grant"))
			proof, err := enrollment.ParseEnvelope(wire)
			payload, verifyErr := enrollment.VerifyEnvelope(proof, enrollment.GrantPurpose, issuerKey.Public().(ed25519.PublicKey))
			grant, parseErr := enrollment.ParseGrant(payload)
			hash := sha256.Sum256([]byte(secret))
			if err != nil || verifyErr != nil || parseErr != nil || grant.DeviceID != id || grant.DeviceSecretHash != hex.EncodeToString(hash[:]) {
				t.Error("register did not carry its signed device/secret grant")
			}
			if !f.noCommit {
				f.devices[id] = secret
			}
			if f.dropAck {
				conn, _, _ := w.(http.Hijacker).Hijack()
				_ = conn.Close()
				return
			}
			if f.ack != "" {
				_, _ = w.Write([]byte(f.ack))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "device": map[string]string{"deviceId": id}})
		case "/salcara-hub/v1/discovery/policy":
			if f.devices[r.Header.Get("X-Salcara-Device-Id")] != r.Header.Get("X-Salcara-Device-Secret") || r.Header.Get("X-Salcara-Device-Secret") == "" {
				w.WriteHeader(403)
				_, _ = w.Write([]byte(`{"code":"device_credentials_invalid"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "scope": "device", "remoteAccess": remoteSupportObservation(enrollmentFixtureOrigin, true)})
		default:
			t.Errorf("unexpected fixture endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	target, _ := url.Parse(srv.URL)
	raw := srv.Client().Transport
	previous := http.DefaultTransport
	http.DefaultTransport = enrollmentFixtureTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "station.fixture.example" {
			return nil, errors.New("unknown credential destination")
		}
		next := r.Clone(r.Context())
		address := *r.URL
		next.Host = r.URL.Host
		address.Scheme, address.Host = target.Scheme, target.Host
		next.URL = &address
		return raw.RoundTrip(next)
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
	return f
}

func bytesWithValue(length int, value byte) []byte {
	out := make([]byte, length)
	for i := range out {
		out[i] = value
	}
	return out
}

func enrollmentTestRequest(t *testing.T, s *Server, h http.Handler, key string) (string, map[string]any) {
	t.Helper()
	s.d.Store.SetRuntimeGatewayKey("synthetic-enrollment-runtime")
	id := createLocal(t, s, h, "api", "Fixture API", key, enrollmentFixtureOrigin+"/v1")
	cfg := s.d.Store.Get()
	account, _ := cfg.LocalAccount(id)
	return id, map[string]any{"accountId": id, "expectedSourceRevision": cfg.AccountSourceRevision(account), "url": enrollmentFixtureOrigin + "/v1", "connect": true}
}

func TestRemoteAPIEnrollmentRegistersThenRecoversWithoutAPIKey(t *testing.T) {
	f := newEnrollmentFixture(t)
	s, h := newTestServer(t)
	id, body := enrollmentTestRequest(t, s, h, f.apiKey)
	probe := map[string]any{}
	for k, v := range body {
		probe[k] = v
	}
	probe["connect"] = false
	if w := localRequest(s, h, "POST", "/api/remote/discover", probe); w.Code != 200 || f.issuerCount != 0 || f.registerCount != 0 {
		t.Fatal("preflight used API identity or registered")
	}
	w := localRequest(s, h, "POST", "/api/remote/discover", body)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"registered":true`) || f.challengeCount != 1 || f.issuerCount != 1 || f.registerCount != 1 {
		t.Fatalf("enrollment failed: %d %s", w.Code, w.Body)
	}
	cfg := s.d.Store.Get()
	if len(cfg.RemoteConnections) != 1 || len(cfg.RemoteEnrollmentAttempts) != 0 || cfg.AccountKey != "" || cfg.RemoteConnections[0].HubInstanceID == "" {
		t.Fatal("enrollment lost device-only identity/pin or left pending context")
	}
	raw, err := os.ReadFile(s.d.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "enrollmentGrant") || strings.Contains(string(raw), `"payload"`) || strings.Contains(string(raw), `"signature"`) {
		t.Fatal("short-lived proof was persisted")
	}
	deviceID := cfg.DeviceID
	updateSourceFixture(t, s, id, func(a *config.LocalAccount) { a.Key = "changed-invalid-api-key" })
	cfg = s.d.Store.Get()
	account, _ := cfg.LocalAccount(id)
	body["expectedSourceRevision"] = cfg.AccountSourceRevision(account)
	w = localRequest(s, h, "POST", "/api/remote/discover", body)
	if w.Code != 200 || s.d.Store.Get().DeviceID != deviceID || f.issuerCount != 1 || f.registerCount != 1 {
		t.Fatal("existing device re-enrolled or depended on the rotated API key")
	}
}

func TestRemoteAPIEnrollmentDenialsAndDestinationBinding(t *testing.T) {
	for _, scenario := range []string{"invalid-api", "foreign-issuer", "wrong-device-grant", "expired-grant", "different-input-origin", "different-input-path", "stale-source"} {
		t.Run(scenario, func(t *testing.T) {
			f := newEnrollmentFixture(t)
			s, h := newTestServer(t)
			key := f.apiKey
			if scenario == "invalid-api" {
				key = "invalid-key"
			}
			_, body := enrollmentTestRequest(t, s, h, key)
			switch scenario {
			case "foreign-issuer":
				f.issuerURL = "https://unknown.fixture.example/enrollment"
			case "wrong-device-grant":
				f.grantMutation = "device"
			case "expired-grant":
				f.grantMutation = "expired"
			case "different-input-origin":
				body["url"] = "https://unknown.fixture.example/v1"
			case "different-input-path":
				body["url"] = enrollmentFixtureOrigin + "/other/v1"
			case "stale-source":
				body["expectedSourceRevision"] = "stale"
			}
			before := s.d.Store.Get()
			w := localRequest(s, h, "POST", "/api/remote/discover", body)
			if w.Code == 200 || f.registerCount != 0 || len(s.d.Store.Get().RemoteConnections) != 0 || config.RemoteIdentity(before) != config.RemoteIdentity(s.d.Store.Get()) {
				t.Fatalf("invalid enrollment was admitted: %d %s", w.Code, w.Body)
			}
			if scenario == "foreign-issuer" && f.issuerCount != 0 {
				t.Fatal("key sent to an untrusted issuer")
			}
			if strings.Contains(w.Body.String(), key) {
				t.Fatal("error exposed API key")
			}
		})
	}
}

func TestRemoteAPIEnrollmentLostOrInvalidAckRecoversSameIdentity(t *testing.T) {
	for _, ack := range []string{"drop", "{}", `{"ok":false,"device":{"deviceId":"fake"}}`, `{"ok":true,"device":{"deviceId":"another-device"}}`, "not-json", strings.Repeat("x", (256<<10)+1)} {
		t.Run(fmt.Sprintf("ack-%d-%s", len(ack), ack[:min(len(ack), 12)]), func(t *testing.T) {
			f := newEnrollmentFixture(t)
			if ack == "drop" {
				f.dropAck = true
			} else {
				f.ack = ack
			}
			s, h := newTestServer(t)
			_, body := enrollmentTestRequest(t, s, h, f.apiKey)
			w := localRequest(s, h, "POST", "/api/remote/discover", body)
			cfg := s.d.Store.Get()
			if w.Code != 409 || !strings.Contains(w.Body.String(), "enrollment_result_unknown") || len(cfg.RemoteConnections) != 0 || len(cfg.RemoteEnrollmentAttempts) != 1 || !cfg.RemoteEnrollmentAttempts[0].Submitted {
				t.Fatalf("invalid acknowledgement became success: %d %s", w.Code, w.Body)
			}
			pendingID := cfg.RemoteEnrollmentAttempts[0].Connection.DeviceID
			state := localRequest(s, h, "GET", "/api/state", nil)
			if strings.Contains(state.Body.String(), cfg.RemoteEnrollmentAttempts[0].Connection.DeviceSecret) {
				t.Fatal("pending device secret leaked to local UI state")
			}
			w = localRequest(s, h, "POST", "/api/remote/discover", body)
			if w.Code != 200 || s.d.Store.Get().DeviceID != pendingID || f.registerCount != 1 || f.challengeCount != 1 || f.issuerCount != 1 {
				t.Fatalf("unknown result was resent or lost identity: %d %s", w.Code, w.Body)
			}
		})
	}
}

func TestRemoteAPIEnrollmentLateIssuerCannotSaveAfterSourceEdit(t *testing.T) {
	f := newEnrollmentFixture(t)
	f.issuerEntered, f.issuerRelease = make(chan struct{}), make(chan struct{})
	s, h := newTestServer(t)
	id, body := enrollmentTestRequest(t, s, h, f.apiKey)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- localRequest(s, h, "POST", "/api/remote/discover", body) }()
	<-f.issuerEntered
	updateSourceFixture(t, s, id, func(a *config.LocalAccount) { a.Key = "edited-key" })
	before := s.d.Store.Get()
	close(f.issuerRelease)
	w := <-done
	if w.Code != 409 || f.registerCount != 0 || !reflect.DeepEqual(before, s.d.Store.Get()) {
		t.Fatal("late issuer response survived source replacement")
	}
}

func TestRemoteAPIEnrollmentUnknownDoesNotResubmitAndSourceEditCannotAdoptPending(t *testing.T) {
	f := newEnrollmentFixture(t)
	f.dropAck, f.noCommit = true, true
	s, h := newTestServer(t)
	id, body := enrollmentTestRequest(t, s, h, f.apiKey)
	first := localRequest(s, h, "POST", "/api/remote/discover", body)
	if first.Code != 409 || len(s.d.Store.Get().RemoteEnrollmentAttempts) != 1 {
		t.Fatal("unknown enrollment did not retain its identity")
	}
	pendingID := s.d.Store.Get().RemoteEnrollmentAttempts[0].Connection.DeviceID
	for i := 0; i < 2; i++ {
		w := localRequest(s, h, "POST", "/api/remote/discover", body)
		if w.Code != 409 || f.registerCount != 1 || f.challengeCount != 1 || s.d.Store.Get().RemoteEnrollmentAttempts[0].Connection.DeviceID != pendingID {
			t.Fatal("ordinary reconnect resubmitted an unknown mutation")
		}
	}
	updateSourceFixture(t, s, id, func(a *config.LocalAccount) { a.Key = "different-api-key" })
	cfg := s.d.Store.Get()
	account, _ := cfg.LocalAccount(id)
	body["expectedSourceRevision"] = cfg.AccountSourceRevision(account)
	w := localRequest(s, h, "POST", "/api/remote/discover", body)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "source_changed") || f.issuerCount != 1 || f.registerCount != 1 {
		t.Fatal("edited source adopted or replaced an unknown registration")
	}
}

func TestRemoteAPIEnrollmentPendingSurvivesRestartWithoutNewDevice(t *testing.T) {
	f := newEnrollmentFixture(t)
	f.dropAck = true
	s, h := newTestServer(t)
	id, body := enrollmentTestRequest(t, s, h, f.apiKey)
	if w := localRequest(s, h, "POST", "/api/remote/discover", body); w.Code != 409 {
		t.Fatal("expected lost reply")
	}
	pendingID := s.d.Store.Get().RemoteEnrollmentAttempts[0].Connection.DeviceID
	reopened, err := config.Open(s.d.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	reopened.SetRuntimeGatewayKey("synthetic-restarted-runtime")
	restarted := New(Deps{Store: reopened, Version: "test", Port: 47831})
	t.Cleanup(restarted.CloseUsageLedger)
	cfg := reopened.Get()
	account, _ := cfg.LocalAccount(id)
	body["expectedSourceRevision"] = cfg.AccountSourceRevision(account)
	w := localRequest(restarted, restarted.Handler(), "POST", "/api/remote/discover", body)
	if w.Code != 200 || reopened.Get().DeviceID != pendingID || f.registerCount != 1 || f.issuerCount != 1 {
		t.Fatalf("restart lost pending identity: %d %s", w.Code, w.Body)
	}
}

func TestRemoteAPIEnrollmentExplicitRetryNeedsPositiveChallengeAndKeepsDevice(t *testing.T) {
	for _, exists := range []bool{false, true} {
		name := "absent"
		if exists {
			name = "existing-wrong-secret"
		}
		t.Run(name, func(t *testing.T) {
			f := newEnrollmentFixture(t)
			f.dropAck, f.noCommit = true, true
			s, h := newTestServer(t)
			_, body := enrollmentTestRequest(t, s, h, f.apiKey)
			if w := localRequest(s, h, "POST", "/api/remote/discover", body); w.Code != 409 {
				t.Fatal("expected unknown first registration")
			}
			pending := s.d.Store.Get().RemoteEnrollmentAttempts[0]
			if exists {
				f.devices[pending.Connection.DeviceID] = "another-secret"
			}
			f.dropAck, f.noCommit = false, false
			body["retryEnrollment"] = true
			w := localRequest(s, h, "POST", "/api/remote/discover", body)
			if exists {
				if w.Code != 409 || !strings.Contains(w.Body.String(), "enrollment_request_conflict") || f.issuerCount != 1 || f.registerCount != 1 || len(s.d.Store.Get().RemoteConnections) != 0 {
					t.Fatalf("ambiguous policy failure bypassed challenge guard: %d %s", w.Code, w.Body)
				}
				return
			}
			cfg := s.d.Store.Get()
			if w.Code != 200 || cfg.DeviceID != pending.Connection.DeviceID || cfg.DeviceSecret != pending.Connection.DeviceSecret || f.challengeCount != 2 || f.issuerCount != 2 || f.registerCount != 2 || f.challenge.RequestID == pending.RequestID {
				t.Fatalf("explicit retry lost original device or request boundary: %d %s", w.Code, w.Body)
			}
		})
	}
}

func TestRemoteAPIEnrollmentCorrectedSameSourceKeepsUnsubmittedIdentity(t *testing.T) {
	for _, scenario := range []string{"invalid-key", "definitive-register-refusal"} {
		t.Run(scenario, func(t *testing.T) {
			f := newEnrollmentFixture(t)
			s, h := newTestServer(t)
			key := "synthetic-invalid-key"
			if scenario == "definitive-register-refusal" {
				key = f.apiKey
				f.rejectRegister = true
			}
			id, body := enrollmentTestRequest(t, s, h, key)
			w := localRequest(s, h, "POST", "/api/remote/discover", body)
			if w.Code != 401 && w.Code != 403 {
				t.Fatalf("expected explicit refusal: %d %s", w.Code, w.Body)
			}
			pending := s.d.Store.Get().RemoteEnrollmentAttempts[0]
			if pending.Submitted {
				t.Fatal("definitively refused request remained unknown")
			}
			f.rejectRegister = false
			f.apiKey = "synthetic-corrected-valid-key"
			updateSourceFixture(t, s, id, func(a *config.LocalAccount) { a.Key = f.apiKey })
			cfg := s.d.Store.Get()
			account, _ := cfg.LocalAccount(id)
			body["expectedSourceRevision"] = cfg.AccountSourceRevision(account)
			w = localRequest(s, h, "POST", "/api/remote/discover", body)
			cfg = s.d.Store.Get()
			if w.Code != 200 || cfg.DeviceID != pending.Connection.DeviceID || cfg.DeviceSecret != pending.Connection.DeviceSecret || len(cfg.RemoteEnrollmentAttempts) != 0 || f.challenge.RequestID == pending.RequestID {
				t.Fatalf("correcting API configuration lost prepared identity: %d %s", w.Code, w.Body)
			}
		})
	}
}
