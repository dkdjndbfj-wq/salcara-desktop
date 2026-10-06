package hubclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/desktopcompanion"
	"salcara/bridge/internal/launcher"
)

// Explicit metadata structs prevent accidental serialization of vault entries,
// executable paths, account IDs, credentials or applied fingerprints.
type agentAPIStatus struct {
	Name       string `json:"name"`
	Model      string `json:"model"`
	Protocol   string `json:"protocol"`
	Configured bool   `json:"configured"`
	Pending    bool   `json:"pending"`
	// Source: "phone" (picked on the phone for remote tasks), "computer"
	// (applied to the tool in Bridge) or "tool" (the tool's own login/config).
	Source    string `json:"source"`
	AccountID string `json:"accountId,omitempty"`
}

// remoteAPIOption is one vault entry a phone may pick. A matched station is
// public routing metadata; API addresses and keys are otherwise never sent.
type remoteAPIOption struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Models  []string          `json:"models"`
	Station *remoteAPIStation `json:"station,omitempty"`
}

// remoteAPIStation is public station metadata only. It lets the phone tie a
// key whose API origin is this relay to an already-paired station record; no
// device secret or API credential crosses the Hub.
type remoteAPIStation struct {
	HubURL   string `json:"hubUrl"`
	DeviceID string `json:"deviceId"`
}

type agentStatus struct {
	ID                    string                             `json:"id"`
	Name                  string                             `json:"name"`
	Tool                  string                             `json:"tool"`
	ControlSurface        string                             `json:"controlSurface"`
	Available             bool                               `json:"available"`
	API                   agentAPIStatus                     `json:"api"`
	RemoteSendSupported   bool                               `json:"remoteSendSupported"`
	ConversationAPISwitch bool                               `json:"conversationApiSwitch"`
	NativeConnection      *desktopcompanion.NativeConnection `json:"nativeConnection,omitempty"`
	// DesktopLive: Codex Desktop live mode is authorized; phone messages to
	// these threads are executed by the desktop app itself (window updates live).
	DesktopLive     *desktopLive           `json:"desktopLive,omitempty"`
	SessionScope    string                 `json:"sessionScope,omitempty"`
	ReadOnlyHistory *readOnlyHistoryStatus `json:"desktopHistory,omitempty"`
}

type desktopLive struct {
	Active            bool                                `json:"active"`
	ExpiresAt         int64                               `json:"expiresAt"`
	SessionKeys       []string                            `json:"sessionKeys"`
	Capabilities      desktopcompanion.NativeCapabilities `json:"capabilities"`
	ApprovalTransport string                              `json:"approvalTransport,omitempty"`
}

func (c *Client) agentStatus(ctx context.Context) map[string]any {
	cfg := config.Config{}
	if c.o.Store != nil {
		cfg = c.o.Store.Get()
	}
	discover := c.o.DiscoverTools
	if discover == nil {
		discover = launcher.Discover
	}
	available := map[string]bool{}
	for _, t := range discover(ctx, cfg.LocalToolPaths) {
		if launcher.ValidTarget(t.ID) {
			available[t.ID] = t.Available
		}
	}
	// "codex" covers every Codex thread (App, CLI and IDE share one store) and
	// "claude" every Claude Code session. Desktop entries remain for old phones.
	list := []agentStatus{
		{ID: "codex-desktop", Name: "Codex Desktop", Tool: "codex", ControlSurface: "desktop"},
		{ID: "claude-desktop", Name: "Claude Desktop", Tool: "claude", ControlSurface: "desktop"},
		{ID: "codex", Name: "Codex", Tool: "codex", ControlSurface: "cli"},
		{ID: "claude", Name: "Claude Code", Tool: "claude", ControlSurface: "cli"},
	}
	var native *desktopcompanion.NativeConnection
	if c.o.Desktop != nil {
		if st, err := c.o.Desktop.NativeStatus(ctx); err == nil && desktopcompanion.ValidateNativeConnection(st, nowMS()) {
			native = &st
		}
	}
	for i := range list {
		s := &list[i]
		s.ConversationAPISwitch = s.ID == "codex" || s.ID == "claude" || s.ID == "claude-desktop"
		s.Available = available[s.ID]
		if s.ControlSurface == "cli" {
			s.API = remoteAPIStatus(cfg, s.ID)
		} else if s.ID == "claude-desktop" {
			// The phone continues Claude Desktop's Code sessions with the Claude Code
			// worker, so it reports (and sets) that worker's API.
			s.API = remoteAPIStatus(cfg, "claude")
		} else {
			s.API = apiConfirmation(cfg, s.ID)
			s.API.Source = "computer"
			if !s.API.Configured && !s.API.Pending {
				s.API.Source = "tool"
			}
		}
		// Installation is not upstream verification or live desktop control.
		s.RemoteSendSupported = s.ControlSurface == "cli" && s.Available
		if s.ID == "claude-desktop" {
			s.SessionScope = "code"
			s.ReadOnlyHistory = c.claudeReadOnlyHistoryStatus(ctx)
			// Continuing its Code sessions needs the Claude Code CLI on this computer.
			s.RemoteSendSupported = s.Available && available["claude"]
		}
		if s.ID == "codex-desktop" && native != nil {
			s.NativeConnection = native
			s.RemoteSendSupported = true
		}
		if s.ID == "codex" && native != nil {
			s.DesktopLive = &desktopLive{Active: true, ExpiresAt: native.ExpiresAt, SessionKeys: native.SessionKeys, Capabilities: native.Capabilities, ApprovalTransport: native.ApprovalTransport}
		}
	}
	return map[string]any{"agents": list, "apis": remoteAPIOptions(cfg), "policy": map[string]bool{"autoAll": cfg.AllowPhoneAutoAll}}
}

// APIHandle is an opaque, per-physical-computer reference to a vault entry.
// Keeping the salt stable across that computer's saved Hub stations lets the
// phone perform an atomic A→B station handover without learning a vault ID.
func APIHandle(cfg config.Config, id string) string {
	salt := cfg.ComputerID
	if salt == "" {
		// Legacy installations have no physical identity yet; retain their old
		// station-scoped behavior until the next QR upgrade establishes one.
		salt = cfg.DeviceSecret + "\x00" + cfg.DeviceID
	}
	sum := sha256.Sum256([]byte("salcara-remote-api-v2\x00" + salt + "\x00" + id))
	return "api_" + hex.EncodeToString(sum[:10])
}

// AccountForHandle resolves a phone-supplied handle back to a vault ID.
func AccountForHandle(cfg config.Config, handle string) (string, bool) {
	for _, a := range cfg.LocalAccounts {
		if APIHandle(cfg, a.ID) == handle {
			return a.ID, true
		}
	}
	return "", false
}

// remoteAPIStatus describes what a Bridge-run worker of this family uses now.
func remoteAPIStatus(cfg config.Config, family string) agentAPIStatus {
	if a, ok := cfg.RemoteToolAccount(family); ok {
		protocol := ""
		if a.Protocol == "responses" || a.Protocol == "chat" || a.Protocol == "anthropic" {
			protocol = a.Protocol
		}
		return agentAPIStatus{Name: safeConfirmationLabel(a.Name, "已命名 API", cfg), Model: safeConfirmationLabel(a.Model, "", cfg),
			Protocol: protocol, Configured: true, Source: "phone", AccountID: APIHandle(cfg, a.ID)}
	}
	st := apiConfirmation(cfg, family)
	st.Source = "computer"
	if !st.Configured && !st.Pending {
		st.Source = "tool"
	} else if applied, ok := cfg.AppliedToolAccount(family); ok && st.Configured {
		st.AccountID = APIHandle(cfg, applied.ID)
	}
	return st
}

func remoteAPIOptions(cfg config.Config) []remoteAPIOption {
	out := []remoteAPIOption{}
	for _, a := range cfg.LocalAccounts {
		models := []string{}
		for _, m := range a.Models {
			if label := safeConfirmationLabel(m, "", cfg); label != "" {
				models = append(models, label)
			}
			if len(models) == 80 {
				break
			}
		}
		out = append(out, remoteAPIOption{ID: APIHandle(cfg, a.ID), Name: safeConfirmationLabel(a.Name, "已命名 API", cfg), Models: models, Station: stationForAPI(cfg, a)})
	}
	return out
}

// stationForAPI returns a station only when the API origin identifies exactly
// one saved station. Different API and Hub origins are intentionally left
// decoupled: a user may route a key through A while the phone remains paired
// to B, and an ambiguous same-origin setup must never guess.
func stationForAPI(cfg config.Config, account config.LocalAccount) *remoteAPIStation {
	// Without the physical identity the opaque API handle is intentionally
	// station-scoped for legacy installs, so it cannot safely accompany A→B.
	if cfg.ComputerID == "" {
		return nil
	}
	apiURL, err := url.Parse(strings.TrimSpace(account.BaseURL))
	if err != nil || apiURL.Scheme == "" || apiURL.Host == "" || apiURL.User != nil || apiURL.RawQuery != "" || apiURL.Fragment != "" {
		return nil
	}
	var matches []remoteAPIStation
	seen := map[string]bool{}
	for _, station := range cfg.RemoteConnections {
		hubURL := config.NormalizeHubURL(station.HubURL)
		if hubURL == "" || station.DeviceID == "" || station.DeviceSecret == "" {
			continue
		}
		hubParsed, parseErr := url.Parse(hubURL)
		if parseErr != nil || hubParsed.Scheme == "" || hubParsed.Host == "" || hubParsed.User != nil || hubParsed.RawQuery != "" || hubParsed.Fragment != "" {
			continue
		}
		if !strings.EqualFold(apiURL.Scheme, hubParsed.Scheme) || !strings.EqualFold(apiURL.Host, hubParsed.Host) {
			continue
		}
		key := strings.ToLower(hubURL) + "\x00" + station.DeviceID
		if seen[key] {
			continue
		}
		seen[key] = true
		matches = append(matches, remoteAPIStation{HubURL: hubURL, DeviceID: station.DeviceID})
		if len(matches) > 1 {
			return nil
		}
	}
	if len(matches) != 1 {
		return nil
	}
	return &matches[0]
}

func apiConfirmation(cfg config.Config, target string) agentAPIStatus {
	selected, selectedFound := cfg.SelectedToolAPI(target)
	applied, appliedFound := cfg.AppliedToolAccount(target)
	// A matching fingerprint alone is insufficient for corrupt/imported configs.
	verified := applied
	appliedValid := appliedFound && config.ValidateLocalAccount(&verified) == nil
	a := applied
	pending := selectedFound && (!appliedValid || selected.ID != applied.ID || config.AccountFingerprint(selected) != config.AccountFingerprint(applied))
	if !appliedValid {
		if !selectedFound {
			return agentAPIStatus{}
		}
		a = selected
	}
	protocol := ""
	if a.Protocol == "responses" || a.Protocol == "chat" || a.Protocol == "anthropic" {
		protocol = a.Protocol
	}
	return agentAPIStatus{
		Name:       safeConfirmationLabel(a.Name, "已命名 API", cfg),
		Model:      safeConfirmationLabel(a.Model, "", cfg),
		Protocol:   protocol,
		Configured: appliedValid,
		Pending:    pending,
	}
}

var confirmationAddress = regexp.MustCompile(`(?i)(?:[a-z0-9-]+\.)+[a-z]{2,63}(?:\b|/)|(?:[0-9]{1,3}\.){3}[0-9]{1,3}`)

// Names/models are user-editable strings: someone can paste a credential or URL
// into them. Fail closed rather than forwarding such labels to the relay.
func safeConfirmationLabel(raw, fallback string, cfg config.Config) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return fallback
	}
	lower := strings.ToLower(s)
	if len([]rune(s)) > 200 || strings.ContainsAny(s, "\\\r\n\x00") || strings.Contains(lower, "://") || strings.HasPrefix(s, "/") || (len(s) > 2 && s[1] == ':' && (s[2] == '/' || s[2] == '\\')) || confirmationAddress.MatchString(s) || strings.Contains(lower, "sk-") || strings.Contains(lower, "bearer ") {
		return fallback
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return fallback
		}
	}
	credentials := []string{cfg.AccountKey, cfg.CodexKey, cfg.ClaudeKey, cfg.GatewayKey, cfg.DeviceSecret}
	for _, a := range cfg.LocalAccounts {
		credentials = append(credentials, a.Key)
	}
	for _, p := range cfg.RemoteConnections {
		credentials = append(credentials, p.DeviceSecret)
	}
	for _, v := range credentials {
		if v != "" && strings.Contains(lower, strings.ToLower(v)) {
			return fallback
		}
	}
	private := []string{cfg.AccountKey, cfg.CodexKey, cfg.ClaudeKey, cfg.GatewayKey, cfg.DeviceSecret, cfg.DeviceID, cfg.RelayRoot, cfg.HubURL}
	for _, a := range cfg.LocalAccounts {
		private = append(private, a.Key, a.BaseURL, a.ID, a.Workspace)
	}
	for _, p := range cfg.RemoteConnections {
		private = append(private, p.DeviceSecret, p.DeviceID, p.HubURL)
	}
	for _, p := range cfg.LocalToolPaths {
		private = append(private, p)
	}
	for _, b := range cfg.ToolAPIApplied {
		private = append(private, b.Fingerprint, b.Provider)
	}
	for _, v := range private {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		// Even a short test/imported key cannot be copied verbatim into a label;
		// substring checks are limited to longer values to avoid hiding "API".
		vl := strings.ToLower(v)
		if lower == vl || (len(v) >= 8 && strings.Contains(lower, vl)) {
			return fallback
		}
		if u, err := url.Parse(v); err == nil && u.Hostname() != "" && strings.Contains(lower, strings.ToLower(u.Hostname())) {
			return fallback
		}
	}
	return s
}
