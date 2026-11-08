package hubclient

import (
	"context"
	"errors"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

// Discovery carries no upstream credentials. Cached observations help users
// choose a station, while commands must still carry a current Hub source scope.
func (c *Client) refreshStationCapabilities(ctx context.Context, station config.Config) {
	if c.o.Store == nil || !station.RemoteExecutionArmed {
		return
	}
	sequence := c.o.Store.BeginRemoteStationObservation(station)
	if sequence == 0 {
		return
	}
	info, err := DiscoverForDevice(ctx, station.EffectiveHubURL(), station.EffectiveHubURL(), station.DeviceID, station.DeviceSecret, station.DeviceEnrollmentClaim())
	if err != nil {
		c.o.Store.ObserveRemoteStation(station, nil, sequence)
		return
	}
	c.o.Store.ObserveRemoteStation(station, info.RemoteAccess, sequence)
}

// A queued envelope cannot reinstate a scope already retired by discovery.
// Refresh on a new revision so a real operator change takes effect without
// waiting for the normal observation loop; the response must match the Hub.
func (c *Client) refreshExecutionStation(ctx context.Context, snapshot config.Config, env protocol.CommandEnvelope) error {
	if !snapshot.RemoteExecutionArmed || !sourceGuardExecutionType(str(env.Command, "type")) || str(env.Command, "type") == "remote.station.switch" || env.Source == nil || !config.ValidStationPolicy(env.Source.Station) {
		return nil
	}
	current := c.o.Store.Get()
	observation := current.RemoteStationObservation
	if observation == nil || observation.Revision != env.Source.PolicyRevision || !observation.Available() {
		c.refreshStationCapabilities(ctx, snapshot)
		current = c.o.Store.Get()
		observation = current.RemoteStationObservation
	}
	if config.RemoteIdentity(current) != config.RemoteIdentity(snapshot) || observation == nil || !observation.Available() || observation.Revision != env.Source.PolicyRevision {
		return errors.New("中转站授权已变化，请刷新后重试")
	}
	return nil
}

func stationObservation(cfg config.Config, hubURL, deviceID string) *config.StationObservation {
	if config.NormalizeHubURL(cfg.EffectiveHubURL()) == config.NormalizeHubURL(hubURL) && cfg.DeviceID == deviceID && cfg.RemoteStationObservation != nil {
		return cfg.RemoteStationObservation
	}
	for _, saved := range cfg.RemoteConnections {
		if config.NormalizeHubURL(saved.HubURL) == config.NormalizeHubURL(hubURL) && saved.DeviceID == deviceID {
			return saved.RemoteSupport
		}
	}
	return nil
}

func stationCapabilities(observation *config.StationObservation) (bool, bool, int64) {
	if observation == nil {
		return false, false, 0
	}
	if !observation.Available() {
		return false, false, observation.CheckedAt
	}
	return config.StationHasAPIAccounts(observation.Station), observation.Station.AllowOfficialAccounts, observation.CheckedAt
}
