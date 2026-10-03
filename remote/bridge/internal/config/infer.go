package config

import "strings"

// InferProtocol picks the upstream wire protocol for a model on a provider.
// An explicit provider interface type wins; otherwise the model family decides,
// so one relay can serve Claude (Messages), GPT (Responses) and Grok/DeepSeek/
// Qwen/GLM/Kimi/Gemini (Chat Completions) to any tool through the local gateway.
// Unknown model names return "" so callers keep the tool's native protocol.
func InferProtocol(model, wire string) string {
	switch wire {
	case "responses", "chat", "anthropic":
		return wire
	}
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	switch {
	case m == "":
		return ""
	case strings.HasPrefix(m, "claude") || strings.Contains(m, "anthropic"):
		return "anthropic"
	case strings.HasPrefix(m, "gpt") || strings.HasPrefix(m, "codex") || strings.HasPrefix(m, "o1") || strings.HasPrefix(m, "o3") || strings.HasPrefix(m, "o4") || strings.HasPrefix(m, "chatgpt"):
		return "responses"
	}
	for _, family := range chatFamilies {
		if strings.HasPrefix(m, family) {
			return "chat"
		}
	}
	return "" // unknown: keep the tool's native protocol
}

var chatFamilies = []string{"grok", "deepseek", "qwen", "qwq", "glm", "kimi", "moonshot", "gemini", "gemma", "doubao", "mistral", "codestral", "llama", "minimax", "abab", "hunyuan", "ernie", "yi-", "step-", "baichuan", "spark"}
