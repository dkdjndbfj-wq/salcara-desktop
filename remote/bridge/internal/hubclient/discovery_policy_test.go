package hubclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func policyFixture() *config.StationObservation {
	return &config.StationObservation{Mode: "restricted", Revision: "device-policy", Station: &protocol.StationPolicy{Version: 1, APIBases: []string{"https://api.fixture/tenant"}}}
}

func TestDiscoveryScopedPolicyIsSeparateAndFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name, id, secret, claim, code string
		status                        int
	}{
		{"saved device", "device", "secret", "", "", 200},
		{"claim only", "", "", "claim", "", 200},
		{"claim replaces device mode", "retired", "retired-secret", "claim", "", 200},
		{"no identity", "", "", "", "device_credentials_required", 401},
		{"revoked", "device", "secret", "", "device_credentials_invalid", 403},
		{"policy unavailable", "device", "secret", "", "remote_unavailable", 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ping, policy atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
					t.Error("model credentials sent")
				}
				switch r.URL.Path {
				case "/tenant/salcara-hub/v1/ping":
					ping.Add(1)
					for _, header := range []string{"X-Salcara-Device-Id", "X-Salcara-Device-Secret", "X-Salcara-Enrollment-Claim"} {
						if r.Header.Get(header) != "" {
							t.Error("ping was authenticated")
						}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"service": "salcara-hub", "protocol": "salcara-remote", "protocolVersion": 1, "authModes": []string{"device-pairing"}, "capabilities": []string{"pair.qr.v1", "pair.revoke.v1", "device.identity.v1", "session.remote.v1", "station.guard.v1", "discovery.policy.v1"}, "remoteAccess": policyFixture()})
				case "/tenant/salcara-hub/v1/discovery/policy":
					policy.Add(1)
					wantID, wantSecret := tc.id, tc.secret
					if tc.claim != "" {
						wantID, wantSecret = "", ""
					}
					if r.Header.Get("X-Salcara-Device-Id") != wantID || r.Header.Get("X-Salcara-Device-Secret") != wantSecret || r.Header.Get("X-Salcara-Enrollment-Claim") != tc.claim {
						t.Error("credential scopes mixed")
					}
					if tc.status != 200 {
						w.WriteHeader(tc.status)
						_ = json.NewEncoder(w).Encode(map[string]string{"code": tc.code, "error": "private upstream response"})
						return
					}
					scope := "device"
					if tc.claim != "" {
						scope = "enrollment"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "scope": scope, "remoteAccess": policyFixture()})
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer srv.Close()
			info, err := DiscoverForDevice(context.Background(), srv.URL+"/tenant/v1", "", tc.id, tc.secret, tc.claim)
			if ping.Load() != 1 || info.ProtocolVersion != 1 {
				t.Fatal("service discovery lost on policy failure")
			}
			if tc.code == "" {
				if err != nil || !info.RemoteAccess.Available() || policy.Load() != 1 {
					t.Fatalf("valid policy missing: %v", err)
				}
			} else if RemoteErrorCode(err) != tc.code || info.RemoteAccess != nil || strings.Contains(err.Error(), "private") {
				t.Fatalf("policy failed open or exposed body: %v", err)
			}
			if tc.id == "" && tc.claim == "" && policy.Load() != 0 {
				t.Error("anonymous policy probe sent")
			}
		})
	}
}

func TestDiscoveryTargetsPreserveSafePrefixes(t *testing.T) {
	for _, tc := range []struct {
		raw, override string
		want          []string
	}{
		{"https://api.fixture/v1", "", []string{"https://api.fixture/salcara-hub", "https://api.fixture"}},
		{"https://api.fixture/a/v1", "", []string{"https://api.fixture/a/salcara-hub", "https://api.fixture/salcara-hub", "https://api.fixture/a"}},
		{"https://api.fixture/a/salcara-hub/v1", "", []string{"https://api.fixture/a/salcara-hub"}},
		{"https://api.fixture/a/v1", "https://api.fixture/plugin/v1", []string{"https://api.fixture/plugin"}},
	} {
		got, err := DiscoveryTargets(tc.raw, tc.override)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s: %v %v", tc.raw, got, err)
		}
	}
	for _, path := range []string{"/a/../v1", "/a//v1", "/a/%2e%2e/v1", "/a/%252f/v1", "/a\\b/v1", "/v1?", "/salcara-hub:evil"} {
		if _, err := DiscoveryTargets("https://api.fixture"+path, ""); err == nil {
			t.Fatalf("unsafe path accepted %s", path)
		}
	}
}

func TestAnonymousDiscoveryFallsBackOnlyOnMissingPrefix(t *testing.T) {
	for _, status := range []int{404, 403, 503, 302, 200} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if strings.HasPrefix(r.URL.Path, "/tenant/") {
					w.WriteHeader(status)
					_, _ = w.Write([]byte("<html>public status</html>"))
					return
				}
				_, _ = w.Write([]byte(`{"service":"salcara-hub","protocol":"salcara-remote","protocolVersion":1,"authModes":["device-pairing"],"capabilities":["device.identity.v1","pair.qr.v1","session.remote.v1","pair.revoke.v1"]}`))
			}))
			defer srv.Close()
			_, err := Discover(context.Background(), srv.URL+"/tenant/v1", "")
			want := int32(1)
			if status == 404 {
				want = 2
			}
			if calls.Load() != want || (err == nil) != (status == 404) {
				t.Fatal("unexpected fallback or accepted HTML")
			}
		})
	}
}

func TestFirstDiscoveryFindsCustomAPIPathWithoutOverride(t *testing.T) {
	for _, prefix := range []string{"/plugin", ""} {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			for _, h := range []string{"Authorization", "X-Salcara-Device-Id", "X-Salcara-Device-Secret", "X-Salcara-Enrollment-Claim"} {
				if r.Header.Get(h) != "" {
					t.Error("first discovery sent credentials")
				}
			}
			if r.URL.Path != prefix+"/v1/ping" {
				w.WriteHeader(404)
				return
			}
			_, _ = w.Write([]byte(`{"service":"salcara-hub","protocol":"salcara-remote","protocolVersion":1,"authModes":["device-pairing"],"capabilities":["device.identity.v1","pair.qr.v1","session.remote.v1","pair.revoke.v1"]}`))
		}))
		info, err := Discover(context.Background(), srv.URL+prefix+"/v1", "")
		srv.Close()
		wantCalls := int32(3)
		if prefix == "" {
			wantCalls = 2
		}
		if err != nil || info.HubURL != srv.URL+prefix || calls.Load() != wantCalls {
			t.Fatalf("custom first discovery lost path: %+v %v calls=%d", info, err, calls.Load())
		}
	}
}

func TestSavedHubAndBotUseSeparateDiscoveryDeviceScopes(t *testing.T) {
	var expectedID, expectedSecret string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/discovery/policy") {
			if r.Header.Get("X-Salcara-Device-Id") != expectedID || r.Header.Get("X-Salcara-Device-Secret") != expectedSecret {
				t.Error("Hub and Bot mixed device policies")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "scope": "device", "remoteAccess": policyFixture()})
			return
		}
		if r.Header.Get("X-Salcara-Device-Id") != "" || r.Header.Get("X-Salcara-Device-Secret") != "" {
			t.Error("ping received device credentials")
		}
		_, _ = w.Write([]byte(`{"service":"salcara-hub","protocol":"salcara-remote","protocolVersion":1,"authModes":["device-pairing"],"capabilities":["device.identity.v1","pair.qr.v1","session.remote.v1","pair.revoke.v1","discovery.policy.v1","station.guard.v1"]}`))
	}))
	defer srv.Close()
	cfg := config.Config{}
	cfg.ConnectRemote(srv.URL, srv.URL+"/salcara-hub")
	cfg.ConnectRemoteWithBotBinding(srv.URL, srv.URL+"/salcara-hub", "group", "", "fixture-signature")
	for _, saved := range cfg.RemoteConnections {
		expectedID, expectedSecret = saved.DeviceID, saved.DeviceSecret
		info, err := DiscoverForConnection(context.Background(), cfg, saved)
		if err != nil || !info.RemoteAccess.Available() {
			t.Fatalf("saved mode/group could not discover its own effective policy: %v", err)
		}
	}
}
