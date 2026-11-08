package hubclient

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/config"
)

func issuerGrantTestFixture(t *testing.T, alter func(*sourceGrantSnapshot)) (sourceIssuer, sourceGrantSnapshot, *http.Client) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	expected := sourceGrantSnapshot{Version: 1, Issuer: "issuer", Audience: "hub", DeviceID: "pc", BindingID: "binding", CredentialHandle: "handle", Agent: "codex", ControlSurface: "cli", SourceType: "site_api", Origin: "https://api.test", APIBase: "https://api.test/tenant", ConfigRevision: "cfg", PolicyRevision: "p", CommandHash: strings.Repeat("a", 64), RequestID: "10000000-0000-4000-8000-000000000001", ChallengeID: "challenge-123", Model: "gpt"}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Salcara-Device-Secret") != "" || r.Header.Get("Authorization") != "" || r.Header.Get("X-Salcara-Pair-Token") != "" {
			t.Error("Hub/device/phone credential crossed issuer connection")
		}
		var input struct {
			Version    int                 `json:"version"`
			Snapshot   sourceGrantSnapshot `json:"snapshot"`
			Credential struct {
				Type  string `json:"type"`
				Value string `json:"value"`
			} `json:"credential"`
		}
		if json.NewDecoder(r.Body).Decode(&input) != nil || input.Version != 1 || input.Credential.Type != "api_key" || input.Credential.Value != "fixture-api-key" || input.Snapshot != expected {
			t.Error("bad direct issuance contract")
		}
		actual := input.Snapshot
		actual.IssuedAt = time.Now().Unix()
		actual.ExpiresAt = actual.IssuedAt + 60
		if alter != nil {
			alter(&actual)
		}
		payload, _ := json.Marshal(actual)
		grant := signedSourceGrant{Payload: base64.RawURLEncoding.EncodeToString(payload), Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, payload))}
		_ = json.NewEncoder(w).Encode(grant)
	}))
	t.Cleanup(server.Close)
	expected.Origin, expected.APIBase = server.URL, server.URL+"/tenant"
	return sourceIssuer{ID: "issuer", AuthorizationURL: server.URL + "/grants", PublicKey: base64.RawURLEncoding.EncodeToString(pub)}, expected, server.Client()
}

func TestSourceIssuerDirectCredentialAndExactSignedChallenge(t *testing.T) {
	issuer, expected, httpClient := issuerGrantTestFixture(t, nil)
	grant, err := requestSourceIssuer(context.Background(), issuer, expected, "fixture-api-key", httpClient.Transport)
	if err != nil || grant.Payload == "" {
		t.Fatalf("valid grant rejected: %v", err)
	}
}

func TestSourceIssuerRejectsSignedDifferentNonceRouteAndConfiguration(t *testing.T) {
	for _, name := range []string{"nonce", "route", "revision", "command", "request", "binding", "ttl"} {
		t.Run(name, func(t *testing.T) {
			issuer, expected, client := issuerGrantTestFixture(t, func(s *sourceGrantSnapshot) {
				switch name {
				case "nonce":
					s.ChallengeID = "another"
				case "route":
					s.APIBase = "https://api.test/another"
				case "revision":
					s.ConfigRevision = "new"
				case "command":
					s.CommandHash = strings.Repeat("b", 64)
				case "request":
					s.RequestID = "different"
				case "binding":
					s.BindingID = "different"
				case "ttl":
					s.ExpiresAt = s.IssuedAt + 301
				}
			})
			if _, err := requestSourceIssuer(context.Background(), issuer, expected, "fixture-api-key", client.Transport); err == nil {
				t.Fatal("mismatched signed grant accepted")
			}
		})
	}
}

func TestSourceIssuerRejectsRedirectWithoutForwardingCredential(t *testing.T) {
	var forwarded bool
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { forwarded = true }))
	defer target.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(307)
	}))
	defer server.Close()
	if _, err := requestSourceIssuer(context.Background(), sourceIssuer{AuthorizationURL: server.URL}, sourceGrantSnapshot{Origin: server.URL}, "fixture-api-key", server.Client().Transport); err == nil {
		t.Fatal("redirect accepted")
	}
	if forwarded {
		t.Fatal("credential followed redirect")
	}
}

type sourceGrantRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn sourceGrantRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func TestSourceIssuerForeignURLCannotObtainCredentialDespiteMatchingHubScopes(t *testing.T) {
	called := false
	issuer := sourceIssuer{ID: "hub-configured", AuthorizationURL: "https://hub-attacker.test/grants", SourceTypes: []string{"site_api"}, Origins: []string{"https://actual-provider.test"}, APIBases: []string{"https://actual-provider.test/tenant"}}
	expected := sourceGrantSnapshot{Issuer: issuer.ID, SourceType: "site_api", Origin: "https://actual-provider.test", APIBase: "https://actual-provider.test/tenant"}
	transport := sourceGrantRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		called = true
		return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader("denied"))}, nil
	})
	if _, err := requestSourceIssuer(context.Background(), issuer, expected, "fixture-provider-private-key", transport); err == nil {
		t.Fatal("foreign issuer accepted")
	}
	if called {
		t.Fatal("foreign URL was contacted with provider credential despite only Hub-controlled scope claims")
	}
	if sourceIssuerSameOrigin(issuer.AuthorizationURL, expected.Origin) {
		t.Fatal("foreign issuer selected by scope claims")
	}
}

func TestSourceGrantCannotBeAcquiredByLocalOrCallerOnlyDispatch(t *testing.T) {
	c, _, _, _ := sourceGuardFixture(t, "api_key")
	if _, err := c.Dispatch(context.Background(), map[string]any{"type": "source.grant.acquire", "challengeId": "challenge-caller", "issuerURL": "https://attacker.test"}); err == nil {
		t.Fatal("caller-only issuance bypass")
	}
}

func TestSourceGrantExactParserRejectsAmbiguousEnvelope(t *testing.T) {
	for _, raw := range []string{`{"payload":"a","payload":"b","signature":"c"}`, `{"payload":"a","Signature":"c"}`, `{"payload":"a","signature":null}`, `{"payload":"a","signature":"c","issuerURL":"https://attacker.test"}`, `{"payload":"a","signature":"c"} {}`} {
		if exactSourceJSON([]byte(raw), []string{"payload", "signature"}, &signedSourceGrant{}) == nil {
			t.Fatalf("ambiguous response accepted: %s", raw)
		}
	}
}

func TestSourceChallengeDiscoveryParserRejectsAmbiguousPolicyAndNonce(t *testing.T) {
	challenge := `{"challengeId":"challenge-a","audience":"hub","policyRevision":"r1","deviceId":"pc","bindingId":"binding","requestId":"10000000-0000-4000-8000-000000000001","commandHash":"hash","command":{"type":"session.send"},"expiresAt":9999999999}`
	policy := `{"mode":"restricted","revision":"r1","audience":"hub","issuers":[]}`
	valid := `{"challenge":` + challenge + `,"policy":` + policy + `}`
	if _, _, err := parseSourceGrantChallengeResponse([]byte(valid)); err != nil {
		t.Fatal("valid discovery rejected")
	}
	for _, raw := range []string{
		strings.Replace(valid, `"challengeId":"challenge-a"`, `"challengeId":"challenge-a","challengeId":"challenge-b"`, 1),
		strings.Replace(valid, `"mode":"restricted"`, `"mode":"restricted","Mode":"open"`, 1),
		strings.Replace(valid, `"revision":"r1"`, `"revision":null`, 1),
		strings.Replace(valid, `"issuers":[]`, `"issuers":[{"id":"i","authorizationUrl":"https://good.test","AuthorizationURL":"https://bad.test"}]`, 1),
		valid + ` {}`,
	} {
		if _, _, err := parseSourceGrantChallengeResponse([]byte(raw)); err == nil {
			t.Fatalf("ambiguous discovery accepted: %s", raw)
		}
	}
}

func TestSourceAcquisitionUsesOneLaneWithoutHoldingTaskAdmissionLock(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	c, store, _, _ := sourceGuardFixture(t, "api_key")
	if err := store.Update(func(cfg *config.Config) error {
		cfg.HubURL = server.URL + "/salcara-hub"
		cfg.DeviceID = "pc"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cfg := store.Get()
	ctx := withCommandGuard(context.Background(), commandGuard{identity: config.RemoteIdentity(cfg), deviceID: cfg.DeviceID})
	done := make(chan error, 1)
	go func() { _, err := c.acquireSourceGrant(ctx, "challenge-first"); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("challenge fetch never started")
	}
	if !c.taskConfigMu.TryLock() {
		close(release)
		t.Fatal("acquisition held task/API lock while waiting on network")
	}
	c.taskConfigMu.Unlock()
	if _, err := c.acquireSourceGrant(ctx, "challenge-second"); err == nil {
		close(release)
		t.Fatal("parallel acquisition admitted")
	}
	close(release)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("denied challenge became grant")
		}
	case <-time.After(time.Second):
		t.Fatal("acquisition did not finish")
	}
}
