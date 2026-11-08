package config

import (
	"encoding/json"
	"salcara/bridge/internal/protocol"
	"testing"
	"time"
)

func TestStationAPIBaseCanonicalEquivalenceAndSeparation(t *testing.T) {
	for raw, want := range map[string]string{"https://API.Example:443/team/v1/": "https://api.example/team", "http://LOCALHOST:80/v1": "http://localhost", "https://[2001:db8::1]:443/team": "https://[2001:db8::1]/team"} {
		got, err := NormalizeStationAPIBase(raw)
		if err != nil || got != want {
			t.Fatalf("%q normalized %q: %v", raw, got, err)
		}
	}
	for _, raw := range []string{"https://a.test/a/../b", "https://a.test/a%2fb", "https://a.test/a%252fb", "https://a.test//b", "https://a.test./a", "https://a.test:0443/a", "https://a.test/a?", "https://a.test/a#x", "https://a.test/a#", "https://a.test/a\\b", "http://remote.test/a"} {
		if _, err := NormalizeStationAPIBase(raw); err == nil {
			t.Errorf("ambiguous base accepted: %s", raw)
		}
	}
	p := &protocol.StationPolicy{Version: 1, APIBases: []string{"https://a.test/Team"}}
	for _, base := range []string{"https://a.test/team", "https://a.test/Team/sub", "https://b.test/Team"} {
		if StationAllowsAccount(p, LocalAccount{SourceType: "api_key", BaseURL: base}) {
			t.Errorf("different station accepted: %s", base)
		}
	}
}

func TestStationObservationLatestProbeWinsAndFailureRevokesLease(t *testing.T) {
	store := gatewayAuthorizationStore(t, false)
	cfg := store.Get()
	observation := &StationObservation{Mode: "open", Revision: "r1", Station: CloneStationPolicy(&cfg.RemoteStationAuthorization.Station), CheckedAt: time.Now().UnixMilli()}
	oldProbe := store.BeginRemoteStationObservation(cfg)
	newProbe := store.BeginRemoteStationObservation(cfg)
	store.ObserveRemoteStation(cfg, observation, newProbe)
	lease := store.Get().RemoteGatewayCredential("remote-codex")
	if lease == "" {
		t.Fatal("current same-station capability revoked a valid worker")
	}
	// Both probes may share the same wall-clock millisecond; completion order
	// must not let the older success or failure replace the current observation.
	store.ObserveRemoteStation(cfg, nil, oldProbe)
	stale := CloneStationObservation(observation)
	stale.Revision = "retired"
	store.ObserveRemoteStation(cfg, stale, oldProbe)
	if got := store.Get(); got.RemoteStationObservation.Revision != "r1" || got.RemoteGatewayCredential("remote-codex") != lease {
		t.Fatal("delayed old response replaced the latest probe")
	}
	latest := store.BeginRemoteStationObservation(cfg)
	store.ObserveRemoteStation(cfg, nil, latest)
	if got := store.Get(); got.RemoteStationObservation == nil || got.RemoteStationObservation.Available() || got.RemoteStationAuthorization != nil || got.RemoteGatewayCredential("remote-codex") != "" {
		t.Fatal("failed current probe retained execution authority")
	}
	store.ObserveRemoteStation(cfg, observation, newProbe)
	if store.Get().RemoteStationObservation.Available() {
		t.Fatal("stale success erased the failed current probe")
	}
}

func TestStationObservationPolicyAndIdentityChangesInvalidateAuthority(t *testing.T) {
	for _, change := range []string{"revision", "scope", "disabled", "identity", "restart"} {
		t.Run(change, func(t *testing.T) {
			store := gatewayAuthorizationStore(t, false)
			cfg := store.Get()
			observation := &StationObservation{Mode: "open", Revision: "r1", Station: CloneStationPolicy(&cfg.RemoteStationAuthorization.Station), CheckedAt: time.Now().UnixMilli()}
			probe := store.BeginRemoteStationObservation(cfg)
			switch change {
			case "revision":
				observation.Revision = "r2"
			case "scope":
				observation.Station.APIBases = []string{"https://another.fixture.test"}
			case "disabled":
				observation.Mode = "disabled"
			case "identity":
				updateGatewayAuthorization(t, store, func(c *Config) { c.ConnectRemote("https://b.fixture.test", "https://b.fixture.test/salcara-hub") })
			case "restart":
				store.SetRuntimeGatewayKey("synthetic-new-process")
			}
			store.ObserveRemoteStation(cfg, observation, probe)
			got := store.Get()
			if got.RemoteStationAuthorization != nil || got.RemoteGatewayCredential("remote-codex") != "" {
				t.Fatal("retired station authority survived lifecycle change")
			}
			if (change == "identity" || change == "restart") && got.RemoteStationObservation != nil {
				t.Fatal("old probe advertised support in a different connection/process")
			}
		})
	}
}

func TestStationObservationFromPreviousVisitCannotReplaceNewConnection(t *testing.T) {
	store := gatewayAuthorizationStore(t, false)
	old := store.Get()
	probe := store.BeginRemoteStationObservation(old)
	updateGatewayAuthorization(t, store, func(c *Config) { c.ConnectRemote("https://b.fixture.test", "https://b.fixture.test/salcara-hub") })
	// Simulate returning to the exact old identity after a fresh target probe.
	updateGatewayAuthorization(t, store, func(c *Config) {
		c.HubURL, c.DeviceID, c.DeviceSecret, c.RemoteDeviceOnly = old.HubURL, old.DeviceID, old.DeviceSecret, old.RemoteDeviceOnly
		c.RemoteStationObservation = &StationObservation{Mode: "open", Revision: "new-visit", Station: CloneStationPolicy(&old.RemoteStationAuthorization.Station), CheckedAt: time.Now().UnixMilli()}
	})
	store.ObserveRemoteStation(old, nil, probe)
	if store.Get().RemoteStationObservation.Revision != "new-visit" {
		t.Fatal("slow response from previous station visit replaced fresh support")
	}
}

func TestStationRuntimeLeaseCannotFollowOtherStationOrRestart(t *testing.T) {
	c := Config{RemoteExecutionArmed: true, GatewayRuntimeKey: "synthetic-process", DeviceID: "device-a", LocalAccounts: []LocalAccount{{ID: "api", SourceType: "api_key", BaseURL: "https://a.test", Key: "key"}}, RemoteAPI: map[string]RemoteAPI{"codex": {AccountID: "api"}}}
	if c.RemoteGatewayCredential("remote-codex") != "" {
		t.Fatal("unbound worker received lease")
	}
	c.RemoteStationAuthorization = &StationAuthorization{Identity: RemoteIdentity(c), Revision: "r1", Station: protocol.StationPolicy{Version: 1, APIBases: []string{"https://a.test"}}}
	lease := c.RemoteGatewayCredential("remote-codex")
	if lease == "" {
		t.Fatal("authorized A received no lease")
	}
	copy := c.Clone()
	copy.RemoteStationAuthorization.Station.APIBases[0] = "https://b.test"
	if c.RemoteStationAuthorization.Station.APIBases[0] != "https://a.test" {
		t.Fatal("scope clone aliases original")
	}
	for _, edit := range []func(*Config){func(c *Config) { c.LocalAccounts[0].BaseURL = "https://b.test" }, func(c *Config) { c.DeviceID = "device-b" }, func(c *Config) { c.RemoteStationAuthorization = nil }, func(c *Config) { c.RemoteStationAuthorization.Revision = "r2" }} {
		next := c.Clone()
		edit(&next)
		if next.RemoteGatewayCredential("remote-codex") == lease {
			t.Fatal("old worker retained scope after change")
		}
	}
	raw, _ := json.Marshal(c)
	var disk Config
	_ = json.Unmarshal(raw, &disk)
	if disk.RemoteStationAuthorization != nil {
		t.Fatal("runtime station authority persisted")
	}
}
