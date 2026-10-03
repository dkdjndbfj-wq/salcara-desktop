package config

import (
	"fmt"
)

func NativeProtocol(kind string) string {
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
func NeedsGateway(a LocalAccount) bool {
	return NeedsAdapter(a) || (a.CatalogOverride && MixedModels(a))
}

// ToolConnection gives the original tool a loopback key only in conversion
// mode. Upstream credentials remain in the vault and are never sent to the Hub.
func (c Config) ToolConnection(a LocalAccount, target string, port int) LocalAccount {
	if target == "claude-desktop" || NeedsGateway(a) {
		a.BaseURL = fmt.Sprintf("http://127.0.0.1:%d/gateway/%s", port, ToolFamily(target))
		a.Key, a.AuthMode = c.GatewayKey, "bearer"
	}
	return a
}
