package config

import (
	"testing"
	"time"
)

func TestStationObservationClockRollbackRestoresLatestPolicy(t *testing.T) {
	store := gatewayAuthorizationStore(t, false)
	cfg := store.Get()
	policy := CloneStationPolicy(&cfg.RemoteStationAuthorization.Station)
	updateGatewayAuthorization(t, store, func(c *Config) {
		future := &StationObservation{Mode: "unknown", CheckedAt: time.Now().Add(5 * time.Hour).UnixMilli()}
		c.RemoteConnections = []RemoteConnection{{HubURL: c.HubURL, DeviceID: c.DeviceID, DeviceSecret: c.DeviceSecret}}
		c.RemoteStationObservation = CloneStationObservation(future)
		for i := range c.RemoteConnections {
			c.RemoteConnections[i].RemoteSupport = CloneStationObservation(future)
		}
	})
	cfg = store.Get()
	oldProbe := store.BeginRemoteStationObservation(cfg)
	latest := store.BeginRemoteStationObservation(cfg)
	current := &StationObservation{Mode: "open", Revision: "after-correction", Station: policy, CheckedAt: time.Now().UnixMilli()}
	store.ObserveRemoteStation(cfg, current, latest)
	got := store.Get()
	if got.RemoteStationObservation == nil || got.RemoteStationObservation.Revision != current.Revision || !got.RemoteStationObservation.Available() {
		t.Fatal("future local timestamp blocked latest successful policy after clock correction")
	}
	if got.RemoteStationAuthorization != nil {
		t.Fatal("recovered display observation granted execution authority")
	}
	for _, saved := range got.RemoteConnections {
		if saved.RemoteSupport == nil || saved.RemoteSupport.Revision != current.Revision {
			t.Fatal("saved connection retained future unknown policy")
		}
	}
	store.ObserveRemoteStation(cfg, nil, oldProbe)
	if store.Get().RemoteStationObservation.Revision != current.Revision {
		t.Fatal("older sequence overwrote corrected policy")
	}
}

func TestStationObservationNewerKeepsNormalCrossProbeOrdering(t *testing.T) {
	now := time.Now().UnixMilli()
	older := &StationObservation{CheckedAt: now - 1000}
	newer := &StationObservation{CheckedAt: now - 1}
	future := &StationObservation{CheckedAt: now + 18000000}
	if !StationObservationIsNewer(newer, older, now) || StationObservationIsNewer(older, newer, now) || StationObservationIsNewer(future, newer, now) {
		t.Fatal("clock rollback exception changed normal cross-probe ordering")
	}
}

func TestStationObservationNormalNewerCrossSourcePolicyStillWins(t *testing.T) {
	store := gatewayAuthorizationStore(t, false)
	cfg := store.Get()
	policy := CloneStationPolicy(&cfg.RemoteStationAuthorization.Station)
	now := time.Now().UnixMilli()
	updateGatewayAuthorization(t, store, func(c *Config) {
		c.RemoteStationObservation = &StationObservation{Mode: "open", Revision: "newer-console", Station: policy, CheckedAt: now - 1}
	})
	cfg = store.Get()
	probe := store.BeginRemoteStationObservation(cfg)
	store.ObserveRemoteStation(cfg, &StationObservation{Mode: "open", Revision: "older-background", Station: policy, CheckedAt: now - 1000}, probe)
	if store.Get().RemoteStationObservation.Revision != "newer-console" {
		t.Fatal("latest background sequence overrode newer independent console policy")
	}
}
