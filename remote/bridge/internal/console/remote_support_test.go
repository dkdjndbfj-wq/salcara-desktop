package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func remoteSupportFixturePing(w http.ResponseWriter, observation *config.StationObservation, guard bool) {
	capabilities := []string{"pair.qr.v1", "device.identity.v1", "session.remote.v1", "pair.revoke.v1"}
	if guard {
		capabilities = append(capabilities, "station.guard.v1")
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"service": "salcara-hub", "protocol": "salcara-remote", "version": "fixture", "protocolVersion": 1,
		"capabilities": capabilities, "authModes": []string{"device-pairing"}, "remoteAccess": observation,
	})
}

func remoteSupportObservation(base string, official bool) *config.StationObservation {
	return &config.StationObservation{Mode: "open", Revision: "synthetic-public-policy-revision", CheckedAt: time.Now().UnixMilli(), Station: &protocol.StationPolicy{Version: 1, APIBases: []string{base}, AllowOfficialAccounts: official}}
}

func TestAccountRemoteSupportUsesFullAPIBaseAndNeverSendsSecrets(t *testing.T) {
	for _, scenario := range []string{"allowed", "different path", "disabled", "missing declaration", "missing guard", "failed"} {
		t.Run(scenario, func(t *testing.T) {
			var upstream *httptest.Server
			var requests atomic.Int32
			upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path != "/a/salcara-hub/v1/ping" || r.Method != http.MethodGet || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Api-Key") != "" || r.ContentLength > 0 {
					t.Error("discovery sent credentials/body or used a non-plugin endpoint")
				}
				if scenario == "failed" {
					w.WriteHeader(http.StatusBadGateway)
					_, _ = w.Write([]byte("private provider response"))
					return
				}
				observation := remoteSupportObservation(upstream.URL+"/a", true)
				if scenario == "different path" {
					observation.Station.APIBases = []string{upstream.URL + "/b"}
				}
				if scenario == "disabled" {
					observation.Mode = "disabled"
				}
				if scenario == "missing declaration" {
					observation = nil
				}
				remoteSupportFixturePing(w, observation, scenario != "missing guard")
			}))
			defer upstream.Close()
			s, h := newTestServer(t)
			s.d.Store.SetRuntimeGatewayKey("synthetic-remote-runtime")
			id := createLocal(t, s, h, "api", "Fixture API", "synthetic-api-secret-never-sent", upstream.URL+"/a/v1")
			before := s.d.Store.Get()
			account, _ := before.LocalAccount(id)
			w := localRequest(s, h, http.MethodPost, "/api/local/accounts/remote-support", map[string]any{"id": id, "expectedSourceRevision": before.AccountSourceRevision(account)})
			if w.Code != http.StatusOK || requests.Load() != 1 || !reflect.DeepEqual(before, s.d.Store.Get()) {
				t.Fatalf("read-only account probe failed or mutated config: %d, %d", w.Code, requests.Load())
			}
			var result struct {
				OK             bool             `json:"ok"`
				ID             string           `json:"id"`
				SourceRevision string           `json:"sourceRevision"`
				BaseURL        string           `json:"baseUrl"`
				Support        remoteSupportDTO `json:"remoteSupport"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.ID != id || result.SourceRevision != before.AccountSourceRevision(account) || result.BaseURL != upstream.URL+"/a" {
				t.Fatal("probe result lost its exact account or full-base owner")
			}
			wantAPI, wantOfficial := "unknown", "unknown"
			if scenario == "allowed" {
				wantAPI, wantOfficial = "supported", "supported"
			} else if scenario == "different path" {
				wantAPI, wantOfficial = "unsupported", "supported"
			} else if scenario == "disabled" {
				wantAPI, wantOfficial = "unsupported", "unsupported"
			}
			if result.Support.APIRemote != wantAPI || result.Support.AccountRemote != wantOfficial || result.OK != (scenario != "failed") {
				t.Fatalf("wrong capability state: %+v", result)
			}
			for _, secret := range []string{account.Key, before.DeviceSecret, before.GatewayRuntimeKey, "private provider response"} {
				if secret != "" && strings.Contains(w.Body.String(), secret) {
					t.Fatal("probe exposed a private credential or provider body")
				}
			}
		})
	}
}

func TestAccountRemoteSupportRejectsStaleSourceBeforeAndAfterProbe(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		remoteSupportFixturePing(w, nil, true)
	}))
	defer upstream.Close()
	s, h := newTestServer(t)
	s.d.Store.SetRuntimeGatewayKey("synthetic-probe-source")
	id := createLocal(t, s, h, "api", "Fixture API", "synthetic-original-key", upstream.URL)
	c := s.d.Store.Get()
	a, _ := c.LocalAccount(id)
	body := map[string]any{"id": id, "expectedSourceRevision": c.AccountSourceRevision(a)}
	for _, invalid := range []map[string]any{{"id": id}, {"id": id, "expectedSourceRevision": ""}, {"id": id, "expectedSourceRevision": c.AccountSourceRevision(a), "url": upstream.URL}} {
		w := localRequest(s, h, http.MethodPost, "/api/local/accounts/remote-support", invalid)
		if w.Code != http.StatusConflict && w.Code != http.StatusBadRequest {
			t.Fatal("invalid source precondition reached discovery")
		}
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- localRequest(s, h, http.MethodPost, "/api/local/accounts/remote-support", body) }()
	<-entered
	updateSourceFixture(t, s, id, func(a *config.LocalAccount) { a.Key = "synthetic-replaced-key" })
	close(release)
	if w := <-done; w.Code != http.StatusConflict {
		t.Fatalf("late result survived source replacement: %d", w.Code)
	}
}

func TestAccountRemoteSupportUsesExactSavedCustomHub(t *testing.T) {
	for _, root := range []string{"missing", "different hub"} {
		t.Run(root, func(t *testing.T) {
			var rootCalls, customCalls atomic.Int32
			var upstream *httptest.Server
			upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Api-Key") != "" || r.ContentLength > 0 {
					t.Error("custom-Hub probe sent a credential or body")
				}
				if r.URL.Path == "/tenant/salcara-hub/v1/ping" {
					customCalls.Add(1)
					remoteSupportFixturePing(w, remoteSupportObservation(upstream.URL+"/tenant", true), true)
					return
				}
				rootCalls.Add(1)
				if root == "missing" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				remoteSupportFixturePing(w, remoteSupportObservation(upstream.URL+"/other", false), true)
			}))
			defer upstream.Close()
			s, h := newTestServer(t)
			s.d.Store.SetRuntimeGatewayKey("synthetic-custom-hub-probe")
			id := createLocal(t, s, h, "api", "Tenant API", "synthetic-key-never-sent", upstream.URL+"/tenant/v1")
			if err := s.d.Store.Update(func(c *config.Config) error {
				c.ConnectRemote(upstream.URL, upstream.URL+"/tenant/salcara-hub")
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before := s.d.Store.Get()
			account, _ := before.LocalAccount(id)
			w := localRequest(s, h, http.MethodPost, "/api/local/accounts/remote-support", map[string]any{"id": id, "expectedSourceRevision": before.AccountSourceRevision(account)})
			var result struct {
				Support remoteSupportDTO `json:"remoteSupport"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if w.Code != http.StatusOK || customCalls.Load() != 1 || rootCalls.Load() != 0 || result.Support.APIRemote != "supported" || result.Support.AccountRemote != "supported" || result.Support.HubURL != upstream.URL+"/tenant/salcara-hub" {
				t.Fatal("account detection ignored its exact saved custom Hub")
			}
			if !reflect.DeepEqual(before, s.d.Store.Get()) {
				t.Fatal("account capability detection changed device configuration")
			}
		})
	}
}

func TestAccountRemoteDiscoveryTargetDefaultsOnlyWithoutSavedSameOriginHub(t *testing.T) {
	account := config.LocalAccount{BaseURL: "https://relay.fixture/tenant/v1", SourceType: "api_key"}
	c := config.Config{}
	if target, err := accountRemoteDiscoveryTarget(c, account); err != nil || target != "" {
		t.Fatal("an unsaved origin lost default plugin discovery")
	}
	c.ConnectRemote("https://relay.fixture", "https://relay.fixture/one/salcara-hub")
	c.ConnectRemote("https://relay.fixture", "https://relay.fixture/two/salcara-hub")
	if target, err := accountRemoteDiscoveryTarget(c, account); err == nil || target != "" {
		t.Fatal("ambiguous saved plugins fell back to an unrelated root Hub")
	}
	c.RemoteConnections[1].RemoteSupport = remoteSupportObservation("https://relay.fixture/tenant", true)
	if target, err := accountRemoteDiscoveryTarget(c, account); err != nil || target != "https://relay.fixture/two/salcara-hub" {
		t.Fatal("full API-base declaration did not locate its saved plugin")
	}
}

func TestAccountRemoteSupportRejectsChangedSavedHubAfterProbe(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		remoteSupportFixturePing(w, nil, true)
	}))
	defer upstream.Close()
	s, h := newTestServer(t)
	s.d.Store.SetRuntimeGatewayKey("synthetic-hub-target-fence")
	id := createLocal(t, s, h, "api", "Tenant API", "synthetic-key", upstream.URL+"/tenant")
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.ConnectRemote(upstream.URL, upstream.URL+"/tenant/salcara-hub")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := s.d.Store.Get()
	account, _ := before.LocalAccount(id)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- localRequest(s, h, http.MethodPost, "/api/local/accounts/remote-support", map[string]any{"id": id, "expectedSourceRevision": before.AccountSourceRevision(account)})
	}()
	<-entered
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.ConnectRemote(upstream.URL, upstream.URL+"/replacement/salcara-hub")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if w := <-done; w.Code != http.StatusConflict {
		t.Fatal("late discovery survived a changed saved Hub target")
	}
}

func TestRemoteSupportChoicesAreCredentialFreeAndExpiredHintsAreUnknown(t *testing.T) {
	s, _ := newTestServer(t)
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.ConnectRemote("https://relay.fixture", "https://relay.fixture/salcara-hub")
		c.RemoteConnections[0].RemoteSupport = remoteSupportObservation("https://relay.fixture/a", true)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c := s.d.Store.Get()
	choices := remoteConnectionChoices(c)
	encoded, _ := json.Marshal(choices)
	if strings.Contains(string(encoded), c.DeviceID) || strings.Contains(string(encoded), c.DeviceSecret) || len(choices) != 1 {
		t.Fatal("saved choices leaked identity or secret")
	}
	a := config.LocalAccount{BaseURL: "https://relay.fixture/b", SourceType: "api_key"}
	support := accountSavedRemoteSupport(c, a)
	if support.APIRemote != "unsupported" || support.AccountRemote != "supported" {
		t.Fatal("same-origin but different API path inherited an API capability")
	}
	c.RemoteConnections[0].RemoteSupport.CheckedAt = time.Now().Add(-6 * time.Minute).UnixMilli()
	if got := accountSavedRemoteSupport(c, a); got.APIRemote != "unknown" || got.AccountRemote != "unknown" {
		t.Fatal("expired capability hint remained available")
	}
	c.RemoteConnections[0].RemoteSupport = nil
	if got := accountSavedRemoteSupport(c, a); got.APIRemote != "unknown" || got.AccountRemote != "unknown" {
		t.Fatal("missing declaration implied remote support")
	}
}

func TestRemoteDiscoverSavesObservationWithoutConnectingAndClearsOnFailure(t *testing.T) {
	var fail atomic.Bool
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		remoteSupportFixturePing(w, remoteSupportObservation(upstream.URL, true), true)
	}))
	defer upstream.Close()
	s, h := newTestServer(t)
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.ConnectRemote(upstream.URL, upstream.URL+"/salcara-hub")
		c.ConnectRemote("https://active.fixture", "https://active.fixture/salcara-hub")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := s.d.Store.Get()
	body := map[string]any{"url": upstream.URL, "connect": false}
	w := localRequest(s, h, http.MethodPost, "/api/remote/discover", body)
	after := s.d.Store.Get()
	if w.Code != http.StatusOK || config.RemoteIdentity(before) != config.RemoteIdentity(after) || !after.RemoteConnections[0].RemoteSupport.Available() {
		t.Fatal("saved-station detection connected or lost its observation")
	}
	fail.Store(true)
	w = localRequest(s, h, http.MethodPost, "/api/remote/discover", body)
	after = s.d.Store.Get()
	if w.Code != http.StatusBadRequest || after.RemoteConnections[0].RemoteSupport.Available() || config.RemoteIdentity(before) != config.RemoteIdentity(after) {
		t.Fatal("failed detection retained a green hint or changed active station")
	}
}

func TestRemoteDiscoverOfficialSelectionRechecksPolicyBeforeConnect(t *testing.T) {
	var allow atomic.Bool
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remoteSupportFixturePing(w, remoteSupportObservation(upstream.URL, allow.Load()), true)
	}))
	defer upstream.Close()
	s, h := newTestServer(t)
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.ConnectRemote(upstream.URL, upstream.URL+"/salcara-hub")
		c.RemoteConnections[0].RemoteSupport = remoteSupportObservation(upstream.URL, true)
		c.ConnectRemote("https://active.fixture", "https://active.fixture/salcara-hub")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := s.d.Store.Get()
	body := map[string]any{"url": upstream.URL, "connect": true, "requireOfficial": true}
	w := localRequest(s, h, http.MethodPost, "/api/remote/discover", body)
	after := s.d.Store.Get()
	if w.Code != http.StatusConflict || config.RemoteIdentity(after) != config.RemoteIdentity(before) || after.RemoteConnections[0].RemoteSupport.Station.AllowOfficialAccounts {
		t.Fatal("official picker connected after the station disallowed official accounts")
	}
	allow.Store(true)
	w = localRequest(s, h, http.MethodPost, "/api/remote/discover", body)
	after = s.d.Store.Get()
	if w.Code != http.StatusOK || after.HubURL != upstream.URL+"/salcara-hub" || !after.RemoteConnections[0].RemoteSupport.Available() {
		t.Fatal("compatible official station could not be chosen")
	}
}

func TestRemoteDiscoverAcceptsStandaloneEnrollmentClaimWithoutSub2APIKey(t *testing.T) {
	var requests atomic.Int32
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/salcara-hub/v1/ping" || r.Header.Get("Authorization") != "" {
			t.Error("Hub discovery used a credential or unexpected endpoint")
		}
		remoteSupportFixturePing(w, nil, false)
	}))
	defer upstream.Close()
	s, h := newTestServer(t)
	claim := strings.Repeat("c", 64)
	url := upstream.URL + "/salcara-hub"

	probe := localRequest(s, h, http.MethodPost, "/api/remote/discover", map[string]any{"url": url, "enrollmentClaim": claim})
	if probe.Code != http.StatusOK || len(s.d.Store.Get().RemoteConnections) != 0 {
		t.Fatal("read-only discovery persisted the enrollment claim or connected the computer")
	}
	invalid := localRequest(s, h, http.MethodPost, "/api/remote/discover", map[string]any{"url": url, "connect": true, "enrollmentClaim": "not-a-claim"})
	if invalid.Code != http.StatusBadRequest || len(s.d.Store.Get().RemoteConnections) != 0 {
		t.Fatal("malformed enrollment claim changed the active connection")
	}
	connected := localRequest(s, h, http.MethodPost, "/api/remote/discover", map[string]any{"url": url, "connect": true, "enrollmentClaim": claim})
	cfg := s.d.Store.Get()
	if connected.Code != http.StatusOK || !cfg.RemoteDeviceOnly || cfg.AccountKey != "" || cfg.DeviceEnrollmentClaim() != claim || cfg.EffectiveHubURL() != url {
		t.Fatalf("standalone Hub did not save a device-only enrollment: status=%d config=%+v", connected.Code, cfg)
	}
	state := localRequest(s, h, http.MethodGet, "/api/state", nil).Body.String()
	if strings.Contains(state, claim) {
		t.Fatal("one-time enrollment claim leaked through the local state API")
	}
	if requests.Load() != 2 {
		t.Fatalf("unexpected discovery count: %d", requests.Load())
	}
}

func TestRemoteDiscoverLateResultCannotOverwriteNewerObservation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first := calls.Add(1) == 1
		if first {
			close(entered)
			<-release
		}
		remoteSupportFixturePing(w, remoteSupportObservation(upstream.URL, first), true)
	}))
	defer upstream.Close()
	s, h := newTestServer(t)
	_ = s.d.Store.Update(func(c *config.Config) error { c.ConnectRemote(upstream.URL, upstream.URL+"/salcara-hub"); return nil })
	body := map[string]any{"url": upstream.URL}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- localRequest(s, h, http.MethodPost, "/api/remote/discover", body) }()
	<-entered
	newer := localRequest(s, h, http.MethodPost, "/api/remote/discover", body)
	close(release)
	older := <-done
	if newer.Code != http.StatusOK || older.Code != http.StatusConflict || s.d.Store.Get().RemoteConnections[0].RemoteSupport.Station.AllowOfficialAccounts {
		t.Fatal("late available reply overwrote the newer unsupported observation")
	}
}

func TestRemoteDiscoverCurrentStationRetiresAuthorityAndRestoresOnlyDisplay(t *testing.T) {
	for _, failure := range []string{"official denied", "disabled", "unavailable"} {
		t.Run(failure, func(t *testing.T) {
			var recovered atomic.Bool
			var upstream *httptest.Server
			upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !recovered.Load() && failure == "unavailable" {
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				observation := remoteSupportObservation(upstream.URL, true)
				if !recovered.Load() {
					if failure == "disabled" {
						observation.Mode = "disabled"
					} else {
						observation.Station.AllowOfficialAccounts = false
					}
				}
				remoteSupportFixturePing(w, observation, true)
			}))
			defer upstream.Close()
			s, h := newTestServer(t)
			if err := s.d.Store.Update(func(c *config.Config) error {
				c.ConnectRemote(upstream.URL, upstream.URL+"/salcara-hub")
				observation := remoteSupportObservation(upstream.URL, true)
				c.RemoteConnections[0].RemoteSupport = config.CloneStationObservation(observation)
				c.RemoteStationObservation = config.CloneStationObservation(observation)
				c.RemoteStationAuthorization = &config.StationAuthorization{Identity: config.RemoteIdentity(*c), Revision: observation.Revision, Station: *config.CloneStationPolicy(observation.Station)}
				c.RemoteExecutionArmed = true
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before := s.d.Store.Get()
			if before.RemoteStationAuthorization == nil || !before.RemoteExecutionArmed {
				t.Fatal("fixture must begin with authenticated active authority")
			}
			identity := config.RemoteIdentity(before)
			body := map[string]any{"url": upstream.URL, "connect": false}
			result := localRequest(s, h, http.MethodPost, "/api/remote/discover", body)
			wantStatus := http.StatusOK
			if failure == "unavailable" {
				wantStatus = http.StatusBadRequest
			}
			current := s.d.Store.Get()
			if result.Code != wantStatus || current.RemoteStationAuthorization != nil || current.RemoteStationObservation == nil || config.RemoteIdentity(current) != identity {
				t.Fatal("active-station detection did not retire the exact current authority")
			}
			if current.RemoteStationObservation.Available() && current.RemoteStationObservation.Station.AllowOfficialAccounts {
				t.Fatal("active station kept an obsolete official-account capability")
			}
			if !reflect.DeepEqual(current.RemoteStationObservation, current.RemoteConnections[0].RemoteSupport) {
				t.Fatal("active capability and saved-station display diverged")
			}
			if err := s.d.Store.Update(func(c *config.Config) error { c.RemoteExecutionArmed = false; return nil }); err != nil {
				t.Fatal(err)
			}
			recovered.Store(true)
			result = localRequest(s, h, http.MethodPost, "/api/remote/discover", body)
			current = s.d.Store.Get()
			if result.Code != http.StatusOK || !current.RemoteStationObservation.Available() || !current.RemoteStationObservation.Station.AllowOfficialAccounts {
				t.Fatal("fresh current-station detection did not restore capability display")
			}
			if current.RemoteStationAuthorization != nil || current.RemoteExecutionArmed || config.RemoteIdentity(current) != identity {
				t.Fatal("capability detection reauthorized or armed remote execution")
			}
		})
	}
}
