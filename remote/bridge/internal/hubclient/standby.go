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
		if cfg.HubRemoteEnabled() && cfg.EffectiveRemoteMode() == "hub" && cfg.RemoteDeviceOnly && validPhoneHash(cfg.PhoneHash) && validPhoneHash(cfg.PhoneBindingID) {
			var wg sync.WaitGroup
			gate := make(chan struct{}, 4)
			for _, station := range cfg.RemoteConnections {
				if station.BotGroupID != "" || config.NormalizeHubURL(station.HubURL) == config.NormalizeHubURL(cfg.EffectiveHubURL()) || station.DeviceSecret == "" {
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
					poll, cancel := context.WithTimeout(ctx, hubConnectionProbeTimeout)
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
	if !base.HubRemoteEnabled() || !c.o.Store.Get().HubRemoteEnabled() || c.o.Store.Get().EffectiveRemoteMode() == "bot" || base.EffectiveRemoteMode() == "bot" || station.BotGroupID != "" {
		return errors.New("备用中转站未启用")
	}
	cfg := activeStationConfig(base, station)
	c.refreshStationCapabilities(ctx, cfg)
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
	if resp.StatusCode/100 != 2 {
		return checkResp(resp)
	}
	var result struct {
		Pairing struct {
			DeviceID, BindingID, PhoneHash string
			Paired                         bool
		}
		Commands []protocol.CommandEnvelope
	}
	// checkResp drains successful bodies; this endpoint still needs the JSON.
	// Decode the complete bounded body so an oversized/trailing envelope cannot
	// start a handover merely because its first JSON value happened to be valid.
	body, err := io.ReadAll(io.LimitReader(resp.Body, protocol.MaxSourcePolicyWireBytes+1))
	if err != nil {
		return err
	}
	if len(body) > protocol.MaxSourcePolicyWireBytes {
		return errors.New("备用站点响应过大")
	}
	if err := json.Unmarshal(body, &result); err != nil {
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
	if str(env.Command, "type") == "source.grant.acquire" {
		// No source envelope is expected for this internal request. The fetched
		// Hub challenge must independently prove the exact station handover.
		result, err := c.acquireSourceGrantFor(ctx, str(env.Command, "challengeId"), station, func(current config.Config) bool {
			return standbyEnvelopeCurrent(base, station, current, env) && len(env.Command) == 2 && env.Source == nil
		}, true)
		reply := protocol.Reply{DeviceID: station.DeviceID, CommandID: env.CommandID, OK: err == nil, Result: result}
		if err != nil {
			reply.Error = err.Error()
		}
		_ = c.postFor(ctx, station, "/bridge/reply", reply)
		return
	}
	c.taskConfigMu.Lock()
	c.expireUpdateLocked()
	current := c.o.Store.Get()
	var result any
	var err error
	sourceOnly, sourceOnlyValid := stationSourceOnly(env.Command)
	in := stationSwitchCommand{TargetHubURL: str(env.Command, "targetHubUrl"), TargetDeviceID: str(env.Command, "targetDeviceId"), TargetComputerID: str(env.Command, "targetComputerId"), Agent: str(env.Command, "agent"), AccountID: str(env.Command, "accountId"), Model: str(env.Command, "model"), SessionKey: str(env.Command, "sessionKey"), OperationID: str(env.Command, "operationId"), SourceOnly: sourceOnly}
	if c.updatePrepared || !sourceOnlyValid || !standbyEnvelopeCurrent(base, station, current, env) || str(env.Command, "type") != "remote.station.switch" || config.NormalizeHubURL(in.TargetHubURL) != config.NormalizeHubURL(station.EffectiveHubURL()) || in.TargetDeviceID != station.DeviceID {
		err = errors.New("备用控制授权或连接已变化，请刷新后重试")
	} else if err = c.validateExecutionSourceFor(env, current); err != nil {
		// Alternate polling must enforce the same contract as active SSE.
	} else {
		ctx = withExecutionSource(ctx, env)
		ctx = context.WithValue(ctx, stationSwitchGuardKey{}, func(latest config.Config) bool {
			return standbyEnvelopeCurrent(base, station, latest, env)
		})
	}
	c.taskConfigMu.Unlock()
	if err == nil {
		result, err = c.switchStationMode(ctx, in, true)
	}
	committed := err == nil
	reply := protocol.Reply{DeviceID: station.DeviceID, CommandID: env.CommandID, OK: err == nil, Result: result}
	if err != nil {
		reply.Error = err.Error()
	}
	_ = c.postFor(ctx, station, "/bridge/reply", reply)
	if committed {
		c.Kick()
	}
}

func standbyEnvelopeCurrent(base, station, current config.Config, env protocol.CommandEnvelope) bool {
	saved, exists := current.SavedRemoteConnection(station.EffectiveHubURL(), station.DeviceID)
	return current.HubRemoteEnabled() && current.EffectiveRemoteMode() != "bot" && saved.BotGroupID == "" && env.CommandID != "" && env.Phone && env.DeviceID == station.DeviceID && exists && saved.DeviceSecret == station.DeviceSecret && current.ComputerID != "" && current.ComputerID == base.ComputerID && current.ComputerID == station.ComputerID && phoneMatches(current, env.BindingID, env.PhoneHash) && phoneMatches(current, base.PhoneBindingID, base.PhoneHash) && phoneMatches(current, station.PhoneBindingID, station.PhoneHash) && config.RemoteIdentity(current) == config.RemoteIdentity(base)
}
