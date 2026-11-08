package config

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"reflect"
	"regexp"
	"strings"
)

// NamedAPIClient stores only a digest of a random client key. AccountIDs is an
// explicit text-only pool; an empty list preserves the legacy fixed AccountID.
type NamedAPIClient struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	AccountID string `json:"accountId"`
	// AccountIDs never inherits the local account-pool membership. Every member
	// is an explicit authorization for this client; AccountID is not implicitly
	// included when this list is non-empty.
	AccountIDs    []string `json:"accountIds,omitempty"`
	PoolStrategy  string   `json:"poolStrategy,omitempty"`
	PoolAccountID string   `json:"poolAccountId,omitempty"`
	// ImageAccountIDs is an explicit image-only member set. Empty keeps the
	// client's single AccountID source; it never opts text or remote routes into
	// an account pool.
	ImageAccountIDs []string `json:"imageAccountIds,omitempty"`
	KeyDigest       string   `json:"keyDigest"`
	Disabled        bool     `json:"disabled,omitempty"`
	// Permissions apply to upstream model IDs after alias resolution. An empty
	// allowlist keeps existing clients compatible; a deny rule always wins.
	ModelAllowlist []string `json:"modelAllowlist,omitempty"`
	ModelDenylist  []string `json:"modelDenylist,omitempty"`
	// Prefix rules are literal, case-sensitive starts-with checks on the resolved
	// upstream ID. Exact and prefix allow rules form a union; either deny wins.
	ModelAllowPrefixes []string          `json:"modelAllowPrefixes,omitempty"`
	ModelDenyPrefixes  []string          `json:"modelDenyPrefixes,omitempty"`
	ModelAliases       map[string]string `json:"modelAliases,omitempty"`
	// ModelPrefix is an optional client-visible namespace such as "team/".
	// Requests must present it exactly; aliases and ACLs are evaluated only
	// after the prefix is removed.
	ModelPrefix    string `json:"modelPrefix,omitempty"`
	MaxConcurrency int    `json:"maxConcurrency,omitempty"`
	// SystemPromptPrefix is prepended to every request's system prompt for
	// requests made with this API client key. Applied before the per-account
	// prefix. Use for client-level channel instructions or safety guardrails.
	SystemPromptPrefix string `json:"systemPromptPrefix,omitempty"`
	// MaxRPM limits requests per minute for this client. 0 = unlimited.
	MaxRPM int `json:"maxRpm,omitempty"`
	// MaxTPM limits tokens per minute for this client. 0 = unlimited.
	MaxTPM int `json:"maxTpm,omitempty"`
	// AuthEpoch is assigned by Store at each authorization change. It prevents
	// restoring the same client settings from reviving an older queued request.
	AuthEpoch string `json:"-"`
}
type CodexAPIService struct {
	Network                    CodexAPINetwork  `json:"network,omitempty"`
	Enabled                    bool             `json:"enabled"`
	Clients                    []NamedAPIClient `json:"clients,omitempty"`
	ImagesMode                 string           `json:"imagesMode,omitempty"`
	MaxConcurrentImageRequests int              `json:"maxConcurrentImageRequests,omitempty"`
	// RuntimeLease is deliberately process-local. A persisted Enabled flag
	// describes the user's last choice, but it is not enough to expose the
	// loopback API after a Bridge restart. The console must explicitly arm the
	// service for the current process lease first.
	RuntimeLease string `json:"-"`
	AuthEpoch    string `json:"-"`
}

// fenceCodexAPIAuthorization runs under the Store lock before publication. Epochs
// belong to this process, not to persisted settings or values supplied by an
// Update callback. New random values also fence removing and recreating an ID.
func fenceCodexAPIAuthorization(old Config, next *Config) {
	next.CodexAPI.AuthEpoch = old.CodexAPI.AuthEpoch
	if next.CodexAPI.AuthEpoch == "" || old.CodexAPI.Enabled != next.CodexAPI.Enabled || old.CodexAPI.RuntimeLease != next.CodexAPI.RuntimeLease || old.CodexAPI.ImagesMode != next.CodexAPI.ImagesMode || old.CodexAPI.MaxConcurrentImageRequests != next.CodexAPI.MaxConcurrentImageRequests || !reflect.DeepEqual(old.CodexAPI.Network, next.CodexAPI.Network) || old.GatewayRuntimeKey != next.GatewayRuntimeKey {
		next.CodexAPI.AuthEpoch = RandomToken(32)
	}
	for i := range next.CodexAPI.Clients {
		client := &next.CodexAPI.Clients[i]
		prior, exists := old.APIClient(client.ID)
		client.AuthEpoch = prior.AuthEpoch
		before, after := prior, *client
		before.AuthEpoch, after.AuthEpoch = "", ""
		accountChanged := !reflect.DeepEqual(apiClientAccountFingerprints(old, prior), apiClientAccountFingerprints(*next, *client))
		if !exists || client.AuthEpoch == "" || !reflect.DeepEqual(before, after) || accountChanged {
			client.AuthEpoch = RandomToken(32)
		}
	}
	// Only the runtime arm/revoke methods own this binding. An Update callback
	// must not revive a profile from an earlier configuration snapshot.
	next.CodexServiceProfile = old.CodexServiceProfile
	if _, ok := next.CodexServiceProfileClient(); !ok {
		next.CodexServiceProfile = ServiceProfileBinding{}
	}
}

func apiClientAccountFingerprints(c Config, client NamedAPIClient) []string {
	ids := make([]string, 0, 1+len(client.AccountIDs)+len(client.ImageAccountIDs))
	ids = append(ids, client.AccountID)
	ids = append(ids, client.AccountIDs...)
	ids = append(ids, client.ImageAccountIDs...)
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		account, present := c.LocalAccount(id)
		marker := id + "\x00missing"
		if present {
			marker = id + "\x00" + AccountFingerprint(account)
		}
		out = append(out, marker)
	}
	return out
}

// CodexAPIActive reports whether the local API is actually serving requests.
// Configs constructed by library callers/tests without a runtime lease retain
// the explicit legacy Enabled behavior; the production Bridge always arms a
// fresh GatewayRuntimeKey at startup, so a persisted Enabled flag is inert
// until the user enables the service again in that process.
func (c Config) CodexAPIActive() bool {
	if !c.CodexAPI.Enabled || ValidateCodexAPINetwork(c.CodexAPI.Network) != nil {
		return false
	}
	if strings.TrimSpace(c.GatewayRuntimeKey) == "" {
		return true
	}
	lease := strings.TrimSpace(c.CodexAPI.RuntimeLease)
	return lease != "" && subtle.ConstantTimeCompare([]byte(lease), []byte(c.GatewayRuntimeKey)) == 1
}

func APIClientDigest(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

var apiClientModelPrefix = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}/$`)

func ValidAPIClientModelPrefix(prefix string) bool {
	return prefix == "" || apiClientModelPrefix.MatchString(prefix)
}

func ValidateAPIClient(c NamedAPIClient) error {
	if !ValidAccountID(c.ID) || !ValidAccountID(c.AccountID) || strings.TrimSpace(c.Name) == "" || len([]rune(c.Name)) > 80 || strings.ContainsAny(c.Name, "\r\n\x00") || len(c.KeyDigest) != 64 || c.MaxConcurrency < 0 || c.MaxConcurrency > 128 || len(c.ModelAliases) > 1000 || len(c.AccountIDs) > 64 || len(c.ImageAccountIDs) > 64 || !ValidAPIClientModelPrefix(c.ModelPrefix) {
		return errors.New("API 客户端配置无效")
	}
	textAccounts := make(map[string]struct{}, len(c.AccountIDs))
	for _, id := range c.AccountIDs {
		if !ValidAccountID(id) || strings.TrimSpace(id) != id {
			return errors.New("文本账号池配置无效")
		}
		if _, duplicate := textAccounts[id]; duplicate {
			return errors.New("文本账号池配置无效")
		}
		textAccounts[id] = struct{}{}
	}
	strategy := strings.TrimSpace(c.PoolStrategy)
	if len(c.AccountIDs) == 0 {
		if strategy != "" || c.PoolAccountID != "" {
			return errors.New("文本账号池配置无效")
		}
	} else {
		if strategy != c.PoolStrategy || !ValidPoolStrategy(strategy) {
			return errors.New("文本账号池策略无效")
		}
		if strategy == PoolStrategySingleAccount {
			if _, present := textAccounts[c.PoolAccountID]; !present {
				return errors.New("文本账号池固定账号无效")
			}
		} else if c.PoolAccountID != "" {
			return errors.New("文本账号池固定账号无效")
		}
	}
	imageAccounts := make(map[string]struct{}, len(c.ImageAccountIDs))
	for _, id := range c.ImageAccountIDs {
		if !ValidAccountID(id) || strings.TrimSpace(id) != id {
			return errors.New("图片账号配置无效")
		}
		if _, duplicate := imageAccounts[id]; duplicate {
			return errors.New("图片账号配置无效")
		}
		imageAccounts[id] = struct{}{}
	}
	if _, e := hex.DecodeString(c.KeyDigest); e != nil {
		return errors.New("API 客户端配置无效")
	}
	for alias, model := range c.ModelAliases {
		if !ValidCatalogModel(alias) || !ValidCatalogModel(model) || c.ModelPrefix != "" && strings.HasPrefix(alias, c.ModelPrefix) {
			return errors.New("模型别名无效")
		}
	}
	for _, models := range [][]string{c.ModelAllowlist, c.ModelDenylist, c.ModelAllowPrefixes, c.ModelDenyPrefixes} {
		if len(models) > 1000 {
			return errors.New("客户端模型权限无效")
		}
		seen := make(map[string]bool, len(models))
		for _, model := range models {
			if !ValidCatalogModel(model) || strings.TrimSpace(model) != model || seen[model] {
				return errors.New("客户端模型权限无效")
			}
			seen[model] = true
		}
	}
	for _, prefixes := range [][]string{c.ModelAllowPrefixes, c.ModelDenyPrefixes} {
		for _, prefix := range prefixes {
			if strings.ContainsAny(prefix, "*?[]") {
				return errors.New("模型前缀必须为文字，不能使用通配符")
			}
		}
	}
	return nil
}

// ResolveAPIClientModel maps one client-visible name to its upstream model.
// The namespace is exact, an alias is resolved at most once, and authorization
// is always checked against the resulting upstream ID.
func ResolveAPIClientModel(c NamedAPIClient, visible string) (string, bool) {
	if !ValidCatalogModel(visible) || !ValidAPIClientModelPrefix(c.ModelPrefix) {
		return "", false
	}
	name := visible
	if c.ModelPrefix != "" {
		if !strings.HasPrefix(name, c.ModelPrefix) {
			return "", false
		}
		name = strings.TrimPrefix(name, c.ModelPrefix)
		if !ValidCatalogModel(name) || strings.HasPrefix(name, c.ModelPrefix) {
			return "", false
		}
	}
	actual := name
	if target, ok := c.ModelAliases[name]; ok {
		actual = target
	}
	if !APIClientModelAllowed(c, actual) {
		return "", false
	}
	return actual, true
}

// APIClientVisibleModel applies the configured namespace to one explicit
// public name. Callers pass either a native model ID or a chosen alias key;
// this helper never selects an alias from a map.
func APIClientVisibleModel(c NamedAPIClient, publicName string) (string, bool) {
	if !ValidCatalogModel(publicName) || !ValidAPIClientModelPrefix(c.ModelPrefix) {
		return "", false
	}
	if c.ModelPrefix != "" && strings.HasPrefix(publicName, c.ModelPrefix) {
		return "", false
	}
	visible := c.ModelPrefix + publicName
	if !ValidCatalogModel(visible) {
		return "", false
	}
	return visible, true
}

// APIClientModelAllowed is shared by discovery and forwarding. Aliases are
// resolved by the caller so a visible alias cannot bypass its target policy.
func APIClientModelAllowed(c NamedAPIClient, model string) bool {
	if c.Disabled || !ValidCatalogModel(model) {
		return false
	}
	for _, denied := range c.ModelDenylist {
		if model == denied {
			return false
		}
	}
	for _, prefix := range c.ModelDenyPrefixes {
		if prefix != "" && strings.HasPrefix(model, prefix) {
			return false
		}
	}
	if len(c.ModelAllowlist) == 0 && len(c.ModelAllowPrefixes) == 0 {
		return true
	}
	for _, allowed := range c.ModelAllowlist {
		if model == allowed {
			return true
		}
	}
	for _, prefix := range c.ModelAllowPrefixes {
		if prefix != "" && strings.HasPrefix(model, prefix) {
			return true
		}
	}
	return false
}
func (c Config) APIClient(id string) (NamedAPIClient, bool) {
	for _, v := range c.CodexAPI.Clients {
		if v.ID == id {
			return v, true
		}
	}
	return NamedAPIClient{}, false
}

// RetireCodexAPIServiceRuntime immediately revokes the captured process-local
// service authorization without depending on a configuration-file write. The
// returned epoch identifies this exact retirement so a later persistence step
// cannot disable a newer explicit start. Runtime retirement is intentionally
// not persisted here; callers may save the already-inert state afterwards.
func (s *Store) RetireCodexAPIServiceRuntime(expectedEpoch string) (string, bool) {
	s.mu.Lock()
	if expectedEpoch == "" || s.cfg.CodexAPI.AuthEpoch != expectedEpoch {
		s.mu.Unlock()
		return "", false
	}
	old := s.cfg.Clone()
	s.cfg.CodexAPI.Enabled = false
	s.cfg.CodexAPI.RuntimeLease = ""
	fenceCodexAPIAuthorization(old, &s.cfg)
	retiredEpoch := s.cfg.CodexAPI.AuthEpoch
	cur := s.cfg.Clone()
	subs := append([]func(old, new Config){}, s.subs...)
	s.mu.Unlock()
	for _, f := range subs {
		f(old, cur)
	}
	return retiredEpoch, true
}

// RetireCodexAPIClientRuntime immediately disables one exact client snapshot.
// Comparing the whole Store-owned snapshot, including AuthEpoch, prevents an
// older revoke request from disabling a client that was concurrently rotated,
// rebound, or explicitly re-enabled. The returned disabled snapshot can be
// used to guard a subsequent persisted update or deletion.
func (s *Store) RetireCodexAPIClientRuntime(expected NamedAPIClient) (NamedAPIClient, bool) {
	s.mu.Lock()
	if expected.AuthEpoch == "" {
		s.mu.Unlock()
		return NamedAPIClient{}, false
	}
	index := -1
	for i := range s.cfg.CodexAPI.Clients {
		if s.cfg.CodexAPI.Clients[i].ID == expected.ID {
			index = i
			break
		}
	}
	if index < 0 || !reflect.DeepEqual(s.cfg.CodexAPI.Clients[index], expected) {
		s.mu.Unlock()
		return NamedAPIClient{}, false
	}
	old := s.cfg.Clone()
	s.cfg.CodexAPI.Clients[index].Disabled = true
	fenceCodexAPIAuthorization(old, &s.cfg)
	retired := s.cfg.CodexAPI.Clients[index]
	cur := s.cfg.Clone()
	subs := append([]func(old, new Config){}, s.subs...)
	s.mu.Unlock()
	for _, f := range subs {
		f(old, cur)
	}
	return retired, true
}
