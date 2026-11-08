package hubclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/desktopcompanion"
	"salcara/bridge/internal/launcher"
	"salcara/bridge/internal/protocol"
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
	SourceType        string               `json:"sourceType"`
	RemoteEligibility remoteAPIEligibility `json:"remoteEligibility"`
	Stations          []remoteAPIStation   `json:"stations"`
	ID                string               `json:"id"`
	Name              string               `json:"name"`
	Models            []string             `json:"models"`
	Station           *remoteAPIStation    `json:"station,omitempty"`
}

type remoteAPIEligibility struct {
	Supported     bool   `json:"supported"`
	Reason        string `json:"reason"`
	APIRemote     bool   `json:"apiRemote"`
	AccountRemote bool   `json:"accountRemote"`
}

// remoteAPIStation is public station metadata only. It lets the phone tie a
// key whose API origin is this relay to an already-paired station record; no
// device secret or API credential crosses the Hub.
type remoteAPIStation struct {
	HubURL        string `json:"hubUrl"`
	DeviceID      string `json:"deviceId"`
	APIRemote     bool   `json:"apiRemote"`
	AccountRemote bool   `json:"accountRemote"`
	CheckedAt     int64  `json:"checkedAt"`
}

type agentStatus struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Tool           string `json:"tool"`
	APIFamily      string `json:"apiFamily,omitempty"`
	ControlSurface string `json:"controlSurface"`
	// Execution describes the Code worker separately from the displayed
	// Desktop identity. It does not grant native Chat/Cowork control.
	Execution             *agentExecution                    `json:"execution,omitempty"`
	Available             bool                               `json:"available"`
	API                   agentAPIStatus                     `json:"api"`
	RemoteSendSupported   bool                               `json:"remoteSendSupported"`
	Capabilities          protocol.AgentCapabilities         `json:"capabilities"`
	ConversationAPISwitch bool                               `json:"conversationApiSwitch"`
	NativeConnection      *desktopcompanion.NativeConnection `json:"nativeConnection,omitempty"`
	// DesktopLive: Codex Desktop live mode is authorized; phone messages to
	// these threads are executed by the desktop app itself (window updates live).
	DesktopLive     *desktopLive           `json:"desktopLive,omitempty"`
	SessionScope    string                 `json:"sessionScope,omitempty"`
	ReadOnlyHistory *readOnlyHistoryStatus `json:"desktopHistory,omitempty"`
}

type agentExecution struct {
	Tool           string `json:"tool"`
	ControlSurface string `json:"controlSurface"`
	Entrypoint     string `json:"entrypoint,omitempty"`
}

type desktopLive struct {
	Active                 bool                                 `json:"active"`
	ExpiresAt              int64                                `json:"expiresAt"`
	SessionKeys            []string                             `json:"sessionKeys"`
	Capabilities           desktopcompanion.NativeCapabilities  `json:"capabilities"`
	ApprovalTransport      string                               `json:"approvalTransport,omitempty"`
	ModelOverrideTransport string                               `json:"modelOverrideTransport,omitempty"`
	ModelOptions           *desktopcompanion.NativeModelSupport `json:"modelOptions,omitempty"`
	AccountID              string                               `json:"accountId,omitempty"`
	APIName                string                               `json:"apiName,omitempty"`
	APIModel               string                               `json:"apiModel,omitempty"`
}

func (c *Client) agentStatus(ctx context.Context) map[string]any {
	direct := isLocalExecution(ctx) && IsDirectExecution(ctx)
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
		{ID: "claude-desktop", Name: "Claude Desktop", Tool: "claude", APIFamily: "claude", ControlSurface: "desktop"},
		{ID: "codex", Name: "Codex", Tool: "codex", ControlSurface: "cli"},
		{ID: "claude", Name: "Claude Code", Tool: "claude", APIFamily: "claude", ControlSurface: "cli"},
	}
	if direct {
		list = list[2:]
	}
	var native *desktopcompanion.NativeConnection
	if c.o.Desktop != nil && !direct {
		if st, err := c.o.Desktop.NativeStatus(ctx); err == nil && desktopcompanion.ValidateNativeConnection(st, nowMS()) {
			st.Capabilities.Navigate = !cfg.RemoteExecutionArmed && c.o.NavigateDesktop != nil
			if _, ok := c.o.Desktop.(nativeOptionsSender); !ok {
				st.Capabilities.ModelOverride = false
				st.ModelOptions, st.ModelOverrideTransport = nil, ""
			}
			if _, ok := c.o.Desktop.(nativeApprovalResponder); !ok {
				st.Capabilities.Approval = false
				st.ApprovalTransport = ""
			}
			if cfg.RemoteExecutionArmed {
				// Reading/stopping a native session is safe; its actual provider
				// is not attested yet, so do not advertise executable operations.
				st.Capabilities.Send, st.Capabilities.Approval = false, false
				st.Capabilities.ModelOverride = false
				st.ModelOptions, st.ModelOverrideTransport = nil, ""
				st.ApprovalTransport = ""
			}
			native = &st
		}
	}
	for i := range list {
		s := &list[i]
		s.Capabilities = builtinCapabilities(s.ID)
		if s.ControlSurface == "cli" && c.mgrReady.Load() {
			if m := c.manager(); m != nil {
				if d, err := cliAgentDescriptor(m.Get(s.Tool)); err == nil {
					s.Capabilities = publishedRemoteCapabilities(m.Get(s.Tool), d.Capabilities)
				} else {
					s.Capabilities = protocol.AgentCapabilities{}
				}
			}
		}
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
			if cfg.RemoteExecutionArmed {
				s.API = agentAPIStatus{Source: "tool"}
			}
		}
		// Installation is not upstream verification or live desktop control.
		s.RemoteSendSupported = s.ControlSurface == "cli" && s.Available && s.Capabilities.Send
		if s.ID == "claude-desktop" {
			s.SessionScope = "code"
			s.Execution = &agentExecution{Tool: "claude", ControlSurface: "cli", Entrypoint: "claude-desktop"}
			s.ReadOnlyHistory = c.claudeReadOnlyHistoryStatus(ctx)
			// Continuing its Code sessions needs the Claude Code CLI on this computer.
			workerCaps := builtinCapabilities("claude")
			if c.mgrReady.Load() {
				if m := c.manager(); m != nil {
					if d, err := cliAgentDescriptor(m.Get("claude")); err == nil {
						workerCaps = publishedRemoteCapabilities(m.Get("claude"), d.Capabilities)
					} else {
						workerCaps = protocol.AgentCapabilities{}
					}
				}
			}
			s.RemoteSendSupported = s.Available && available["claude"] && workerCaps.Send
			workerAvailable := available["claude"]
			historyAvailable := s.ReadOnlyHistory != nil && s.ReadOnlyHistory.Available
			s.Capabilities = protocol.AgentCapabilities{}
			if s.RemoteSendSupported {
				// Code uses the same CLI executor and must retain its complete,
				// already-validated contract, including newly added controls.
				s.Capabilities = workerCaps
				s.Capabilities.PermissionModes = append([]string(nil), workerCaps.PermissionModes...)
				s.Capabilities.ApprovalModes = append([]string(nil), workerCaps.ApprovalModes...)
			}
			// Native Chat/Cowork history grants only these read-only capabilities.
			s.Capabilities.List = workerAvailable && workerCaps.List || historyAvailable
			s.Capabilities.Read = workerAvailable && workerCaps.Read || historyAvailable
			s.Capabilities.ImageOutput = workerAvailable && workerCaps.ImageOutput || historyAvailable
		}
		if s.ID == "codex-desktop" && native != nil {
			s.NativeConnection = native
			s.RemoteSendSupported = native.Capabilities.Send
			s.Capabilities = protocol.AgentCapabilities{
				List: native.Capabilities.List, Read: native.Capabilities.Read,
				Send: native.Capabilities.Send, Interrupt: native.Capabilities.Interrupt,
				Approval: native.Capabilities.Approval, ModelOverride: native.Capabilities.ModelOverride,
			}
		}
		if s.ID == "codex" && native != nil {
			s.DesktopLive = &desktopLive{Active: true, ExpiresAt: native.ExpiresAt, SessionKeys: native.SessionKeys, Capabilities: native.Capabilities, ApprovalTransport: native.ApprovalTransport,
				ModelOverrideTransport: native.ModelOverrideTransport, ModelOptions: native.ModelOptions}
			if applied, _, ok := nativeAppliedAPI(cfg); ok && !cfg.RemoteExecutionArmed {
				s.DesktopLive.AccountID = APIHandle(cfg, applied.ID)
				s.DesktopLive.APIName = safeConfirmationLabel(applied.Name, "已命名 API", cfg)
				s.DesktopLive.APIModel = safeConfirmationLabel(applied.Model, "", cfg)
			}
		}
	}
	// Extension adapters are appended after the stable built-in records. Their
	// public descriptor is the only source of capability truth; absent or
	// malformed descriptors stay unavailable and cannot be sent through a
	// built-in adapter by accident.
	if c.mgrReady.Load() {
		var extensionAgents []agents.Agent
		if m := c.manager(); m != nil {
			extensionAgents = m.Agents()
		}
		seen := map[string]bool{}
		for _, item := range list {
			seen[item.ID] = true
		}
		for _, a := range extensionAgents {
			if a == nil || seen[a.ID()] || a.ID() == "codex" || a.ID() == "claude" {
				continue
			}
			d, valid := agents.CanonicalDescriptor(a)
			if !valid || !validAgentDescriptor(d) || d.ControlSurface != "cli" {
				continue
			}
			t := a.Detect(ctx)
			availableNow := t.Available && t.ID == a.ID()
			caps := publishedRemoteCapabilities(a, d.Capabilities)
			family := config.NormalizeAgentFamily(d.APIFamily)
			if family == "" {
				family = config.NormalizeAgentFamily(d.Tool)
			}
			api := agentAPIStatus{Source: "tool"}
			// Do not infer an extension Agent's API from the Codex/Claude active
			// account. Only an explicit shared-directory selection or an applied
			// binding is meaningful for a new adapter family.
			if family != "" {
				_, remoteSet := cfg.RemoteAPI[family]
				_, selectionSet := cfg.ToolAPISelections[family]
				_, appliedSet := cfg.AppliedToolBinding(family)
				if remoteSet || selectionSet || appliedSet {
					api = remoteAPIStatus(cfg, family)
				}
			}
			list = append(list, agentStatus{ID: d.ID, Name: safeAgentLabel(d.Name, d.ID), Tool: d.Tool, APIFamily: d.APIFamily,
				ControlSurface: d.ControlSurface, Available: availableNow, API: api, Capabilities: caps,
				RemoteSendSupported:   availableNow && caps.Send,
				ConversationAPISwitch: caps.ModelOverride})
			seen[d.ID] = true
		}
	}
	apis := remoteAPIOptions(cfg)
	if direct {
		// A pinned phone has no Hub station and no native-desktop authority.
		// Publish the same independent source the direct worker snapshot uses.
		directList := make([]agentStatus, 0, len(list))
		for _, item := range list {
			if item.ControlSurface != "cli" {
				continue
			}
			item.API = directAPIStatus(cfg, c.apiFamilyForAgent(item.ID))
			item.DesktopLive, item.NativeConnection = nil, nil
			directList = append(directList, item)
		}
		list, apis = directList, directAPIOptions(cfg)
	}
	return map[string]any{"agents": list, "apis": apis, "policy": map[string]bool{"autoAll": cfg.AllowPhoneAutoAll}}
}

func builtinCapabilities(id string) protocol.AgentCapabilities {
	switch id {
	case "codex", "claude":
		// This is the pre-manager capability snapshot used by the read-only
		// status endpoint.  The dispatch path still re-checks the live
		// descriptor before executing; advertising the two precise controls here
		// prevents the phone from hiding Stop/Continue merely because the Bridge
		// manager is still warming up.
		return protocol.AgentCapabilities{List: true, Read: true, Send: true, Interrupt: true, PreciseInterrupt: true, Continue: true, Approval: true,
			Questions: true, Subagents: true, Attachments: true, ImageInput: true, ImageOutput: true, ModelOverride: true}
	default:
		return protocol.AgentCapabilities{}
	}
}

var agentIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func validAgentDescriptor(d protocol.AgentDescriptor) bool {
	if !agentIDPattern.MatchString(d.ID) || !agentIDPattern.MatchString(d.Tool) || d.Name == "" || len([]rune(d.Name)) > 200 {
		return false
	}
	if d.APIFamily != "" && !config.ValidAgentFamily(d.APIFamily) {
		return false
	}
	return d.ControlSurface == "cli" || d.ControlSurface == "desktop" || d.ControlSurface == "read-only"
}

func safeAgentLabel(value, fallback string) string {
	value = strings.TrimSpace(value)
	lower := strings.ToLower(value)
	if value == "" || len([]rune(value)) > 200 || strings.ContainsAny(value, "\\\r\n\x00") || strings.Contains(lower, "://") || strings.Contains(lower, "sk-") || strings.Contains(lower, "bearer ") || confirmationAddress.MatchString(value) {
		return fallback
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fallback
		}
	}
	return value
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
	} else if applied, ok := cfg.AuthorizedAppliedToolAccount(family); ok && st.Configured {
		st.AccountID = APIHandle(cfg, applied.ID)
	}
	return st
}

func remoteAPIOptions(cfg config.Config) []remoteAPIOption {
	out := []remoteAPIOption{}
	for _, a := range cfg.LocalAccounts {
		models := []string{}
		seen := map[string]bool{}
		for _, m := range a.Models {
			// Stored Models are only picker hints and may predate a policy edit.
			// Filter them here as well as in the verified catalogue path so an
			// early agents.status response cannot advertise an explicitly denied
			// model before the phone asks models.list.
			if seen[m] || !config.ModelAllowed(a, m) {
				continue
			}
			if label := safeConfirmationLabel(m, "", cfg); label != "" {
				models = append(models, label)
				seen[m] = true
			}
			if len(models) == 80 {
				break
			}
		}
		out = append(out, remoteAPIOption{ID: APIHandle(cfg, a.ID), Name: safeConfirmationLabel(a.Name, "已命名 API", cfg), Models: models, Station: stationForAPI(cfg, a), SourceType: a.SourceType, RemoteEligibility: remoteEligibility(cfg, a), Stations: stationsForAPI(cfg, a)})
	}
	return out
}

// Legacy clients get a single station only for an unambiguous eligible match.
// New clients can choose among explicitly supporting saved stations.
func stationForAPI(cfg config.Config, account config.LocalAccount) *remoteAPIStation {
	matches := stationsForAPI(cfg, account)
	if len(matches) != 1 {
		return nil
	}
	return &matches[0]
}

func stationsForAPI(cfg config.Config, account config.LocalAccount) []remoteAPIStation {
	matches := []remoteAPIStation{}
	if cfg.ComputerID == "" {
		return matches
	}
	seen := map[string]bool{}
	for _, station := range cfg.RemoteConnections {
		hubURL := config.NormalizeHubURL(station.HubURL)
		if hubURL == "" || station.DeviceID == "" || station.DeviceSecret == "" {
			continue
		}
		if !cfg.RemoteHubAllowsAccount(hubURL, account) {
			continue
		}
		observation := stationObservation(cfg, hubURL, station.DeviceID)
		if !observation.Available() || !config.StationAllowsAccount(observation.Station, account) {
			continue
		}
		key := hubURL + "\x00" + station.DeviceID
		if seen[key] {
			continue
		}
		seen[key] = true
		api, official, checked := stationCapabilities(observation)
		matches = append(matches, remoteAPIStation{HubURL: hubURL, DeviceID: station.DeviceID, APIRemote: api, AccountRemote: official, CheckedAt: checked})
	}
	return matches
}

func remoteEligibility(cfg config.Config, a config.LocalAccount) remoteAPIEligibility {
	hubURL := cfg.EffectiveHubURL()
	observation := stationObservation(cfg, hubURL, cfg.DeviceID)
	api, official, _ := stationCapabilities(observation)
	result := remoteAPIEligibility{Reason: "unavailable", APIRemote: api, AccountRemote: official}
	if !cfg.RemoteHubAllowsAccount(hubURL, a) {
		result.Reason = "not_bound"
		return result
	}
	if !observation.Available() {
		return result
	}
	result.Supported = config.StationAllowsAccount(observation.Station, a)
	if result.Supported {
		result.Reason = "supported"
	} else if observation.Station.Version == 2 && a.SourceType == "reverse_proxy" {
		result.Reason = "reverse_proxy_not_allowed"
	} else if observation.Station.Version == 2 && a.SourceType == "api_key" {
		result.Reason = "external_api_not_allowed"
	} else if config.IsProtectedOfficialAccount(a) || !api {
		result.Reason = "unsupported"
	} else {
		result.Reason = "station_mismatch"
	}
	return result
}

func apiConfirmation(cfg config.Config, target string) agentAPIStatus {
	selected, selectedFound := cfg.SelectedToolAPI(target)
	applied, appliedFound := cfg.AuthorizedAppliedToolAccount(target)
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
		AccountID: func() string {
			if appliedValid {
				return APIHandle(cfg, applied.ID)
			}
			return ""
		}(),
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
