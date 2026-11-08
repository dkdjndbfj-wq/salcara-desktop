// systemprompt.go — per-account and per-client system prompt injection.
//
// Cockpit and Magpie both support injecting a system prompt prefix on every
// request forwarded to an account or client. This is useful for:
//   - Channel-specific safety guardrails (e.g. "You are a coding assistant,
//     do not discuss politics.")
//   - Watermarking (inject a hidden signature into every prompt)
//   - Operator persona (give every request a default system persona)
//   - Debugging (inject a "think step by step" prefix without modifying clients)
//
// The injection is applied AFTER request validation and model resolution, so
// it cannot be used to bypass model ACLs or override the client's own system
// prompt — it is prepended, not replaced.
//
// Two injection points:
//   1. Per-account: LocalAccount.SystemPromptPrefix
//   2. Per-client:  NamedAPIClient.SystemPromptPrefix (applied first)
//
// If both are set, the client prefix is prepended before the account prefix.
//
// Drop this file into internal/apiservice/ alongside service.go.
// Wire it in service.go immediately after the body is read and validated:
//
//	body = InjectSystemPromptPrefix(body, selected.SystemPromptPrefix, a.SystemPromptPrefix, messages)

package apiservice

import (
	"encoding/json"
)

// InjectSystemPromptPrefix prepends clientPrefix + accountPrefix to the
// system prompt inside the request body. If both are empty, body is returned
// unchanged. If the request has no system prompt, a new one is created.
//
// isMessages indicates whether the body is an Anthropic Messages API payload
// (true) or an OpenAI Responses/Chat Completions payload (false). The system
// prompt field name differs between them.
//
// The function never modifies the caller's slice; it always returns a new one
// on mutation.
func InjectSystemPromptPrefix(body []byte, clientPrefix, accountPrefix string, isMessages bool) []byte {
	combined := combinePrefix(clientPrefix, accountPrefix)
	if combined == "" || len(body) == 0 {
		return body
	}

	var input map[string]json.RawMessage
	if json.Unmarshal(body, &input) != nil {
		return body // malformed body — don't touch it
	}

	if isMessages {
		// Anthropic Messages API: system is a top-level string or array of
		// content blocks. We only handle the string case here (the common path).
		// A block-form system prompt is left untouched to avoid breaking tool-use flows.
		input["system"] = prependToSystemField(input["system"], combined, false)
	} else {
		// OpenAI Responses / Chat Completions: system is inside the messages array
		// as the first element with role=system, or can be a top-level string in
		// some legacy shapes. Handle both.
		if _, ok := input["system"]; ok {
			// Top-level "system" field (some OpenAI extensions).
			input["system"] = prependToSystemField(input["system"], combined, false)
		} else if msgs, ok := input["messages"]; ok {
			input["messages"] = prependToSystemMessage(msgs, combined)
		} else {
			// No system key: create one.
			raw, _ := json.Marshal(combined)
			input["system"] = raw
		}
	}

	out, err := json.Marshal(input)
	if err != nil {
		return body
	}
	return out
}

// combinePrefix joins client and account prefixes with a double newline
// separator so they read as separate paragraphs.
func combinePrefix(clientPrefix, accountPrefix string) string {
	if clientPrefix == "" {
		return accountPrefix
	}
	if accountPrefix == "" {
		return clientPrefix
	}
	return clientPrefix + "\n\n" + accountPrefix
}

// prependToSystemField prepends text to a JSON field that is either a string
// or an array of content blocks. Returns the modified raw JSON.
func prependToSystemField(raw json.RawMessage, prefix string, blockMode bool) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		// No existing system prompt — create a plain string.
		out, _ := json.Marshal(prefix)
		return out
	}

	// Try string first.
	var s string
	if json.Unmarshal(raw, &s) == nil {
		out, _ := json.Marshal(prefix + "\n\n" + s)
		return out
	}

	// Array of content blocks (Anthropic extended system format) — leave untouched.
	return raw
}

// prependToSystemMessage handles the OpenAI messages array, prepending prefix
// to the first role=system message, or inserting one if none exists.
func prependToSystemMessage(raw json.RawMessage, prefix string) json.RawMessage {
	var msgs []json.RawMessage
	if json.Unmarshal(raw, &msgs) != nil || len(msgs) == 0 {
		// Insert a new system message at position 0.
		sys, _ := json.Marshal(map[string]string{"role": "system", "content": prefix})
		updated := append([]json.RawMessage{sys}, msgs...)
		out, _ := json.Marshal(updated)
		return out
	}

	// Check if the first message is a system message.
	var first map[string]json.RawMessage
	if json.Unmarshal(msgs[0], &first) == nil {
		var role string
		if json.Unmarshal(first["role"], &role) == nil && role == "system" {
			var content string
			if json.Unmarshal(first["content"], &content) == nil {
				// Prepend to existing content.
				first["content"], _ = json.MarshalIndent(prefix+"\n\n"+content, "", "")
				msgs[0], _ = json.Marshal(first)
				out, _ := json.Marshal(msgs)
				return out
			}
			// Content is a block array — skip injection for this shape.
			return raw
		}
	}

	// No system message at position 0 — insert one.
	sys, _ := json.Marshal(map[string]string{"role": "system", "content": prefix})
	updated := append([]json.RawMessage{sys}, msgs...)
	out, _ := json.Marshal(updated)
	return out
}
