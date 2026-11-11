package hubclient

import (
	"context"
	"errors"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

// Discovery carries no upstream credentials. Cached observations help users
// choose a station, while commands must still carry a current Hub source scope.
func (c *Client) refreshStationCapabilities(ctx context.Context, station config.Config) error {
	if c.o.Store == nil || !station.RemoteExecutionArmed {
		return nil
	}
	sequence := c.o.Store.BeginRemoteStationObservation(station)
	if sequence == 0 {
		return nil
	}
	var info Discovery
	var err error
	if saved, ok := station.SavedRemoteConnection(station.EffectiveHubURL(), station.DeviceID); ok {
		info, err = c.DiscoverForConnection(ctx, station, saved)
	} else {
		info, err = DiscoverForDevice(ctx, station.EffectiveHubURL(), station.EffectiveHubURL(), station.DeviceID, station.DeviceSecret, station.DeviceEnrollmentClaim())
	}
	if info.Identity != nil {
		// Upgrade a trusted exact-URL legacy record before the next credential
		// request. Pins are durable, unlike short-lived routing manifests.
		if pinErr := c.o.Store.Update(func(current *config.Config) error {
			if ctx.Err() != nil || current.RemoteConnectionEpoch != station.RemoteConnectionEpoch {
				return errors.New("远程连接已变化或检查已取消")
			}
			for i := range current.RemoteConnections {
				saved := &current.RemoteConnections[i]
				if saved.DeviceID != station.DeviceID || config.NormalizeHubURL(saved.HubURL) != config.NormalizeHubURL(station.EffectiveHubURL()) {
					continue
				}
				if saved.DeviceSecret != station.DeviceSecret {
					return ErrHubIdentityChanged
				}
				if original, exists := station.SavedRemoteConnection(saved.HubURL, saved.DeviceID); !exists || saved.HubInstanceID != original.HubInstanceID || saved.IdentityPublicKey != original.IdentityPublicKey || saved.IngressURL != original.IngressURL {
					return errors.New("远程入口已更新，请重新检查")
				}
				if saved.HubInstanceID != "" && (saved.HubInstanceID != info.Identity.HubInstanceID || saved.IdentityPublicKey != info.Identity.IdentityPublicKey) {
					return ErrHubIdentityChanged
				}
				saved.HubInstanceID, saved.IdentityPublicKey = info.Identity.HubInstanceID, info.Identity.IdentityPublicKey
				saved.IngressURL = info.HubURL + "/v1"
				return nil
			}
			return ErrHubIdentityChanged
		}); pinErr != nil {
			return pinErr
		}
	}
	if err != nil {
		c.o.Store.ObserveRemoteStation(station, nil, sequence)
		if errors.Is(err, ErrHubIdentityChanged) || errors.Is(err, ErrHubIdentityVerification) {
			return err
		}
		return nil
	}
	c.o.Store.ObserveRemoteStation(station, info.RemoteAccess, sequence)
	return nil
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
