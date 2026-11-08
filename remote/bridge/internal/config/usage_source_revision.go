package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strings"
)

// UsageSourceTarget is a provider that supports the dashboard's API-key usage
// endpoint. Execution credentials and provider URLs are never DTO fields.
// Official-account usage has a separate, explicitly requested protected probe.
type UsageSourceTarget struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Host string `json:"host"`
	Kind string `json:"kind"`
	Root string `json:"-"`
	Key  string `json:"-"`
}

// UsageSourceTargets defines both the dashboard query projection and the Store
// revision boundary. Catalog, model policy and unrelated settings cannot change
// this projection. Protected credential references never supply API keys.
func (c Config) UsageSourceTargets() []UsageSourceTarget {
	var targets []UsageSourceTarget
	type credential struct{ root, key string }
	seen := make(map[credential]bool)
	add := func(t UsageSourceTarget) {
		id := credential{t.Root, t.Key}
		if t.Root == "" || t.Key == "" || seen[id] {
			return
		}
		seen[id] = true
		targets = append(targets, t)
	}
	if c.LoggedIn() && !c.RemoteDeviceOnly {
		add(UsageSourceTarget{ID: "relay", Name: "中转站", Host: usageSourceHost(c.RelayRoot), Kind: "relay", Root: c.RelayRoot, Key: c.AccountKey})
	}
	for _, a := range c.LocalAccounts {
		if a.CredentialRef != "" {
			continue
		}
		switch a.SourceType {
		case "", "api_key", "reverse_proxy":
		default: // OAuth/official/unknown source types are not API-key probes.
			continue
		}
		root := NormalizeRelayRoot(a.BaseURL)
		name := strings.TrimSpace(a.Name)
		if name == "" {
			name = usageSourceHost(root)
		}
		add(UsageSourceTarget{ID: a.ID, Name: name, Host: usageSourceHost(root), Kind: "api", Root: root, Key: strings.TrimSpace(a.Key)})
	}
	return targets
}

func usageSourceHost(root string) string {
	if u, err := url.Parse(root); err == nil && u.Host != "" {
		return u.Host
	}
	return root
}

// UsageSourceFingerprint identifies current query destinations, credentials and
// displayed source metadata. It is private state, not an authorization token or
// a public usage field. Length-delimited JSON avoids ambiguous separators.
func (c Config) UsageSourceFingerprint() string {
	targets := c.UsageSourceTargets()
	rows := make([][6]string, 0, len(targets))
	for _, t := range targets {
		rows = append(rows, [6]string{t.ID, t.Name, t.Host, t.Kind, t.Root, APIClientDigest(t.Key)})
	}
	// A selected account is not deduplicated by its credentials. Include every
	// saved source, including protected references, in the lock-bound fence so
	// replacing/deleting a duplicate or an official identity cannot survive ABA.
	// Keep this separate from UsageSourceTargets: the legacy overview still
	// probes each shared API credential once and never probes OAuth credentials.
	type accountSource struct {
		ID, Name, Kind, SourceType, ProviderID, Root, KeyDigest, CredentialRef string
		CredentialEpoch                                                        uint64
	}
	accounts := make([]accountSource, 0, len(c.LocalAccounts))
	for _, a := range c.LocalAccounts {
		accounts = append(accounts, accountSource{a.ID, a.Name, a.Kind, a.SourceType, a.ProviderID, NormalizeRelayRoot(a.BaseURL), APIClientDigest(strings.TrimSpace(a.Key)), a.CredentialRef, a.CredentialEpoch})
	}
	raw, _ := json.Marshal(struct {
		Targets  [][6]string
		Accounts []accountSource
	}{rows, accounts})
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

// Called while Store.mu is held, before publishing a successful update. An ABA
// restoration must retire earlier observations even if OnChange is delayed.
// Update's persistence rollback also restores this process-only revision.
func fenceUsageSourceRevision(old Config, next *Config) {
	next.UsageSourceRevision = old.UsageSourceRevision
	if next.UsageSourceRevision == "" || old.UsageSourceFingerprint() != next.UsageSourceFingerprint() {
		next.UsageSourceRevision = RandomToken(32)
	}
}
