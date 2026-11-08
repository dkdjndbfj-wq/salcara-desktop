package hubclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func TestStationV2DiscoveryRequiresMatchingGuard(t *testing.T) {
	for _, tc := range []struct {
		version int
		guard   string
		want    bool
	}{
		{1, "station.guard.v1", true}, {1, "station.guard.v2", false},
		{2, "station.guard.v1", false}, {2, "station.guard.v2", true},
	} {
		t.Run(tc.guard+string(rune('0'+tc.version)), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"service": "salcara-hub", "protocol": "salcara-remote", "protocolVersion": 1,
					"authModes":    []string{"device-pairing"},
					"capabilities": []string{"device.identity.v1", "pair.qr.v1", "session.remote.v1", "pair.revoke.v1", tc.guard},
					"remoteAccess": &config.StationObservation{Mode: "open", Revision: "r1", Station: &protocol.StationPolicy{Version: tc.version, APIBases: []string{"https://api.test"}}},
				})
			}))
			defer server.Close()
			info, err := Discover(context.Background(), server.URL, "")
			if err != nil || (info.RemoteAccess != nil) != tc.want {
				t.Fatalf("version=%d guard=%s observation=%+v err=%v", tc.version, tc.guard, info.RemoteAccess, err)
			}
		})
	}
}

func TestStationV2SourceGuardSeparatesAPIAndProxyInBothModes(t *testing.T) {
	for _, mode := range []string{"open", "restricted"} {
		for _, sourceType := range []string{"api_key", "reverse_proxy"} {
			t.Run(mode+"/"+sourceType, func(t *testing.T) {
				client, _, _, envelope := sourceGuardFixture(t, sourceType)
				envelope.Source.Mode = mode
				policy := &protocol.StationPolicy{Version: 2, APIBases: []string{}, ExternalAPIBases: []string{"https://api.example.test"}}
				if sourceType == "reverse_proxy" {
					envelope.Source.SourceType = "verified_proxy"
					policy.ExternalAPIBases, policy.ReverseProxyAPIBases = nil, []string{"https://api.example.test"}
				}
				envelope.Source.Station = policy
				if err := client.validateExecutionSource(envelope); err != nil {
					t.Fatalf("permitted class rejected: %v", err)
				}
				policy.ExternalAPIBases, policy.ReverseProxyAPIBases = policy.ReverseProxyAPIBases, policy.ExternalAPIBases
				if err := client.validateExecutionSource(envelope); err == nil {
					t.Fatal("other source class inherited this address grant")
				}
			})
		}
	}
}

func TestStationV2EligibilityExplainsClassRestrictions(t *testing.T) {
	for _, tc := range []struct{ sourceType, scope, reason string }{
		{"api_key", "own", "supported"}, {"api_key", "external", "supported"}, {"api_key", "proxy", "external_api_not_allowed"},
		{"reverse_proxy", "proxy", "supported"}, {"reverse_proxy", "own", "reverse_proxy_not_allowed"}, {"reverse_proxy", "external", "reverse_proxy_not_allowed"},
	} {
		_, store, account, _ := sourceGuardFixture(t, tc.sourceType)
		policy := &protocol.StationPolicy{Version: 2, APIBases: []string{}}
		switch tc.scope {
		case "own":
			policy.APIBases = []string{"https://api.example.test"}
		case "external":
			policy.ExternalAPIBases = []string{"https://api.example.test"}
		case "proxy":
			policy.ReverseProxyAPIBases = []string{"https://api.example.test"}
		}
		cfg := store.Get()
		cfg.RemoteStationObservation = &config.StationObservation{Mode: "open", Revision: "r2", Station: policy, CheckedAt: time.Now().UnixMilli()}
		result := remoteEligibility(cfg, account)
		if result.Reason != tc.reason || result.Supported != (tc.reason == "supported") || !result.APIRemote {
			t.Fatalf("%s/%s: %+v", tc.sourceType, tc.scope, result)
		}
	}
}

func TestStationV2ChallengeParsingUsesStrictVersionContract(t *testing.T) {
	wrap := func(station string) []byte {
		return []byte(`{"challenge":{"challengeId":"challenge-a"},"policy":{"mode":"restricted","revision":"r1","audience":"hub","issuers":[],"station":` + station + `}}`)
	}
	valid := `{"version":2,"apiBases":[],"externalApiBases":["https://external.test"],"reverseProxyApiBases":["https://proxy.test"],"allowOfficialAccounts":false}`
	_, policy, err := parseSourceGrantChallengeResponse(wrap(valid))
	if err != nil || policy.Station == nil || len(policy.Station.ExternalAPIBases) != 1 || len(policy.Station.ReverseProxyAPIBases) != 1 {
		t.Fatalf("valid v2 rejected: %v", err)
	}
	for _, raw := range []string{
		strings.Replace(valid, `"version":2`, `"version":1`, 1),
		strings.Replace(valid, `"version":2`, `"version":3`, 1),
		strings.Replace(valid, `"version":2`, `"version":2,"allowAllAccounts":true`, 1),
		strings.Replace(valid, `"https://external.test"`, `"https://external.test/../bypass"`, 1),
		`{"version":1,"apiBases":[],"externalApiBases":[]}`,
		`{"version":2,"apiBases":[],"reverseProxyApiBases":null}`,
	} {
		if _, _, err := parseSourceGrantChallengeResponse(wrap(raw)); err == nil {
			t.Errorf("incompatible grant scope accepted: %s", raw)
		}
	}
}

func TestStationV2RecommendationsFilterEachAccountBeforeAggregateHints(t *testing.T) {
	cfg := config.Config{ComputerID: "physical-pc"}
	policies := []protocol.StationPolicy{
		{Version: 2, APIBases: []string{}, ExternalAPIBases: []string{"https://shared.test"}},
		{Version: 2, APIBases: []string{}, ReverseProxyAPIBases: []string{"https://shared.test"}},
		{Version: 2, APIBases: []string{"https://unrelated.test"}},
	}
	for i := range policies {
		cfg.RemoteConnections = append(cfg.RemoteConnections, config.RemoteConnection{
			HubURL: "https://hub" + string(rune('0'+i)) + ".test/salcara-hub", DeviceID: "pc", DeviceSecret: "synthetic-device-secret",
			RemoteSupport: &config.StationObservation{Mode: "open", Revision: "r2", CheckedAt: time.Now().UnixMilli(), Station: &policies[i]},
		})
	}
	for i, sourceType := range []string{"api_key", "reverse_proxy"} {
		account := config.LocalAccount{SourceType: sourceType, BaseURL: "https://shared.test/v1"}
		matches := stationsForAPI(cfg, account)
		if len(matches) != 1 || matches[0].HubURL != cfg.RemoteConnections[i].HubURL || !matches[0].APIRemote {
			t.Fatalf("%s received unpermitted station recommendation: %+v", sourceType, matches)
		}
	}
}
