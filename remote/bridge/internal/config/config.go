// Package config stores the bridge settings in os.UserConfigDir()/SalcaraBridge/config.json.
//
// Security note: API keys are stored in plain JSON. The file is created with mode 0600 (owner read/write only)
// inside the user's own profile directory (on Windows: %AppData%\SalcaraBridge, which is already private to the
// Windows user account). Any obfuscation would not be real protection against software running as the same
// user, so we deliberately don't pretend otherwise.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"salcara/bridge/internal/atomicfile"
	"salcara/bridge/internal/protocol"
)

// Approval policies (PROTOCOL.md §5).
const (
	ApprovalAsk       = "ask"
	ApprovalAutoEdits = "auto_edits"
	ApprovalAutoAll   = "auto_all"
)

// Codex auth modes for the one-click Codex configuration.
const (
	CodexAuthToken = "token" // experimental_bearer_token in config.toml
	CodexAuthEnv   = "env"   // env_key = "SUB2API_API_KEY" (+ setx on Windows)
)

// Config is the on-disk configuration.
type Config struct {
	RelayRoot         string             `json:"relayRoot"`
	HubURL            string             `json:"hubUrl,omitempty"` // empty = RelayRoot + "/salcara-hub"
	AccountKey        string             `json:"accountKey"`
	CodexKey          string             `json:"codexKey,omitempty"`  // empty = AccountKey
	ClaudeKey         string             `json:"claudeKey,omitempty"` // empty = AccountKey
	RemoteDeviceOnly  bool               `json:"remoteDeviceOnly,omitempty"`
	RemoteConnections []RemoteConnection `json:"remoteConnections,omitempty"`
	// Exactly one phone transport may execute at a time. Saved credentials for
	// other transports remain intact; empty retains legacy mode inference.
	ActiveRemoteMode string `json:"activeRemoteMode,omitempty"`
	// One physical computer/phone authorization, independent of station credentials.
	ComputerID       string `json:"computerId,omitempty"`
	PhoneBindingID   string `json:"phoneBindingId,omitempty"`
	PhoneHash        string `json:"phoneHash,omitempty"`
	PhonePairPending string `json:"phonePairPending,omitempty"` // exact station identity allowed to finish a local QR
	PhonePairAttempt string `json:"phonePairAttempt,omitempty"`
	PhonePairExpires int64  `json:"phonePairExpires,omitempty"`
	// Direct relay state is separate from Hub pairing. The listener stays off
	// unless the user explicitly requests a direct pairing QR in the console.
	DirectRelayEnabled bool     `json:"directRelayEnabled,omitempty"`
	DirectRelayHosts   []string `json:"directRelayHosts,omitempty"`
	DirectPhoneID      string   `json:"directPhoneId,omitempty"`
	DirectPhoneHash    string   `json:"directPhoneHash,omitempty"`
	// DirectSessionKeys is a durable privacy fence for sessions that have ever
	// been opened or created over the authenticated P2P transport. A process
	// restart must not make an existing direct-only conversation eligible for
	// Hub mirroring again.
	DirectSessionKeys []string `json:"directSessionKeys,omitempty"`
	// DirectSessionFenceUnknown remains set when a P2P session start could not
	// be conclusively associated with its generated key. It makes a restart
	// fail closed rather than mirror an unknown session through Hub.
	DirectSessionFenceUnknown  bool `json:"directSessionFenceUnknown,omitempty"`
	DirectSessionStartsPending int  `json:"directSessionStartsPending,omitempty"`
	// Conditional station cleanup survives offline Hubs and a process restart.
	// Entries contain the revoked generation only, never API/device secrets.
	PendingPhoneRevokes []PendingPhoneRevoke `json:"pendingPhoneRevokes,omitempty"`
	// Last committed handover receipt contains metadata, never a key/secret.
	RemoteHandoverOperation string `json:"remoteHandoverOperation,omitempty"`
	RemoteHandoverFrom      string `json:"remoteHandoverFrom,omitempty"`
	RemoteHandoverAt        int64  `json:"remoteHandoverAt,omitempty"`
	// Hash of the non-secret station-switch payload. It makes retries with one
	// operation ID idempotent without accepting a different API/model payload.
	RemoteHandoverPayloadHash string `json:"remoteHandoverPayloadHash,omitempty"`

	DeviceID     string             `json:"deviceId"`
	DeviceSecret string             `json:"deviceSecret"`
	DeviceName   string             `json:"deviceName"`
	Projects     []protocol.Project `json:"projects"`
	// Explorer grants bind explicitly created directories to their filesystem
	// identity. They do not authorize the disk or parent used by the picker.
	ExplorerGrants []ExplorerGrant `json:"explorerGrants,omitempty"`

	Approval string `json:"approval"`
	// AllowPhoneAutoAll lets a paired phone start tasks with the "全部自动"
	// policy (no confirmation for any command). Off by default: a stolen phone
	// token must not be able to run arbitrary commands without approval.
	AllowPhoneAutoAll  bool   `json:"allowPhoneAutoAll,omitempty"`
	Autostart          bool   `json:"autostart"`
	AutostartDecided   bool   `json:"autostartDecided"` // set once autostart was enabled on first login (or the user chose)
	OpenConsoleOnStart bool   `json:"openConsoleOnStart"`
	CodexModel         string `json:"codexModel,omitempty"`
	ClaudeModel        string `json:"claudeModel,omitempty"`
	CodexAuthMode      string `json:"codexAuthMode,omitempty"`

	// Local API accounts work without a Hub login and are never sent to the Hub.
	LocalAccounts []LocalAccount                `json:"localAccounts,omitempty"`
	AccountTags   map[string]AccountTagMetadata `json:"accountTags,omitempty"`
	CodexAPI      CodexAPIService               `json:"codexApi,omitempty"`
	// CodexServiceProfile owns only the service-specific global Codex profile.
	// Its process-local authorization never shares Workbench launch ownership.
	CodexServiceProfile ServiceProfileBinding `json:"-"`
	LocalAccountsReady  bool                  `json:"localAccountsReady,omitempty"`
	ActiveCodexAccount  string                `json:"activeCodexAccount,omitempty"`
	ActiveClaudeAccount string                `json:"activeClaudeAccount,omitempty"`
	LocalToolPaths      map[string]string     `json:"localToolPaths,omitempty"`
	ToolAPISelections   map[string]string     `json:"toolApiSelections,omitempty"`
	ToolModels          map[string]string     `json:"toolModels,omitempty"`
	ToolProtocols       map[string]string     `json:"toolProtocols,omitempty"`
	// ToolCatalogOverride replaces the original tool's model picker with the
	// selected API's model catalog (Codex model_catalog_json), per tool card.
	ToolCatalogOverride map[string]bool `json:"toolCatalogOverride,omitempty"`
	GatewayKey          string          `json:"gatewayKey,omitempty"`
	// GatewayRuntimeKey is an in-memory lease generated for each Bridge
	// process. It is deliberately never written to config.json. Tool configs
	// that still contain a lease from a previous process must fail closed until
	// the user explicitly reapplies the connection from Salcara.
	GatewayRuntimeKey string            `json:"-"`
	GatewayAuthEpochs map[string]string `json:"-"`
	// UsageSourceRevision retires earlier dashboard observations on committed
	// source/credential/display changes. It is process-local and Store-owned.
	UsageSourceRevision string `json:"-"`
	// RemoteConnectionEpoch retires in-flight remote connection operations on
	// every committed identity, transport or runtime lease change. It is
	// process-local and can only be assigned by the Store.
	RemoteConnectionEpoch string `json:"-"`
	// RemoteExecutionArmed is process-local. It makes remote workers use the
	// vault snapshot instead of re-reading native tool config that another
	// process may have changed since Salcara last applied it.
	RemoteExecutionArmed       bool                  `json:"-"`
	RemoteStationAuthorization *StationAuthorization `json:"-"`
	RemoteStationObservation   *StationObservation   `json:"-"`
	ToolAPIApplied             map[string]AppliedAPI `json:"toolApiApplied,omitempty"`
	// RemoteAPI holds the API a paired phone chose for Bridge-run agents.
	RemoteAPI map[string]RemoteAPI `json:"remoteApi,omitempty"`
	// DirectAPI belongs to the pinned phone pairing, independently of Hub choices.
	DirectAPI map[string]RemoteAPI `json:"directApi,omitempty"`
}

type ExplorerGrant struct {
	Path        string `json:"path"`
	DirectoryID string `json:"directoryId"`
}

// Clone returns a deep copy.
func (c Config) Clone() Config {
	if c.AccountTags != nil {
		tags := make(map[string]AccountTagMetadata, len(c.AccountTags))
		for key, metadata := range c.AccountTags {
			metadata.Tags = append([]string(nil), metadata.Tags...)
			tags[key] = metadata
		}
		c.AccountTags = tags
	}
	c.RemoteStationObservation = CloneStationObservation(c.RemoteStationObservation)
	if c.RemoteStationAuthorization != nil {
		a := *c.RemoteStationAuthorization
		a.Station = *CloneStationPolicy(&a.Station)
		c.RemoteStationAuthorization = &a
	}
	c.CodexAPI.Network.Ports = append([]int(nil), c.CodexAPI.Network.Ports...)
	if c.GatewayAuthEpochs != nil {
		epochs := make(map[string]string, len(c.GatewayAuthEpochs))
		for target, epoch := range c.GatewayAuthEpochs {
			epochs[target] = epoch
		}
		c.GatewayAuthEpochs = epochs
	}
	c.CodexAPI.Clients = append([]NamedAPIClient(nil), c.CodexAPI.Clients...)
	for i := range c.CodexAPI.Clients {
		c.CodexAPI.Clients[i].AccountIDs = append([]string(nil), c.CodexAPI.Clients[i].AccountIDs...)
		c.CodexAPI.Clients[i].ImageAccountIDs = append([]string(nil), c.CodexAPI.Clients[i].ImageAccountIDs...)
		c.CodexAPI.Clients[i].ModelAllowlist = append([]string(nil), c.CodexAPI.Clients[i].ModelAllowlist...)
		c.CodexAPI.Clients[i].ModelDenylist = append([]string(nil), c.CodexAPI.Clients[i].ModelDenylist...)
		c.CodexAPI.Clients[i].ModelAllowPrefixes = append([]string(nil), c.CodexAPI.Clients[i].ModelAllowPrefixes...)
		c.CodexAPI.Clients[i].ModelDenyPrefixes = append([]string(nil), c.CodexAPI.Clients[i].ModelDenyPrefixes...)
		if aliases := c.CodexAPI.Clients[i].ModelAliases; aliases != nil {
			copy := make(map[string]string, len(aliases))
			for k, v := range aliases {
				copy[k] = v
			}
			c.CodexAPI.Clients[i].ModelAliases = copy
		}
	}
	c.RemoteConnections = append([]RemoteConnection(nil), c.RemoteConnections...)
	c.DirectRelayHosts = append([]string(nil), c.DirectRelayHosts...)
	c.DirectSessionKeys = append([]string(nil), c.DirectSessionKeys...)
	for i := range c.RemoteConnections {
		c.RemoteConnections[i].RemoteSupport = CloneStationObservation(c.RemoteConnections[i].RemoteSupport)
	}
	c.PendingPhoneRevokes = append([]PendingPhoneRevoke(nil), c.PendingPhoneRevokes...)
	c.Projects = append([]protocol.Project(nil), c.Projects...)
	c.ExplorerGrants = append([]ExplorerGrant(nil), c.ExplorerGrants...)
	c.LocalAccounts = append([]LocalAccount(nil), c.LocalAccounts...)
	for i := range c.LocalAccounts {
		c.LocalAccounts[i].Models = append([]string(nil), c.LocalAccounts[i].Models...)
		c.LocalAccounts[i].RemoteHubURLs = append([]string(nil), c.LocalAccounts[i].RemoteHubURLs...)
		c.LocalAccounts[i].ModelAllowlist = append([]string(nil), c.LocalAccounts[i].ModelAllowlist...)
		c.LocalAccounts[i].ModelDenylist = append([]string(nil), c.LocalAccounts[i].ModelDenylist...)
	}
	if c.LocalToolPaths != nil {
		paths := make(map[string]string, len(c.LocalToolPaths))
		for k, v := range c.LocalToolPaths {
			paths[k] = v
		}
		c.LocalToolPaths = paths
	}
	if c.ToolAPISelections != nil {
		selected := make(map[string]string, len(c.ToolAPISelections))
		for k, v := range c.ToolAPISelections {
			selected[k] = v
		}
		c.ToolAPISelections = selected
	}
	if c.ToolAPIApplied != nil {
		applied := make(map[string]AppliedAPI, len(c.ToolAPIApplied))
		for k, v := range c.ToolAPIApplied {
			v.ManagedModels = append([]string(nil), v.ManagedModels...)
			applied[k] = v
		}
		c.ToolAPIApplied = applied
	}
	if c.RemoteAPI != nil {
		remote := make(map[string]RemoteAPI, len(c.RemoteAPI))
		for k, v := range c.RemoteAPI {
			remote[k] = v
		}
		c.RemoteAPI = remote
	}
	if c.DirectAPI != nil {
		direct := make(map[string]RemoteAPI, len(c.DirectAPI))
		for k, v := range c.DirectAPI {
			direct[k] = v
		}
		c.DirectAPI = direct
	}
	if c.ToolModels != nil {
		models := map[string]string{}
		for k, v := range c.ToolModels {
			models[k] = v
		}
		c.ToolModels = models
	}
	if c.ToolCatalogOverride != nil {
		override := make(map[string]bool, len(c.ToolCatalogOverride))
		for k, v := range c.ToolCatalogOverride {
			override[k] = v
		}
		c.ToolCatalogOverride = override
	}
	if c.ToolProtocols != nil {
		protocols := map[string]string{}
		for k, v := range c.ToolProtocols {
			protocols[k] = v
		}
		c.ToolProtocols = protocols
	}
	return c
}

// EffectiveHubURL is the hub base (without /v1).
func (c Config) EffectiveHubURL() string {
	if h := strings.TrimRight(strings.TrimSpace(c.HubURL), "/"); h != "" {
		return h
	}
	if c.RelayRoot == "" {
		return ""
	}
	return strings.TrimRight(c.RelayRoot, "/") + "/salcara-hub"
}

func (c Config) EffectiveCodexKey() string {
	if c.CodexKey != "" {
		return c.CodexKey
	}
	return c.AccountKey
}

func (c Config) EffectiveClaudeKey() string {
	if c.ClaudeKey != "" {
		return c.ClaudeKey
	}
	return c.AccountKey
}

// GatewayCredential returns the current process lease when the Bridge is
// running, falling back to the legacy persisted key for library/test callers
// that construct a Config without starting the application.
func (c Config) GatewayCredential() string {
	if strings.TrimSpace(c.GatewayRuntimeKey) != "" {
		return c.GatewayRuntimeKey
	}
	return c.GatewayKey
}

// LoggedIn reports whether relay address and account key are set.
func (c Config) LoggedIn() bool {
	return c.RelayRoot != "" && (c.RemoteDeviceOnly || c.AccountKey != "")
}

// NormalizeRelayRoot trims spaces, trailing slashes and a trailing /v1 ("https://x.com/v1/" → "https://x.com").
func NormalizeRelayRoot(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, "/")
	if strings.HasSuffix(strings.ToLower(s), "/v1") {
		s = strings.TrimRight(s[:len(s)-3], "/")
	}
	if s != "" && !strings.Contains(s, "://") {
		s = "https://" + s
	}
	return s
}

// ValidApproval reports whether p is a known policy.
func ValidApproval(p string) bool {
	return p == ApprovalAsk || p == ApprovalAutoEdits || p == ApprovalAutoAll
}

// Dir returns the config directory (os.UserConfigDir()/SalcaraBridge).
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "SalcaraBridge"), nil
}

// Store is a concurrency-safe handle on the config file.
type Store struct {
	path                       string
	mu                         sync.RWMutex
	cfg                        Config
	subs                       []func(old, new Config)
	stationObservationSequence uint64
	stationObservationLatest   map[string]uint64
	phoneRevocationPending     bool
	savedImagesMode            string
	savedImagesConcurrency     int
}

// Open loads (or creates) the config file at path. An empty path means Dir()/config.json.
func Open(path string) (*Store, error) {
	if path == "" {
		dir, err := Dir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(dir, "config.json")
	}
	s := &Store{path: path}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &s.cfg); err != nil {
			// Keep the broken file for the user and start fresh rather than refusing to run.
			_ = os.Rename(path, path+".broken")
			s.cfg = Config{}
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return nil, err
	}
	changed := s.fillDefaults()
	fenceCodexAPIAuthorization(Config{}, &s.cfg)
	fenceGatewayAuthorization(Config{}, &s.cfg)
	fenceUsageSourceRevision(Config{}, &s.cfg)
	fenceRemoteConnectionEpoch(Config{}, &s.cfg)
	if changed {
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
	}
	s.savedImagesMode, s.savedImagesConcurrency = s.cfg.CodexAPI.ImagesMode, s.cfg.CodexAPI.MaxConcurrentImageRequests
	return s, nil
}

func (s *Store) fillDefaults() bool {
	changed := false
	if s.cfg.DeviceID == "" {
		s.cfg.DeviceID = NewUUID()
		changed = true
	}
	if s.cfg.DeviceSecret == "" {
		s.cfg.DeviceSecret = RandomToken(32)
		changed = true
	}
	if s.cfg.GatewayKey == "" {
		s.cfg.GatewayKey = "sk-salcara-local-" + RandomToken(32)
		changed = true
	}
	if s.cfg.DeviceName == "" {
		h, _ := os.Hostname()
		if h == "" {
			h = "我的电脑"
		}
		s.cfg.DeviceName = h
		changed = true
	}
	if !ValidApproval(s.cfg.Approval) {
		s.cfg.Approval = ApprovalAsk
		changed = true
	}
	if s.cfg.CodexAuthMode == "" {
		s.cfg.CodexAuthMode = CodexAuthToken
		changed = true
	}
	if s.cfg.Projects == nil {
		s.cfg.Projects = []protocol.Project{}
		changed = true
	}
	if !s.cfg.LocalAccountsReady {
		// Migrate existing tool credentials once; keep remote credentials intact.
		if s.cfg.LoggedIn() && len(s.cfg.LocalAccounts) == 0 {
			for _, kind := range []string{"codex", "claude"} {
				key, model, name := s.cfg.EffectiveCodexKey(), s.cfg.CodexModel, "原 Codex API"
				if kind == "claude" {
					key, model, name = s.cfg.EffectiveClaudeKey(), s.cfg.ClaudeModel, "原 Claude API"
				}
				a := LocalAccount{ID: NewUUID(), Name: name, Kind: kind, BaseURL: s.cfg.RelayRoot, Key: key, Model: model, AuthMode: "bearer"}
				s.cfg.LocalAccounts = append(s.cfg.LocalAccounts, a)
				if kind == "codex" {
					s.cfg.ActiveCodexAccount = a.ID
				} else {
					s.cfg.ActiveClaudeAccount = a.ID
				}
			}
		}
		s.cfg.LocalAccountsReady = true
		changed = true
	}
	return changed
}

// Path is the config file location.
func (s *Store) Path() string { return s.path }

// Get returns a copy of the current config.
func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.Clone()
}

// SetRuntimeGatewayKey arms a new process-local gateway lease. The value is
// intentionally not persisted and is never sent to the Hub. It invalidates
// loopback credentials written by a previous Bridge process.
func (s *Store) SetRuntimeGatewayKey(key string) {
	s.mu.Lock()
	old := s.cfg.Clone()
	s.cfg.GatewayRuntimeKey = strings.TrimSpace(key)
	for target, binding := range s.cfg.ToolAPIApplied {
		binding.LaunchLease = ""
		s.cfg.ToolAPIApplied[target] = binding
	}
	s.cfg.RemoteExecutionArmed = s.cfg.GatewayRuntimeKey != ""
	s.cfg.RemoteStationAuthorization = nil
	s.cfg.RemoteStationObservation = nil
	s.stationObservationLatest = nil
	for i := range s.cfg.RemoteConnections {
		s.cfg.RemoteConnections[i].RemoteSupport = nil
	}
	// The Codex API service is independently exposed on loopback.  A
	// persisted Enabled flag must not survive a process restart as an active
	// listener: bind it to the fresh process lease and require the console to
	// explicitly enable it again.
	s.cfg.CodexAPI.RuntimeLease = ""
	s.cfg.CodexAPI.AuthEpoch = RandomToken(32)
	s.cfg.CodexServiceProfile = ServiceProfileBinding{}
	fenceGatewayAuthorization(old, &s.cfg)
	fenceRemoteConnectionEpoch(old, &s.cfg)
	s.mu.Unlock()
}

// RevokeAppliedLaunchLease retires only the caller's process-local connection.
// It must work even when the configuration file cannot be written. Runtime
// leases are never persisted; a newer apply is unaffected by an older failure.
func (s *Store) RevokeAppliedLaunchLease(target, lease string) {
	s.mu.Lock()
	target = ToolFamily(target)
	binding, ok := s.cfg.ToolAPIApplied[target]
	if !ok || lease == "" || binding.LaunchLease != lease {
		s.mu.Unlock()
		return
	}
	old := s.cfg.Clone()
	binding.LaunchLease = ""
	s.cfg.ToolAPIApplied[target] = binding
	fenceGatewayAuthorization(old, &s.cfg)
	cur := s.cfg.Clone()
	subs := append([]func(old, new Config){}, s.subs...)
	s.mu.Unlock()
	for _, f := range subs {
		f(old, cur)
	}
}

// Update applies fn to a copy, saves it and notifies subscribers. If fn returns an error nothing changes.
func (s *Store) Update(fn func(c *Config) error) error {
	_, err := s.updateWithSnapshot(fn, false)
	return err
}

// UpdateWithSnapshot returns the exact committed configuration, including
// Store-owned authorization epochs. A caller must not adopt a later Get as
// its own write: another update can revoke or replace a client in between.
func (s *Store) UpdateWithSnapshot(fn func(c *Config) error) (Config, error) {
	return s.updateWithSnapshot(fn, true)
}

func (s *Store) updateWithSnapshot(fn func(c *Config) error, capture bool) (Config, error) {
	s.mu.Lock()
	old := s.cfg.Clone()
	next := s.cfg.Clone()
	if err := fn(&next); err != nil {
		s.mu.Unlock()
		return Config{}, err
	}
	s.cfg = next
	s.fillDefaults()
	s.cfg.retireInvalidAppliedLaunches()
	fenceCodexAPIAuthorization(old, &s.cfg)
	s.cfg.retireInvalidManagedAPILaunches()
	fenceGatewayAuthorization(old, &s.cfg)
	fenceUsageSourceRevision(old, &s.cfg)
	fenceRemoteConnectionEpoch(old, &s.cfg)
	if err := s.saveLocked(); err != nil {
		s.cfg = old
		s.mu.Unlock()
		return Config{}, err
	}
	if old.RemoteConnectionEpoch != s.cfg.RemoteConnectionEpoch {
		// A connection can move A -> B -> A before an old probe completes.
		// Retire its ticket on every successful connection transition.
		s.stationObservationLatest = nil
	}
	cur := s.cfg.Clone()
	var committed Config
	if capture {
		committed = cur.Clone()
	}
	subs := append([]func(old, new Config){}, s.subs...)
	s.mu.Unlock()
	for _, f := range subs {
		f(old, cur)
	}
	return committed, nil
}

// OnChange registers fn to be called after every successful Update.
func (s *Store) OnChange(fn func(old, new Config)) {
	s.mu.Lock()
	s.subs = append(s.subs, fn)
	s.mu.Unlock()
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicfile.WriteFile(s.path, b, 0o600); err != nil {
		return fmt.Errorf("保存配置失败: %w", err)
	}
	s.phoneRevocationPending = false
	s.savedImagesMode, s.savedImagesConcurrency = s.cfg.CodexAPI.ImagesMode, s.cfg.CodexAPI.MaxConcurrentImageRequests
	return nil
}

// NewUUID returns a random RFC 4122 version 4 UUID.
func NewUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// RandomToken returns n random bytes hex-encoded.
func RandomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// ProjectName derives a display name from a folder path (last element, handling both separators).
func ProjectName(p string) string {
	t := strings.TrimRight(p, `/\`)
	if i := strings.LastIndexAny(t, `/\`); i >= 0 {
		t = t[i+1:]
	}
	if t == "" {
		return p // "/" or "C:\"
	}
	return t
}
