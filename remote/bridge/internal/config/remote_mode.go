package config

// EffectiveRemoteMode resolves older installations conservatively. Direct
// wins if both old flags were enabled; restarting never starts two transports.
func (c Config) EffectiveRemoteMode() string {
	if c.DirectRelayEnabled {
		return "direct"
	}
	switch c.ActiveRemoteMode {
	case "none", "hub", "bot":
		return c.ActiveRemoteMode
	case "direct":
		return "none" // a disabled direct listener never falls back to a Hub
	case "":
		if !c.LoggedIn() {
			return "none"
		}
		if group, _, _ := c.BotEnrollment(); group != "" {
			return "bot"
		}
		return "hub"
	default:
		return "none"
	}
}

func (c Config) HubRemoteEnabled() bool {
	if c.DirectRelayEnabled {
		return false
	}
	return c.ActiveRemoteMode == "" || c.ActiveRemoteMode == "hub" || c.ActiveRemoteMode == "bot"
}

func (c Config) DirectRemoteEnabled() bool {
	return c.DirectRelayEnabled && c.EffectiveRemoteMode() == "direct"
}

func (c *Config) ActivateRemoteMode(mode string) {
	previous := c.EffectiveRemoteMode()
	c.ActiveRemoteMode = mode
	c.DirectRelayEnabled = mode == "direct"
	// A mode change never removes phone identities, source choices or tokens.
	if previous != mode {
		c.RemoteStationAuthorization = nil
	}
}
