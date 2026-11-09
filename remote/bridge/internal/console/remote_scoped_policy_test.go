package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
)

func TestRemoteDiscoveryKeepsServiceDetectionWhenDeviceIsRevoked(t *testing.T) {
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("model key reached Hub")
		}
		if strings.HasSuffix(r.URL.Path, "/ping") {
			if r.Header.Get("X-Salcara-Device-Secret") != "" || r.Header.Get("X-Salcara-Enrollment-Claim") != "" {
				t.Error("ping received device auth")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"service": "salcara-hub", "protocol": "salcara-remote", "protocolVersion": 1, "authModes": []string{"device-pairing"}, "capabilities": []string{"pair.qr.v1", "pair.revoke.v1", "device.identity.v1", "session.remote.v1", "station.guard.v1", "discovery.policy.v1"}, "remoteAccess": remoteSupportObservation(upstream.URL, true)})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/discovery/policy") {
			if r.Header.Get("X-Salcara-Enrollment-Claim") == strings.Repeat("c", 64) {
				if r.Header.Get("X-Salcara-Device-Id") != "" || r.Header.Get("X-Salcara-Device-Secret") != "" {
					t.Error("claim mixed with retired credentials")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "scope": "enrollment", "remoteAccess": remoteSupportObservation(upstream.URL, true)})
				return
			}
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":"device_credentials_invalid","error":"private upstream text"}`))
			return
		}
		t.Errorf("unexpected endpoint %s", r.URL.Path)
	}))
	defer upstream.Close()
	s, h := newTestServer(t)
	s.d.Store.SetRuntimeGatewayKey("synthetic-runtime")
	id := createLocal(t, s, h, "api", "Working API", "synthetic-model-key", upstream.URL+"/v1")
	_ = s.d.Store.Update(func(c *config.Config) error { c.ConnectRemote(upstream.URL, upstream.URL+"/salcara-hub"); return nil })
	before := s.d.Store.Get()
	probe := localRequest(s, h, http.MethodPost, "/api/remote/discover", map[string]any{"url": upstream.URL})
	var result struct {
		OK     bool `json:"ok"`
		Plugin struct {
			ProtocolVersion int `json:"protocolVersion"`
		} `json:"plugin"`
		ErrorCode   string `json:"errorCode"`
		DeviceError string `json:"deviceError"`
	}
	_ = json.Unmarshal(probe.Body.Bytes(), &result)
	if probe.Code != 200 || !result.OK || result.Plugin.ProtocolVersion != 1 || result.ErrorCode != "device_credentials_invalid" || result.DeviceError == "" || strings.Contains(probe.Body.String(), "private") {
		t.Fatal("device revocation hid service discovery or exposed response")
	}
	after := s.d.Store.Get()
	if after.RemoteConnections[0].RemoteSupport.Available() || config.RemoteIdentity(before) != config.RemoteIdentity(after) || !reflect.DeepEqual(before.LocalAccounts, after.LocalAccounts) {
		t.Fatal("device error retained global authority or damaged ordinary API")
	}
	failed := localRequest(s, h, http.MethodPost, "/api/remote/discover", map[string]any{"url": upstream.URL, "connect": true})
	if failed.Code != 403 || !strings.Contains(failed.Body.String(), "device_credentials_invalid") {
		t.Fatal("revoked device connected without recovery")
	}
	recover := localRequest(s, h, http.MethodPost, "/api/remote/discover", map[string]any{"url": upstream.URL, "connect": true, "reenroll": true, "enrollmentClaim": strings.Repeat("c", 64)})
	after = s.d.Store.Get()
	if recover.Code != 200 || after.DeviceID == before.DeviceID || len(after.RemoteConnections) != 1 {
		t.Fatal("explicit recovery did not rotate the existing device")
	}
	a, exists := after.LocalAccount(id)
	if !exists || a.Key != "synthetic-model-key" || !reflect.DeepEqual(before.Projects, after.Projects) {
		t.Fatal("reenrollment changed ordinary API/projects")
	}
}

func TestHubDiscoveryDoesNotOverwriteSameURLBotObservation(t *testing.T) {
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remoteSupportFixturePing(w, remoteSupportObservation(upstream.URL+"/hub-api", false), true)
	}))
	defer upstream.Close()
	s, h := newTestServer(t)
	_ = s.d.Store.Update(func(c *config.Config) error {
		c.ConnectRemote(upstream.URL, upstream.URL+"/salcara-hub")
		c.ConnectRemoteWithBotBinding(upstream.URL, upstream.URL+"/salcara-hub", "group", "invite", "signature")
		c.RemoteConnections[1].RemoteSupport = remoteSupportObservation(upstream.URL+"/bot-api", true)
		c.RemoteStationObservation = config.CloneStationObservation(c.RemoteConnections[1].RemoteSupport)
		return nil
	})
	before := s.d.Store.Get()
	result := localRequest(s, h, http.MethodPost, "/api/remote/discover", map[string]any{"url": upstream.URL})
	after := s.d.Store.Get()
	if result.Code != 200 || !reflect.DeepEqual(before.RemoteConnections[1], after.RemoteConnections[1]) || !reflect.DeepEqual(before.RemoteStationObservation, after.RemoteStationObservation) {
		t.Fatal("Hub detection changed the Bot's device policy or active authority")
	}
	if after.RemoteConnections[0].RemoteSupport.Station.AllowOfficialAccounts {
		t.Fatal("Hub detection adopted the Bot's broader policy")
	}
}

func TestRemoteDiscoveryCannotCommitAfterPhoneStationABA(t *testing.T) {
	for _, connect := range []bool{false, true} {
		entered, release := make(chan struct{}), make(chan struct{})
		var upstream *httptest.Server
		upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			<-release
			remoteSupportFixturePing(w, remoteSupportObservation(upstream.URL, true), true)
		}))
		s, h := newTestServer(t)
		_ = s.d.Store.Update(func(c *config.Config) error { c.ConnectRemote(upstream.URL, upstream.URL+"/salcara-hub"); return nil })
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			done <- localRequest(s, h, http.MethodPost, "/api/remote/discover", map[string]any{"url": upstream.URL, "connect": connect})
		}()
		<-entered
		_ = s.d.Store.Update(func(c *config.Config) error {
			c.ConnectRemote("https://other.fixture", "https://other.fixture/salcara-hub")
			return nil
		})
		_ = s.d.Store.Update(func(c *config.Config) error { c.ConnectRemote(upstream.URL, upstream.URL+"/salcara-hub"); return nil })
		before := s.d.Store.Get()
		close(release)
		result := <-done
		upstream.Close()
		if result.Code != http.StatusConflict || !reflect.DeepEqual(before, s.d.Store.Get()) {
			t.Fatal("a delayed desktop probe overwrote a newer phone station selection")
		}
	}
}
