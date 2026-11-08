package config

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// AccountSourceRevision is an opaque, process-bound public revision. The raw
// fingerprint is an internal consistency check and must not become a public
// unsalted credential hash. A Bridge restart invalidates outstanding proofs.
func (c Config) AccountSourceRevision(a LocalAccount) string {
	if c.GatewayRuntimeKey == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(c.GatewayRuntimeKey))
	bindings := "legacy"
	if a.RemoteHubBindingsSet {
		hubs := make([]string, 0, len(a.RemoteHubURLs))
		for _, hub := range a.RemoteHubURLs {
			if hub = NormalizeHubURL(hub); hub != "" {
				hubs = append(hubs, hub)
			}
		}
		sort.Strings(hubs)
		bindings = "explicit:" + strings.Join(hubs, "\x1f")
	}
	_, _ = mac.Write([]byte(strings.Join([]string{"salcara-source-revision-v1", a.ID, AccountFingerprint(a), bindings}, "\x00")))
	return "src-v1-" + hex.EncodeToString(mac.Sum(nil))
}

// RemoteGatewayCredential binds the worker's immutable config to its route,
// effective account, vault revision, phone/station identity and authorization
// revision. Restoring a source must not revive a retired worker's credential.
func (c Config) RemoteGatewayCredential(target string) string {
	family, remote := strings.CutPrefix(target, "remote-")
	if !remote {
		return c.GatewayKey
	}
	if !c.HubRemoteEnabled() {
		return ""
	}
	if !c.RemoteExecutionArmed {
		return c.GatewayCredential()
	}
	if c.GatewayRuntimeKey == "" {
		return ""
	}
	a, ok := c.RemoteToolAccount(family)
	if !ok {
		a, ok = c.AuthorizedAppliedToolAccount(family)
	}
	if !ok {
		return ""
	}
	vault, ok := c.LocalAccount(a.ID)
	if !ok || !c.RemoteStationAllows(vault) {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(c.GatewayRuntimeKey))
	_, _ = mac.Write([]byte(strings.Join([]string{"salcara-remote-route-v3", target, c.GatewayAuthorizationRevision(target), a.ID, AccountFingerprint(vault), AccountFingerprint(a), RemoteIdentity(c), c.PhoneBindingID, c.PhoneHash, c.remoteStationFingerprint()}, "\x00")))
	return "rt-salcara-route-" + hex.EncodeToString(mac.Sum(nil))
}

// LocalGatewayCredential accepts only the current explicit application of a
// concrete source to this route. A profile cannot follow a later account switch
// or reuse another tool's credential, even within the same Bridge process.
func (c Config) LocalGatewayCredential(target string) string {
	if c.GatewayRuntimeKey == "" {
		return c.GatewayCredential()
	}
	a, ok := c.AuthorizedAppliedToolAccount(target)
	if !ok {
		return ""
	}
	return c.localGatewayCredential(a, target)
}

func (c Config) localGatewayCredential(a LocalAccount, target string) string {
	if c.GatewayRuntimeKey == "" {
		return c.GatewayCredential()
	}
	if a.GatewayLaunchLease == "" {
		return ""
	}
	vault, ok := c.LocalAccount(a.ID)
	if !ok {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(c.GatewayRuntimeKey))
	_, _ = mac.Write([]byte(strings.Join([]string{"salcara-local-route-v1", ToolFamily(target), a.ID, AccountFingerprint(vault), AccountFingerprint(a), a.GatewayLaunchLease}, "\x00")))
	return "lt-salcara-route-" + hex.EncodeToString(mac.Sum(nil))
}

func NativeProtocol(kind string) string {
	if IsAdditionalTool(kind) {
		return "chat"
	}
	if kind == "codex" {
		return "responses"
	}
	return "anthropic"
}

func NeedsAdapter(a LocalAccount) bool {
	return a.Protocol != "" && a.Protocol != NativeProtocol(a.Kind)
}

// MixedModels reports whether any model this API offers speaks a protocol the
// tool cannot use directly (e.g. Grok or Claude models behind Codex).
func MixedModels(a LocalAccount) bool {
	native := NativeProtocol(a.Kind)
	for _, m := range append(append([]string{}, a.Models...), a.Model) {
		if p := InferProtocol(m, a.Wire); p != "" && p != native {
			return true
		}
	}
	return false
}

// NeedsGateway: conversion is required for the default model, or the tool may
// pick other-family models at runtime (model picker override / phone switching).
// Reverse-proxy and OAuth/official sources are always gateway-backed.  Their
// upstream identity must be selected by Salcara and bound to the current
// process lease; writing their URL/key directly into a native tool profile
// would let an independently started process bypass that boundary.
func NeedsGateway(a LocalAccount) bool {
	return AccountServiceTier(a.SpeedMode) != "" || a.CredentialRef != "" || a.SourceType == "reverse_proxy" || a.SourceType == "oauth" || a.SourceType == "official_account" || a.PoolEnabled || NeedsAdapter(a) || (a.CatalogOverride && MixedModels(a))
}

// ToolConnection keeps every production local connection behind its applied
// source and route lease, including relay APIs that speak the native protocol.
// Bridge-run remote tools use their own route-scoped derivative of the process lease.
// Upstream credentials remain in the vault and are never sent to the Hub.
func (c Config) ToolConnection(a LocalAccount, target string, port int) LocalAccount {
	if c.GatewayRuntimeKey != "" || target == "claude-desktop" || strings.HasPrefix(target, "remote-") || strings.HasPrefix(target, "direct-") || NeedsGateway(a) {
		key := c.localGatewayCredential(a, target)
		if strings.HasPrefix(target, "direct-") {
			key = c.DirectGatewayCredential(target)
		} else if strings.HasPrefix(target, "remote-") {
			key = c.RemoteGatewayCredential(target)
		}
		a.BaseURL = fmt.Sprintf("http://127.0.0.1:%d/gateway/%s", port, ToolFamily(target))
		// A running Bridge owns the loopback gateway for this process.  Use the
		// route-scoped lease for every managed tool, including a native-protocol
		// relay API; the persisted GatewayKey is only a
		// compatibility fallback for library callers that have not armed a
		// runtime.  This makes a config/profile left by an older Bridge fail
		// closed after restart instead of silently reusing the saved upstream.
		a.Key, a.AuthMode = key, "bearer"
	}
	return a
}
