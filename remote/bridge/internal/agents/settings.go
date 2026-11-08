package agents

import (
	"context"
	"strings"

	"salcara/bridge/internal/config"
)

type configSnapshotKey struct{}
type localSettingsKey struct{}
type directSettingsKey struct{}

// WithConfigSnapshot is internal-only; phone JSON cannot construct this key.
func WithConfigSnapshot(ctx context.Context, cfg config.Config) context.Context {
	ctx = context.WithValue(ctx, localSettingsKey{}, false)
	ctx = context.WithValue(ctx, directSettingsKey{}, false)
	return context.WithValue(ctx, configSnapshotKey{}, cfg.Clone())
}

// WithLocalConfigSnapshot freezes an authenticated console operation to the
// locally applied source. The context key cannot be supplied by phone JSON.
func WithLocalConfigSnapshot(ctx context.Context, cfg config.Config) context.Context {
	return context.WithValue(WithConfigSnapshot(ctx, cfg), localSettingsKey{}, true)
}

// WithDirectConfigSnapshot freezes the pinned phone's independent source choice.
func WithDirectConfigSnapshot(ctx context.Context, cfg config.Config) context.Context {
	return context.WithValue(WithConfigSnapshot(ctx, cfg), directSettingsKey{}, true)
}

// workerGatewayForContext keeps auxiliary requests on the same route as their
// worker. local also identifies routes that may use a local account pool.
func workerGatewayForContext(ctx context.Context, cfg config.Config, family string) (target, credential string, local bool) {
	if direct, _ := ctx.Value(directSettingsKey{}).(bool); direct {
		if choice, selected := cfg.DirectAPI[family]; selected && choice.AccountID != "" {
			target = config.DirectGatewayTarget(family)
			return target, cfg.DirectGatewayCredential(target), false
		}
		return family, cfg.LocalGatewayCredential(family), true
	}
	if local, _ := ctx.Value(localSettingsKey{}).(bool); local {
		return family, cfg.LocalGatewayCredential(family), true
	}
	target = config.RemoteGatewayTarget(family)
	return target, cfg.RemoteGatewayCredential(target), false
}

func settingsForContext(ctx context.Context, fallback func() Settings) Settings {
	base := fallback()
	cfg, ok := ctx.Value(configSnapshotKey{}).(config.Config)
	if !ok {
		return base
	}
	port := base.LocalPort
	if port == 0 {
		port = 47831
	}
	s := SettingsWithGateway(cfg, port)
	if local, _ := ctx.Value(localSettingsKey{}).(bool); local {
		s = LocalSettingsWithGateway(cfg, port)
	}
	if direct, _ := ctx.Value(directSettingsKey{}).(bool); direct {
		s = DirectSettingsWithGateway(cfg, port)
	}
	s.BridgeExe, s.LocalPort, s.LocalToken = base.BridgeExe, port, base.LocalToken
	if s.CodexPath == "" {
		s.CodexPath = base.CodexPath
	}
	if s.ClaudePath == "" {
		s.ClaudePath = base.ClaudePath
	}
	if s.DeepSeekPath == "" {
		s.DeepSeekPath = base.DeepSeekPath
	}
	if s.HermesPath == "" {
		s.HermesPath = base.HermesPath
	}
	return s
}

// SettingsFromConfig keeps Hub login separate from the API actually applied to
// the original tool. A selection alone must never change the remote worker's key.
func SettingsFromConfig(c config.Config) Settings {
	return SettingsWithGateway(c, 47831)
}

// SettingsWithGateway resolves which API the Bridge-run workers use, in order:
//  1. the API the paired phone picked for this agent (RemoteAPI),
//  2. the API applied to the original tool on this computer,
//  3. the legacy relay login keys,
//  4. nothing: Codex may use its own configuration; Claude refuses new execution
//     until an explicit API is selected (native login is never a fallback).
func SettingsWithGateway(c config.Config, port int) Settings {
	return workerSettings(c, port, true)
}

// LocalSettingsWithGateway uses the computer's applied API independently of
// the paired phone's selection and station lease. It still requires the local
// launch lease and pins Bridge-owned workers to the loopback gateway.
func LocalSettingsWithGateway(c config.Config, port int) Settings {
	c.RemoteAPI = nil
	return workerSettings(c, port, false)
}

// DirectSettingsWithGateway follows the computer until this phone explicitly
// selects a source. An invalid or revoked direct choice fails closed instead of
// falling back to the native tool's login, Hub account, or previously applied API.
func DirectSettingsWithGateway(c config.Config, port int) Settings {
	s := LocalSettingsWithGateway(c, port)
	for _, family := range []string{"codex", "claude", "deepseek-harness", "hermes"} {
		if choice, selected := c.DirectAPI[family]; !selected || choice.AccountID == "" {
			continue
		}
		a, ok := c.DirectToolAccount(family)
		sourceFingerprint := ""
		if ok {
			sourceFingerprint = config.AccountFingerprint(a)
			a, _ = workerConnection(c, a, config.DirectGatewayTarget(family), port, false)
		}
		switch family {
		case "codex":
			s.UseOriginalCodex, s.CodexProvider = false, appliedCodexProvider(c)
			s.CodexRoot, s.CodexKey, s.CodexModel = a.BaseURL, a.Key, a.Model
		case "claude":
			s.ClaudeRoot, s.ClaudeKey, s.ClaudeModel, s.ClaudeAuthMode = a.BaseURL, a.Key, a.Model, a.AuthMode
		default:
			s.Extensions[family] = ExtensionSettings{Root: a.BaseURL, Key: a.Key, Model: a.Model, SourceFingerprint: sourceFingerprint}
		}
	}
	return s
}

// The native runtime binds persisted threads to a provider ID independently of
// the upstream URL/key. Selecting a phone API keeps the computer's applied ID;
// only the managed gateway route and credential change. Unconfigured native
// Codex uses the built-in openai provider on both phone transports.
func appliedCodexProvider(c config.Config) string {
	if binding, ok := c.AppliedToolBinding("codex"); ok && binding.Provider != "" {
		return binding.Provider
	}
	return "openai"
}

func workerSettings(c config.Config, port int, remote bool) Settings {
	s := Settings{
		RelayRoot: c.RelayRoot, CodexKey: c.EffectiveCodexKey(), ClaudeKey: c.EffectiveClaudeKey(),
		Approval: c.Approval, CodexModel: c.CodexModel, ClaudeModel: c.ClaudeModel,
		CodexPath: c.LocalToolPaths["codex"], ClaudePath: c.LocalToolPaths["claude"],
		DeepSeekPath: c.LocalToolPaths["deepseek-harness"], HermesPath: c.LocalToolPaths["hermes"],
		Extensions: map[string]ExtensionSettings{},
	}
	if c.RemoteDeviceOnly {
		s.CodexKey, s.ClaudeKey, s.RelayRoot = "", "", ""
	}
	if a, ok := c.RemoteToolAccount("codex"); ok {
		a = remoteConnection(c, a, "codex", port)
		s.UseOriginalCodex, s.CodexProvider = false, appliedCodexProvider(c)
		s.CodexRoot, s.CodexKey, s.CodexModel = a.BaseURL, a.Key, a.Model
	} else if binding, exists := c.AppliedToolBinding("codex"); exists {
		s.UseOriginalCodex, s.CodexProvider, s.CodexKey = true, binding.Provider, ""
		if a, ok := c.AuthorizedAppliedToolAccount("codex"); ok {
			var converted bool
			a, converted = workerConnection(c, a, "codex", port, remote)
			if remote && c.RemoteExecutionArmed && !converted {
				// A remote worker must use the explicit vault snapshot, never the
				// native config file. This prevents a third-party proxy/config edit
				// from changing the source after Salcara was last used.
				s.UseOriginalCodex = false
				if s.CodexProvider == "" {
					s.CodexProvider = "openai"
				}
			}
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
	} else if _, exists := c.AppliedToolBinding("claude"); exists {
		s.ClaudeKey = ""
		if a, ok := c.AuthorizedAppliedToolAccount("claude"); ok {
			a, _ = workerConnection(c, a, "claude", port, remote)
			s.ClaudeRoot, s.ClaudeKey, s.ClaudeModel, s.ClaudeAuthMode = a.BaseURL, a.Key, a.Model, a.AuthMode
		}
	}
	for _, family := range []string{"deepseek-harness", "hermes"} {
		if a, ok := c.RemoteToolAccount(family); ok {
			sourceFingerprint := config.AccountFingerprint(a)
			a = remoteConnection(c, a, family, port)
			s.Extensions[family] = ExtensionSettings{Root: a.BaseURL, Key: a.Key, Model: a.Model, SourceFingerprint: sourceFingerprint}
		} else if a, ok := c.AuthorizedAppliedToolAccount(family); ok {
			sourceFingerprint := config.AccountFingerprint(a)
			a, _ = workerConnection(c, a, family, port, remote)
			s.Extensions[family] = ExtensionSettings{Root: a.BaseURL, Key: a.Key, Model: a.Model, SourceFingerprint: sourceFingerprint}
		}
	}
	return s
}

// remoteConnection routes a phone-selected API through the loopback gateway.
// The gateway is also used for native protocols so the Bridge's process lease,
// pool selection and API provenance remain the same across model families.
func remoteConnection(c config.Config, a config.LocalAccount, family string, port int) config.LocalAccount {
	a, _ = workerConnection(c, a, config.RemoteGatewayTarget(family), port, true)
	return a
}

// workerConnection adapts the Bridge-owned process to the current key's catalog,
// including an Applied API whose original desktop model-picker override was off.
// This transient copy never changes the original tool's provider or applied binding.
func workerConnection(c config.Config, a config.LocalAccount, target string, port int, remote bool) (config.LocalAccount, bool) {
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
	// Once the Bridge process is armed, every remote worker goes through the
	// current loopback lease, even for a native protocol. This prevents a
	// third-party edit of ~/.codex/config.toml or Claude settings from changing
	// the source behind the phone's selected account. The original desktop
	// process remains untouched; only Bridge-owned workers use this route.
	direct := strings.HasPrefix(target, "direct-")
	converted := direct || c.GatewayRuntimeKey != "" || c.RemoteExecutionArmed || a.PoolEnabled || config.NeedsAdapter(a) || config.MixedModels(a)
	if converted {
		if remote || direct || c.GatewayRuntimeKey == "" {
			a.CatalogOverride = true
		}
		gatewayTarget := target
		if remote && c.RemoteExecutionArmed && !strings.HasPrefix(gatewayTarget, "remote-") {
			gatewayTarget = config.RemoteGatewayTarget(config.ToolKind(gatewayTarget))
		}
		a = c.ToolConnection(a, gatewayTarget, port)
	}
	if root, _, err := config.APIBase(a.BaseURL); err == nil {
		a.BaseURL = root
	}
	return a, converted
}
