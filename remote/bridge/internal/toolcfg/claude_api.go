package toolcfg

import "strings"

// ClaudeAPIEnvironment pins an API connection without inheriting a competing
// cloud provider, custom authorization header, or saved OAuth environment.
// Empty values matter: Claude merges settings.env, so deleting a key would let
// an earlier user/project setting win. Unrelated settings are left untouched.
func ClaudeAPIEnvironment(base, key, authMode string) map[string]string {
	env := map[string]string{
		"ANTHROPIC_BASE_URL": strings.TrimRight(base, "/"),
		"ANTHROPIC_API_KEY":  "", "ANTHROPIC_AUTH_TOKEN": "",
		"ANTHROPIC_CUSTOM_HEADERS":        "",
		"CLAUDE_CODE_OAUTH_TOKEN":         "",
		"CLAUDE_CODE_OAUTH_REFRESH_TOKEN": "", "CLAUDE_CODE_OAUTH_SCOPES": "",
		"CLAUDE_CODE_USE_BEDROCK": "", "CLAUDE_CODE_USE_VERTEX": "",
		"CLAUDE_CODE_USE_FOUNDRY": "", "CLAUDE_CODE_USE_ANTHROPIC_AWS": "",
		"CLAUDE_CODE_USE_MANTLE": "",
		"ANTHROPIC_PROFILE":      "", "ANTHROPIC_FEDERATION_RULE_ID": "",
		"ANTHROPIC_ORGANIZATION_ID": "",
	}
	if authMode == "api-key" {
		env["ANTHROPIC_API_KEY"] = key
	} else {
		env["ANTHROPIC_AUTH_TOKEN"] = key
	}
	return env
}
