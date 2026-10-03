package agents

import (
	"context"
	"errors"
	"strings"
	"time"
)

const errCodexRemoteProvider = "此 API 配置无法用于远程连接"

func needsCodexProviderGuard(s Settings) bool {
	return !s.UseOriginalCodex && s.codexRoot() != "" && s.codexProvider() != "openai"
}

// Only Bridge-managed custom workers are checked. Codex's own login/config and
// the built-in OpenAI URL override retain their original behavior. This reads
// effective configuration locally; it never sends a model request.
func validateCodexEffectiveProvider(ctx context.Context, c *rpcConn, s Settings) error {
	if !needsCodexProviderGuard(s) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var response map[string]any
	if err := c.Call(ctx, "config/read", map[string]any{"includeLayers": false}, &response); err != nil {
		// The native error can contain user configuration values. Never expose it.
		return errors.New(errCodexRemoteProvider)
	}
	if !codexEffectiveProviderSafe(response, s) {
		return errors.New(errCodexRemoteProvider)
	}
	return nil
}

func codexEffectiveProviderSafe(response map[string]any, s Settings) bool {
	cfg, _ := response["config"].(map[string]any)
	provider := s.codexProvider()
	providers, _ := cfg["model_providers"].(map[string]any)
	entry, _ := providers[provider].(map[string]any)
	if entry == nil || cfg["model_provider"] != provider || entry["base_url"] != strings.TrimRight(s.codexRoot(), "/")+"/v1" ||
		entry["env_key"] != "SUB2API_API_KEY" || entry["wire_api"] != "responses" ||
		entry["requires_openai_auth"] != false || entry["supports_websockets"] != false {
		return false
	}
	for _, field := range []string{"http_headers", "env_http_headers", "query_params", "experimental_bearer_token", "auth", "aws"} {
		if codexProviderFieldNonempty(entry[field]) {
			return false
		}
	}
	return true
}

func codexProviderFieldNonempty(value any) bool {
	switch value := value.(type) {
	case nil:
		return false
	case string:
		return value != ""
	case map[string]any:
		return len(value) != 0
	default:
		// Unknown shapes cannot establish a credential-free provider.
		return true
	}
}
