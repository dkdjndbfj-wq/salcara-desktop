package config

import (
	"crypto/sha256"
	"encoding/hex"
)

// Every remote origin/path gets its own identity, independent of model keys.
type RemoteConnection struct {
	HubURL       string `json:"hubUrl"`
	DeviceID     string `json:"deviceId"`
	DeviceSecret string `json:"deviceSecret"`
}

func (c *Config) ConnectRemote(root, hubURL string) {
	var saved RemoteConnection
	for _, p := range c.RemoteConnections {
		if p.HubURL == hubURL {
			saved = p
			break
		}
	}
	if saved.DeviceID == "" {
		saved = RemoteConnection{HubURL: hubURL, DeviceID: NewUUID(), DeviceSecret: RandomToken(32)}
		c.RemoteConnections = append(c.RemoteConnections, saved)
	}
	c.RelayRoot, c.HubURL, c.RemoteDeviceOnly = root, hubURL, true
	c.DeviceID, c.DeviceSecret = saved.DeviceID, saved.DeviceSecret
}

func RemoteIdentity(c Config) string {
	mode := "legacy"
	key := c.AccountKey
	if c.RemoteDeviceOnly {
		mode, key = "device", ""
	}
	h := sha256.Sum256([]byte(mode + "\x00" + c.EffectiveHubURL() + "\x00" + c.DeviceID + "\x00" + c.DeviceSecret + "\x00" + key))
	return hex.EncodeToString(h[:])
}
