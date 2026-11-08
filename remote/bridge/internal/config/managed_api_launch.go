package config

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strings"
)

// ManagedAPIClient resolves only an explicitly launched, current local service.
// This is not a LocalAccount: pooling must never masquerade as one remote source.
func (c Config) ManagedAPIClient(target string) (NamedAPIClient, bool) {
	if target != "codex" && target != "codex-desktop" && target != "claude" && target != "claude-desktop" {
		return NamedAPIClient{}, false
	}
	b, ok := c.ToolAPIApplied[ToolFamily(target)]
	row, found := c.APIClient(b.LocalClientID)
	return row, ok && found && b.ManagedBy == "salcara-local-api" && b.LaunchLease != "" &&
		c.GatewayRuntimeKey != "" && c.CodexAPIActive() && !row.Disabled && ValidateAPIClient(row) == nil &&
		b.ClientEpoch != "" && b.ClientEpoch == row.AuthEpoch && b.ServiceEpoch == c.CodexAPI.AuthEpoch
}

func (c Config) ManagedAPICredential(target string) string {
	row, ok := c.ManagedAPIClient(target)
	if !ok {
		return ""
	}
	b := c.ToolAPIApplied[ToolFamily(target)]
	mac := hmac.New(sha256.New, []byte(c.GatewayRuntimeKey))
	_, _ = mac.Write([]byte(strings.Join([]string{"salcara-managed-api-v1", target, row.ID, row.AuthEpoch, b.ServiceEpoch, b.LaunchLease}, "\x00")))
	return "ml-salcara-" + hex.EncodeToString(mac.Sum(nil))
}

func (c Config) ResolveManagedAPIClient(target, key string) (NamedAPIClient, bool) {
	expected := c.ManagedAPICredential(target)
	if expected == "" || subtle.ConstantTimeCompare([]byte(key), []byte(expected)) != 1 {
		return NamedAPIClient{}, false
	}
	return c.ManagedAPIClient(target)
}

func (c *Config) RecordManagedAPI(target string, client NamedAPIClient, lease string, catalog bool, models []string) {
	if c.ToolAPIApplied == nil {
		c.ToolAPIApplied = map[string]AppliedAPI{}
	}
	c.ToolAPIApplied[ToolFamily(target)] = AppliedAPI{ManagedBy: "salcara-local-api", LocalClientID: client.ID,
		ClientEpoch: client.AuthEpoch, ServiceEpoch: c.CodexAPI.AuthEpoch, LaunchLease: lease, Catalog: catalog, ManagedModels: append([]string(nil), models...)}
}

func (c *Config) retireInvalidManagedAPILaunches() {
	for target, b := range c.ToolAPIApplied {
		if b.ManagedBy != "salcara-local-api" || b.LaunchLease == "" {
			continue
		}
		if _, ok := c.ManagedAPIClient(target); !ok {
			b.LaunchLease = ""
			c.ToolAPIApplied[target] = b
		}
	}
}
