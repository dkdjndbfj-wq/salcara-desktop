package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

const MaxRemoteHubBindings = 32

// Every remote origin/path and transport mode gets its own identity,
// independent of model keys. Hub and Bot may share a server without sharing
// device credentials or replacing each other's saved binding.
type RemoteConnection struct {
	RemoteSupport     *StationObservation `json:"remoteSupport,omitempty"`
	HubURL            string              `json:"hubUrl"`
	DeviceID          string              `json:"deviceId"`
	DeviceSecret      string              `json:"deviceSecret"`
	EnrollmentClaim   string              `json:"enrollmentClaim,omitempty"`
	HubInstanceID     string              `json:"hubInstanceId,omitempty"`
	IdentityPublicKey string              `json:"identityPublicKey,omitempty"`
	IngressURL        string              `json:"ingressUrl,omitempty"`
	BotGroupID        string              `json:"botGroupId,omitempty"`
	BotInvite         string              `json:"botInvite,omitempty"`
	BotSignature      string              `json:"botSignature,omitempty"`
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
	if strings.HasSuffix(s, "/v1") {
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

// CanonicalRemoteHubBindings accepts only Hubs already registered on this
// computer. It returns their saved canonical URLs without exposing credentials.
func CanonicalRemoteHubBindings(c Config, hubURLs []string) ([]string, error) {
	if len(hubURLs) > MaxRemoteHubBindings {
		return nil, errors.New("远程站点数量过多")
	}
	saved := make(map[string]string, len(c.RemoteConnections))
	for _, station := range c.RemoteConnections {
		key := NormalizeHubURL(station.HubURL)
		if key != "" && station.DeviceID != "" && station.DeviceSecret != "" {
			saved[key] = station.HubURL
		}
	}
	out := make([]string, 0, len(hubURLs))
	seen := make(map[string]bool, len(hubURLs))
	for _, raw := range hubURLs {
		key := NormalizeHubURL(raw)
		canonical, ok := saved[key]
		if !ok || key == "" {
			return nil, errors.New("只能选择这台电脑已登记的中转站")
		}
		if seen[key] {
			return nil, errors.New("远程站点不能重复")
		}
		seen[key] = true
		out = append(out, canonical)
	}
	return out, nil
}

// RemoteHubAllowsAccount is a local account-sharing preference. The Hub's
// authenticated StationPolicy remains a separate, final authorization check.
func (c Config) RemoteHubAllowsAccount(hubURL string, account LocalAccount) bool {
	if !account.RemoteHubBindingsSet {
		return true
	}
	want := NormalizeHubURL(hubURL)
	if want == "" {
		return false
	}
	for _, saved := range account.RemoteHubURLs {
		if NormalizeHubURL(saved) == want {
			return true
		}
	}
	return false
}

func (c *Config) ConnectRemote(root, hubURL string) {
	c.connectRemote(root, hubURL, "", "")
}

func (c *Config) ConnectRemoteWithEnrollmentClaim(root, hubURL, claim string) {
	c.connectRemote(root, hubURL, strings.TrimSpace(claim), "")
}

// ReenrollRemote rotates only this station's retired device binding. Local
// accounts, projects, tool selections and conversations remain independent.
// Call only after the user explicitly requests re-enrollment. The station may
// require a fresh enrollment claim before it accepts the new device.
func (c *Config) ReenrollRemote(root, hubURL, claim string) {
	want := NormalizeHubURL(hubURL)
	for i := range c.RemoteConnections {
		saved := &c.RemoteConnections[i]
		if NormalizeHubURL(saved.HubURL) == want && saved.BotGroupID == "" {
			saved.DeviceID, saved.DeviceSecret = NewUUID(), RandomToken(32)
			saved.EnrollmentClaim = strings.TrimSpace(claim)
			saved.HubInstanceID, saved.IdentityPublicKey, saved.IngressURL = "", "", ""
			saved.RemoteSupport = nil
			c.activateRemoteConnection(root, *saved)
			return
		}
	}
	c.ConnectRemoteWithEnrollmentClaim(root, hubURL, claim)
}

// RemoteIngress returns a persistent identity pin even when its routing
// manifest has expired. Cache expiry must never make a replacement Hub trusted.
func (c Config) RemoteIngress() (id, publicKey, endpoint string) {
	if saved, ok := c.SavedRemoteConnection(c.EffectiveHubURL(), c.DeviceID); ok {
		return saved.HubInstanceID, saved.IdentityPublicKey, saved.IngressURL
	}
	return "", "", ""
}

// ConnectRemoteWithBotBinding registers this computer with a Bot group while
// retaining a fresh device identity. The invite is cleared by the first
// successful Hub registration; it is only a transient onboarding secret.
func (c *Config) ConnectRemoteWithBotBinding(root, hubURL, groupID, invite, signature string) {
	c.connectRemote(root, hubURL, "", strings.TrimSpace(groupID))
	c.ActivateRemoteMode("bot")
	want := NormalizeHubURL(hubURL)
	for i := range c.RemoteConnections {
		if NormalizeHubURL(c.RemoteConnections[i].HubURL) == want && c.RemoteConnections[i].DeviceID == c.DeviceID {
			c.RemoteConnections[i].BotGroupID = strings.TrimSpace(groupID)
			c.RemoteConnections[i].BotInvite = strings.TrimSpace(invite)
			c.RemoteConnections[i].BotSignature = strings.TrimSpace(signature)
			c.RemoteConnections[i].EnrollmentClaim = ""
		}
	}
}

func (c *Config) connectRemote(root, hubURL, enrollmentClaim, botGroupID string) {
	hubURL = NormalizeHubURL(hubURL)
	// Treat the public protocol suffix and a trailing slash as spelling, not a
	// second station. This prevents a user who pastes `.../v1/` after already
	// saving `.../salcara-hub` from creating a duplicate device identity.
	stationKey := NormalizeHubURL(hubURL)
	savedIndex := -1
	for i := range c.RemoteConnections {
		if NormalizeHubURL(c.RemoteConnections[i].HubURL) == stationKey && c.RemoteConnections[i].BotGroupID == botGroupID {
			savedIndex = i
			break
		}
	}
	if savedIndex < 0 {
		c.RemoteConnections = append(c.RemoteConnections, RemoteConnection{
			HubURL: hubURL, DeviceID: NewUUID(), DeviceSecret: RandomToken(32), EnrollmentClaim: enrollmentClaim, BotGroupID: botGroupID,
		})
		savedIndex = len(c.RemoteConnections) - 1
	} else {
		// Keep one canonical spelling in the persisted record so later station
		// comparisons and handover payload hashes remain stable.
		saved := &c.RemoteConnections[savedIndex]
		if saved.HubURL != hubURL {
			saved.HubURL = hubURL
		}
		if enrollmentClaim != "" {
			saved.EnrollmentClaim = enrollmentClaim
		}
	}
	c.activateRemoteConnection(root, c.RemoteConnections[savedIndex])
}

// RestoreRemoteConnection selects an existing exact device identity. It never
// creates or rewrites a dormant Hub/Bot binding at the same server address.
func (c *Config) RestoreRemoteConnection(root, hubURL, deviceID string) error {
	saved, ok := c.SavedRemoteConnection(hubURL, deviceID)
	if !ok {
		return errors.New("已保存的远程连接已变化，请重新选择")
	}
	c.activateRemoteConnection(root, saved)
	return nil
}

func (c *Config) activateRemoteConnection(root string, saved RemoteConnection) {
	previousIdentity := RemoteIdentity(*c)
	c.RelayRoot, c.HubURL, c.RemoteDeviceOnly = strings.TrimRight(strings.TrimSpace(root), "/"), NormalizeHubURL(saved.HubURL), true
	c.DeviceID, c.DeviceSecret = saved.DeviceID, saved.DeviceSecret
	mode := "hub"
	if saved.BotGroupID != "" {
		mode = "bot"
	}
	c.ActivateRemoteMode(mode)
	if RemoteIdentity(*c) != previousIdentity {
		c.RemoteStationAuthorization = nil
		c.RemoteStationObservation = CloneStationObservation(saved.RemoteSupport)
	}
}

func (c Config) DeviceEnrollmentClaim() string {
	hubURL := NormalizeHubURL(c.EffectiveHubURL())
	for _, saved := range c.RemoteConnections {
		if NormalizeHubURL(saved.HubURL) == hubURL && saved.DeviceID == c.DeviceID {
			return saved.EnrollmentClaim
		}
	}
	return ""
}

func (c Config) BotEnrollment() (groupID, invite, signature string) {
	hubURL := NormalizeHubURL(c.EffectiveHubURL())
	for _, saved := range c.RemoteConnections {
		if NormalizeHubURL(saved.HubURL) == hubURL && saved.DeviceID == c.DeviceID {
			return saved.BotGroupID, saved.BotInvite, saved.BotSignature
		}
	}
	return "", "", ""
}

func (c *Config) ClearBotEnrollment(hubURL, deviceID, invite string) bool {
	wantHub := NormalizeHubURL(hubURL)
	for i := range c.RemoteConnections {
		saved := &c.RemoteConnections[i]
		if NormalizeHubURL(saved.HubURL) == wantHub && saved.DeviceID == deviceID && saved.BotInvite == invite {
			saved.BotInvite, saved.BotSignature = "", ""
			return true
		}
	}
	return false
}

func (c *Config) ClearDeviceEnrollmentClaim(hubURL, deviceID, claim string) bool {
	wantHub := NormalizeHubURL(hubURL)
	for i := range c.RemoteConnections {
		saved := &c.RemoteConnections[i]
		if NormalizeHubURL(saved.HubURL) == wantHub && saved.DeviceID == deviceID && saved.EnrollmentClaim == claim {
			saved.EnrollmentClaim = ""
			return true
		}
	}
	return false
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
