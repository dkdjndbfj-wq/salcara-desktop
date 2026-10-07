package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func (c *Client) standbyLoop(ctx context.Context) {
	tick := time.NewTicker(c.o.StandbyEvery)
	defer tick.Stop()
	for ctx.Err() == nil {
		cfg := c.o.Store.Get()
		if cfg.RemoteDeviceOnly && validPhoneHash(cfg.PhoneHash) && validPhoneHash(cfg.PhoneBindingID) {
			var wg sync.WaitGroup
			gate := make(chan struct{}, 4)
			for _, station := range cfg.RemoteConnections {
				if config.NormalizeHubURL(station.HubURL) == config.NormalizeHubURL(cfg.EffectiveHubURL()) || station.DeviceSecret == "" {
					continue
				}
				wg.Add(1)
				go func(station config.RemoteConnection) {
					defer wg.Done()
					select {
					case gate <- struct{}{}:
					case <-ctx.Done():
						return
					}
					defer func() { <-gate }()
					poll, cancel := context.WithTimeout(ctx, 4*time.Second)
					defer cancel()
					_ = c.pollStandby(poll, cfg, station)
				}(station)
			}
			wg.Wait()
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// No registration, model request, history, or permanent connection on B.
func (c *Client) pollStandby(ctx context.Context, base config.Config, station config.RemoteConnection) error {
	cfg := activeStationConfig(base, station)
	req, err := c.requestFor(ctx, cfg, http.MethodGet, "/bridge/standby", nil, false)
	if err != nil {
		return err
	}
	req.Header.Set("X-Salcara-Computer-Id", base.ComputerID)
	resp, err := c.o.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := checkResp(resp); err != nil {
		return err
	}
	var result struct {
		Pairing struct {
			DeviceID, BindingID, PhoneHash string
			Paired                         bool
		}
		Commands []protocol.CommandEnvelope
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<10)).Decode(&result); err != nil {
		return err
	}
	if !result.Pairing.Paired || result.Pairing.DeviceID != station.DeviceID || !phoneMatches(base, result.Pairing.BindingID, result.Pairing.PhoneHash) || len(result.Commands) > 1 {
		return errors.New("备用站点绑定不匹配")
	}
	for _, env := range result.Commands {
		// The HTTP poll's cancellation cannot cancel an already received write.
		// A strict time budget and the same operation UUID survive phone retries.
		go c.executeStandby(base, cfg, env)
	}
	return nil
}

func (c *Client) executeStandby(base, station config.Config, env protocol.CommandEnvelope) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c.taskConfigMu.Lock()
	c.expireUpdateLocked()
	current := c.o.Store.Get()
	var result any
	var err error
	in := stationSwitchCommand{TargetHubURL: str(env.Command, "targetHubUrl"), TargetDeviceID: str(env.Command, "targetDeviceId"), TargetComputerID: str(env.Command, "targetComputerId"), Agent: str(env.Command, "agent"), AccountID: str(env.Command, "accountId"), Model: str(env.Command, "model"), SessionKey: str(env.Command, "sessionKey"), OperationID: str(env.Command, "operationId")}
	saved, exists := current.SavedRemoteConnection(station.EffectiveHubURL(), station.DeviceID)
	if c.updatePrepared || env.CommandID == "" || !env.Phone || env.DeviceID != station.DeviceID || str(env.Command, "type") != "remote.station.switch" || !exists || saved.DeviceSecret != station.DeviceSecret || config.NormalizeHubURL(in.TargetHubURL) != config.NormalizeHubURL(station.EffectiveHubURL()) || in.TargetDeviceID != station.DeviceID || !phoneMatches(current, env.BindingID, env.PhoneHash) || !phoneMatches(current, base.PhoneBindingID, base.PhoneHash) || config.RemoteIdentity(current) != config.RemoteIdentity(base) {
		err = errors.New("备用控制授权或连接已变化，请刷新后重试")
	} else {
		result, err = c.switchStationMode(ctx, in, true)
	}
	committed := err == nil
	c.taskConfigMu.Unlock()
	reply := protocol.Reply{DeviceID: station.DeviceID, CommandID: env.CommandID, OK: err == nil, Result: result}
	if err != nil {
		reply.Error = err.Error()
	}
	_ = c.postFor(ctx, station, "/bridge/reply", reply)
	if committed {
		c.Kick()
	}
}
