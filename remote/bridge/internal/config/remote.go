package config

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Every remote origin/path gets its own identity, independent of model keys.
type RemoteConnection struct {
	HubURL       string `json:"hubUrl"`
	DeviceID     string `json:"deviceId"`
	DeviceSecret string `json:"deviceSecret"`
}

// PendingPhoneRevoke is an exact, retired phone generation on one saved Hub.
// Device credentials are resolved from RemoteConnections when retrying; a new
// pairing must never be erased by a delayed, unconditional revoke.
type PendingPhoneRevoke struct {
	HubURL    string `json:"hubUrl"`
	DeviceID  string `json:"deviceId"`
	BindingID string `json:"bindingId"`
	PhoneHash string `json:"phoneHash"`
}

// NormalizeHubURL compares saved station identities without allowing the
// public protocol suffix to create a second spelling of the same station.
// It deliberately does not resolve hosts or follow redirects.
func NormalizeHubURL(raw string) string {
	s := strings.TrimRight(strings.TrimSpace(raw), "/")
	if len(s) >= 3 && strings.EqualFold(s[len(s)-3:], "/v1") {
		s = s[:len(s)-3]
	}
	return strings.TrimRight(s, "/")
}

// SavedRemoteConnection returns only the opaque station record already saved
// on this computer. The device secret never crosses the phone command wire.
func (c Config) SavedRemoteConnection(hubURL, deviceID string) (RemoteConnection, bool) {
	want := NormalizeHubURL(hubURL)
	for _, item := range c.RemoteConnections {
		if NormalizeHubURL(item.HubURL) == want && item.DeviceID == deviceID && item.DeviceSecret != "" {
			return item, true
		}
	}
	return RemoteConnection{}, false
}

func (c *Config) ConnectRemote(root, hubURL string) {
	root = strings.TrimRight(strings.TrimSpace(root), "/")
	hubURL = NormalizeHubURL(hubURL)
	// Treat the public protocol suffix and a trailing slash as spelling, not a
	// second station. This prevents a user who pastes `.../v1/` after already
	// saving `.../salcara-hub` from creating a duplicate device identity.
	stationKey := NormalizeHubURL(hubURL)
	var saved RemoteConnection
	for _, p := range c.RemoteConnections {
		if NormalizeHubURL(p.HubURL) == stationKey {
			saved = p
			break
		}
	}
	if saved.DeviceID == "" {
		saved = RemoteConnection{HubURL: hubURL, DeviceID: NewUUID(), DeviceSecret: RandomToken(32)}
		c.RemoteConnections = append(c.RemoteConnections, saved)
	} else if saved.HubURL != hubURL {
		// Keep one canonical spelling in the persisted record so later station
		// comparisons and handover payload hashes remain stable.
		for i := range c.RemoteConnections {
			if NormalizeHubURL(c.RemoteConnections[i].HubURL) == stationKey {
				c.RemoteConnections[i].HubURL = hubURL
				break
			}
		}
		saved.HubURL = hubURL
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
