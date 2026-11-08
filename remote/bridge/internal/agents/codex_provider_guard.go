package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const errCodexRemoteProvider = "此 API 配置无法用于远程连接"

type codexThreadProviderMismatch struct{}

func (codexThreadProviderMismatch) Error() string {
	return "这条原会话绑定的 Codex 供应商与当前电脑配置不同，请在电脑切回该会话原来的供应商配置后重试"
}

func (codexThreadProviderMismatch) ExecutionErrorCode() string { return "session_provider_mismatch" }

func codexResumeFailure(err error) error {
	var native *rpcError
	var data struct {
		Type string `json:"type"`
	}
	if errors.As(err, &native) && json.Unmarshal(native.Data, &data) == nil && data.Type == "modelProviderMismatch" {
		return codexThreadProviderMismatch{}
	}
	return fmt.Errorf("Codex 恢复会话失败：%w", err)
}

func needsCodexProviderGuard(s Settings) bool {
	return !s.UseOriginalCodex && s.codexRoot() != ""
}

// Only Bridge-managed workers are checked. Codex's own login/config retains
// its original behavior. This local read never sends a model request.
func validateCodexEffectiveProvider(ctx context.Context, c *rpcConn, s Settings) error {
	return validateCodexEffectiveProviderAt(ctx, c, s, "")
}

// Project layers can contribute authentication headers too. Read from the
// actual task directory before each start/resume/send, not only process cwd.
func validateCodexEffectiveProviderAt(ctx context.Context, c *rpcConn, s Settings, cwd string) error {
	if !needsCodexProviderGuard(s) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var response map[string]any
	params := map[string]any{"includeLayers": false}
	if cwd != "" {
		params["cwd"] = cwd
	}
	if err := c.Call(ctx, "config/read", params, &response); err != nil {
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
	if cfg == nil || cfg["model_provider"] != provider {
		return false
	}
	providers, _ := cfg["model_providers"].(map[string]any)
	if provider == "openai" {
		// A user-defined reserved provider can override auth/transport beyond
		// openai_base_url. Do not guess its effective built-in merge semantics.
		if providers != nil && codexProviderFieldNonempty(providers[provider]) {
			return false
		}
		if raw := cfg["model_providers"]; raw != nil && providers == nil {
			return false
		}
		return cfg["openai_base_url"] == strings.TrimRight(s.codexRoot(), "/")+"/v1" &&
			cfg["cli_auth_credentials_store"] == "ephemeral" && cfg["forced_login_method"] == "api"
	}
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

// The store was verified above before this mutating auth RPC. Its value is
// scoped to the Bridge-owned process and never returned in logs/errors. Old
// app-servers which cannot establish this mode fail before any thread starts.
func initializeCodexBuiltinAuthentication(ctx context.Context, c *rpcConn, s Settings) error {
	if !needsCodexProviderGuard(s) || s.codexProvider() != "openai" {
		return nil
	}
	if s.CodexKey == "" {
		return errors.New(errCodexRemoteProvider)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var login struct {
		Type string `json:"type"`
	}
	if c.Call(ctx, "account/login/start", map[string]any{"type": "apiKey", "apiKey": s.CodexKey}, &login) != nil || login.Type != "apiKey" {
		return errors.New(errCodexRemoteProvider)
	}
	var status struct {
		Account *struct {
			Type string `json:"type"`
		} `json:"account"`
		RequiresOpenaiAuth bool `json:"requiresOpenaiAuth"`
	}
	if c.Call(ctx, "account/read", map[string]any{"refreshToken": false}, &status) != nil || status.Account == nil || status.Account.Type != "apiKey" || !status.RequiresOpenaiAuth {
		return errors.New(errCodexRemoteProvider)
	}
	return nil
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
