package hubclient

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func fullEscapedStationSizeFixture() *protocol.StationPolicy {
	p := &protocol.StationPolicy{Version: 2, APIBases: []string{}}
	for i := 0; i < 32; i++ {
		prefix := fmt.Sprintf("https://api.example/team%d/", i)
		base := prefix + strings.Repeat("&", 4096-len(prefix))
		if i < 16 {
			p.APIBases = append(p.APIBases, base)
		} else {
			p.ExternalAPIBases = append(p.ExternalAPIBases, base)
		}
		p.ReverseProxyAPIBases = append(p.ReverseProxyAPIBases, base)
	}
	return p
}

func TestStationPolicySizeEscapedDiscoveryAndChallenge(t *testing.T) {
	p := fullEscapedStationSizeFixture()
	if !config.ValidStationPolicy(p) {
		t.Fatal("full escaped fixture must be valid")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"service": "salcara-hub", "protocol": "salcara-remote", "protocolVersion": 1,
			"authModes": []string{"device-pairing"}, "capabilities": []string{"device.identity.v1", "pair.qr.v1", "session.remote.v1", "pair.revoke.v1", "station.guard.v2"},
			"remoteAccess": &config.StationObservation{Mode: "open", Revision: "r1", Station: p},
		})
	}))
	defer server.Close()
	discovery, err := Discover(context.Background(), server.URL, "")
	if err != nil || discovery.RemoteAccess == nil || !config.StationPolicyEqual(discovery.RemoteAccess.Station, p) {
		t.Fatalf("large discovery rejected: %v", err)
	}
	// A phone command can expand sixfold when serialized by the Hub too.
	command := map[string]any{"type": "session.send", "text": strings.Repeat("&", 240<<10)}
	raw, _ := json.Marshal(map[string]any{"challenge": map[string]any{"challengeId": "challenge-size", "command": command}, "policy": sourceGrantPolicy{Mode: "restricted", Revision: "r1", Audience: "station", Station: p, Issuers: []sourceIssuer{}}})
	if len(raw) <= 2<<20 || len(raw) > protocol.MaxSourcePolicyWireBytes {
		t.Fatalf("unexpected challenge fixture length %d", len(raw))
	}
	challenge, policy, err := parseSourceGrantChallengeResponse(raw)
	if err != nil || !config.StationPolicyEqual(policy.Station, p) || challenge.ChallengeID != "challenge-size" {
		t.Fatalf("large challenge rejected: %v", err)
	}
	if _, _, err := parseSourceGrantChallengeResponse(append(raw, []byte(strings.Repeat(" ", protocol.MaxSourcePolicyWireBytes))...)); err == nil {
		t.Fatal("oversized challenge accepted")
	}
}

func TestSourceGrantSizeEscapedIssuerResponse(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	expected := sourceGrantSnapshot{Version: 1, Origin: "https://api.example", APIBase: fullEscapedStationSizeFixture().APIBases[0]}
	issuer := sourceIssuer{AuthorizationURL: "https://api.example/grants", PublicKey: base64.RawURLEncoding.EncodeToString(pub)}
	response := func(oversized bool) http.RoundTripper {
		return sourceGrantRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			actual := expected
			actual.IssuedAt, actual.ExpiresAt = time.Now().Unix(), time.Now().Unix()+60
			payload, _ := json.Marshal(actual)
			if len(payload) <= 16<<10 {
				t.Fatal("escaped payload did not exceed old cap")
			}
			grant := signedSourceGrant{Payload: base64.RawURLEncoding.EncodeToString(payload), Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, payload))}
			raw, _ := json.Marshal(grant)
			if oversized {
				raw = append(raw, []byte(strings.Repeat(" ", protocol.MaxSignedSourceGrantBytes))...)
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
		})
	}
	if _, err := requestSourceIssuer(context.Background(), issuer, expected, "synthetic", response(false)); err != nil {
		t.Fatal(err)
	}
	if _, err := requestSourceIssuer(context.Background(), issuer, expected, "synthetic", response(true)); err == nil {
		t.Fatal("oversized issuer envelope accepted")
	}
}

func TestStationPolicySizeCommandSSEAndFrameBounds(t *testing.T) {
	envelope := protocol.CommandEnvelope{CommandID: "size-command", Command: map[string]any{"type": "session.send", "text": strings.Repeat("&", 240<<10)}, Source: &protocol.ExecutionSource{Mode: "open", Station: fullEscapedStationSizeFixture()}}
	raw, _ := json.Marshal(envelope)
	wire := "event: command\ndata: " + string(raw) + "\n\n"
	count := 0
	err := readBridgeSSE(strings.NewReader(wire), func() {}, func(name, data string) {
		count++
		var actual protocol.CommandEnvelope
		if name != "command" || json.Unmarshal([]byte(data), &actual) != nil || !config.StationPolicyEqual(actual.Source.Station, envelope.Source.Station) {
			t.Fatal("large source envelope corrupted")
		}
	})
	if !errors.Is(err, io.EOF) || count != 1 {
		t.Fatalf("large SSE failed: count=%d err=%v", count, err)
	}
	for _, oversized := range []string{
		"data: " + strings.Repeat("x", protocol.MaxSourcePolicyWireBytes) + "\n\n",
		strings.Repeat("data: "+strings.Repeat("x", 1024)+"\n", protocol.MaxSourcePolicyWireBytes/1024+1) + "\n",
	} {
		called := false
		err := readBridgeSSE(strings.NewReader(oversized), func() {}, func(string, string) { called = true })
		if !errors.Is(err, errBridgeSSETooLarge) || called {
			t.Fatalf("oversized frame dispatched=%v err=%v", called, err)
		}
	}
	count = 0
	err = readBridgeSSE(strings.NewReader(wire+wire), func() {}, func(string, string) { count++ })
	if !errors.Is(err, io.EOF) || count != 2 {
		t.Fatal("frame budget did not reset between commands")
	}
}
