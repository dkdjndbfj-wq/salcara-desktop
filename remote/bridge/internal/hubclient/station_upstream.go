package hubclient

import (
	"context"
	"errors"

	"salcara/bridge/internal/config"
)

// Catalogues bypass the HTTP gateway, so they enforce the same station at
// their actual request boundary, including calls made during API selection.
func (c *Client) validateStationUpstream(ctx context.Context, account config.LocalAccount) error {
	if c.o.Store == nil {
		return nil
	}
	cfg := c.o.Store.Get()
	if !cfg.RemoteExecutionArmed {
		return nil
	}
	env, hasSource := executionSourceFromContext(ctx)
	_, remote := commandGuardFromContext(ctx)
	if !hasSource && !remote {
		return nil
	} // explicit local console operation
	if !hasSource || env.Source == nil || str(env.Command, "type") == "remote.station.switch" {
		return errors.New("未取得当前站点授权，未访问上游")
	}
	if err := c.validateCommandGuard(ctx, str(env.Command, "type")); err != nil {
		return err
	}
	if err := c.validateExecutionSourceFor(env, cfg); err != nil {
		return err
	}
	current, exists := cfg.LocalAccount(account.ID)
	if !exists || config.AccountFingerprint(current) != config.AccountFingerprint(account) || !cfg.RemoteHubAllowsAccount(cfg.EffectiveHubURL(), current) || !config.StationAllowsAccount(env.Source.Station, current) {
		return errors.New("API 来源或中转站已变化，未访问上游")
	}
	return nil
}
