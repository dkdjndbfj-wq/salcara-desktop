package agents

import "salcara/bridge/internal/config"

// SettingsFromConfig keeps Hub login separate from the API actually applied to
// the original tool. A selection alone must never change the remote worker's key.
func SettingsFromConfig(c config.Config) Settings {
	return SettingsWithGateway(c, 47831)
}

// SettingsWithGateway resolves which API the Bridge-run workers use, in order:
//  1. the API the paired phone picked for this agent (RemoteAPI),
//  2. the API applied to the original tool on this computer,
//  3. the legacy relay login keys,
//  4. nothing: the tool runs with its own login/config, exactly like on the desktop.
func SettingsWithGateway(c config.Config, port int) Settings {
	s := Settings{
		RelayRoot: c.RelayRoot, CodexKey: c.EffectiveCodexKey(), ClaudeKey: c.EffectiveClaudeKey(),
		Approval: c.Approval, CodexModel: c.CodexModel, ClaudeModel: c.ClaudeModel,
		CodexPath: c.LocalToolPaths["codex"], ClaudePath: c.LocalToolPaths["claude"],
	}
	if c.RemoteDeviceOnly {
		s.CodexKey, s.ClaudeKey, s.RelayRoot = "", "", ""
	}
	if a, ok := c.RemoteToolAccount("codex"); ok {
		a = remoteConnection(c, a, "codex", port)
		s.UseOriginalCodex, s.CodexProvider = false, ""
		s.CodexRoot, s.CodexKey, s.CodexModel = a.BaseURL, a.Key, a.Model
	} else if binding, exists := c.ToolAPIApplied["codex"]; exists {
		s.UseOriginalCodex, s.CodexProvider, s.CodexKey = true, binding.Provider, ""
		if a, ok := c.AppliedToolAccount("codex"); ok {
			var converted bool
			a, converted = workerConnection(c, a, "codex", port)
			if converted {
				// The original desktop provider may still point directly upstream.
				// Override this Bridge-owned worker only; otherwise it would ignore
				// the newly required gateway despite the refreshed catalog.
				s.UseOriginalCodex = false
				if s.CodexProvider == "" {
					s.CodexProvider = "openai"
				}
			}
			s.CodexRoot, s.CodexKey, s.CodexModel = a.BaseURL, a.Key, a.Model
		}
	}
	if a, ok := c.RemoteToolAccount("claude"); ok {
		a = remoteConnection(c, a, "claude", port)
		s.ClaudeRoot, s.ClaudeKey, s.ClaudeModel, s.ClaudeAuthMode = a.BaseURL, a.Key, a.Model, a.AuthMode
	} else if _, exists := c.ToolAPIApplied["claude"]; exists {
		s.ClaudeKey = ""
		if a, ok := c.AppliedToolAccount("claude"); ok {
			a, _ = workerConnection(c, a, "claude", port)
			s.ClaudeRoot, s.ClaudeKey, s.ClaudeModel, s.ClaudeAuthMode = a.BaseURL, a.Key, a.Model, a.AuthMode
		}
	}
	return s
}

// remoteConnection routes a phone-selected API through the loopback gateway
// only when its wire protocol differs from what the agent speaks.
func remoteConnection(c config.Config, a config.LocalAccount, family string, port int) config.LocalAccount {
	a, _ = workerConnection(c, a, config.RemoteGatewayTarget(family), port)
	return a
}

// workerConnection adapts the Bridge-owned process to the current key's catalog,
// including an Applied API whose original desktop model-picker override was off.
// This transient copy never changes the original tool's provider or applied binding.
func workerConnection(c config.Config, a config.LocalAccount, target string, port int) (config.LocalAccount, bool) {
	if a.Model != "" && len(a.Models) > 0 {
		member := false
		for _, model := range a.Models {
			member = member || model == a.Model
		}
		if !member {
			// The upstream group can retire a default while key/address stay the
			// same. Clear this worker-only default; never choose the first model
			// or rewrite the applied desktop binding.
			a.Model = ""
		}
	}
	// The phone can switch models per message, so any API that also offers
	// other-family models (Grok, Claude behind Codex…) goes through the gateway,
	// which picks the upstream wire per request.
	converted := config.NeedsAdapter(a) || config.MixedModels(a)
	if converted {
		a.CatalogOverride = true
		a = c.ToolConnection(a, target, port)
	}
	if root, _, err := config.APIBase(a.BaseURL); err == nil {
		a.BaseURL = root
	}
	return a, converted
}
