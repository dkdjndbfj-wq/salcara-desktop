// capabilities.go — extends LocalAccount with per-account capability declarations
// and per-account extra headers.
//
// Drop this file into internal/config/ alongside accounts.go.
//
// Capabilities lets operators declare what an account can actually do, so the
// request router can filter the candidate pool BEFORE selecting — instead of
// discovering that, say, an account lacks vision support after forwarding the
// request and getting a 400 back.
//
// ExtraHeaders lets operators attach static outgoing headers to every request
// forwarded to one account. Common uses: X-Custom-Org, x-api-org, channel-
// specific authentication, or provider-specific routing hints. These headers
// are injected by the gateway after auth is resolved; they are never stored in
// the AccountFingerprint because they are routing metadata, not identity.

package config

import "strings"

// AccountCapabilities declares the optional features an account supports.
// The zero value ("unknown") is treated as "probably yes" for all fields so
// that existing unconfigured accounts keep working.  Set a field to false only
// when you know the account does NOT support that capability.
type AccountCapabilities struct {
	// Streaming controls whether the account accepts stream=true requests.
	// Nil means "unknown / probably yes".
	Streaming *bool `json:"streaming,omitempty"`

	// FunctionCalling controls whether the account accepts tool_choice/tools.
	FunctionCalling *bool `json:"functionCalling,omitempty"`

	// Vision controls whether the account accepts image_url content blocks.
	Vision *bool `json:"vision,omitempty"`

	// MaxContextTokens is the maximum total token budget (prompt + completion).
	// 0 means unknown. The router uses this to skip accounts whose budget is
	// smaller than the estimated request size.
	MaxContextTokens int `json:"maxContextTokens,omitempty"`

	// Reasoning controls whether the account supports extended thinking / o1-style
	// reasoning tokens (Anthropic extended_thinking, OpenAI o-series).
	Reasoning *bool `json:"reasoning,omitempty"`
}

// boolTrue/boolFalse are package-level helpers for constructing capability
// pointers in tests and migrations.
func BoolPtr(b bool) *bool { return &b }

// SupportsStreaming reports whether the account is known to support streaming.
// Returns true when unknown (fail-open).
func (c AccountCapabilities) SupportsStreaming() bool {
	return c.Streaming == nil || *c.Streaming
}

// SupportsFunctionCalling reports whether the account is known to support tool use.
// Returns true when unknown (fail-open).
func (c AccountCapabilities) SupportsFunctionCalling() bool {
	return c.FunctionCalling == nil || *c.FunctionCalling
}

// SupportsVision reports whether the account is known to support image inputs.
// Returns true when unknown (fail-open).
func (c AccountCapabilities) SupportsVision() bool {
	return c.Vision == nil || *c.Vision
}

// SupportsReasoning reports whether the account is known to support extended thinking.
// Returns true when unknown (fail-open).
func (c AccountCapabilities) SupportsReasoning() bool {
	return c.Reasoning == nil || *c.Reasoning
}

// ─── request-body capability probe ──────────────────────────────────────────

// RequestNeeds is computed from the request body to determine what capabilities
// are required. Used by FilterByCapabilities.
type RequestNeeds struct {
	Streaming       bool
	FunctionCalling bool // request body contains "tools" or "tool_choice"
	Vision          bool // request body contains image_url content blocks
	Reasoning       bool // request body contains extended_thinking or reasoning_effort
	EstimatedTokens int  // rough upper-bound, 0 = unknown
}

// FilterByCapabilities removes accounts from candidates that definitely lack a
// capability required by needs.  Accounts with unknown capabilities (nil) are
// kept so we don't accidentally drop accounts that were configured before this
// field existed.
//
// If no account survives the filter (e.g. all accounts have Vision=false but
// the request contains an image), the original slice is returned unchanged so
// the request can still attempt to proceed — the upstream error will be more
// informative than a local 503.
func FilterByCapabilities(candidates []LocalAccount, needs RequestNeeds) []LocalAccount {
	if !needs.Streaming && !needs.FunctionCalling && !needs.Vision && !needs.Reasoning && needs.EstimatedTokens == 0 {
		return candidates // fast path: no special needs
	}
	var filtered []LocalAccount
	for _, a := range candidates {
		caps := a.Capabilities
		if needs.Streaming && !caps.SupportsStreaming() {
			continue
		}
		if needs.FunctionCalling && !caps.SupportsFunctionCalling() {
			continue
		}
		if needs.Vision && !caps.SupportsVision() {
			continue
		}
		if needs.Reasoning && !caps.SupportsReasoning() {
			continue
		}
		if needs.EstimatedTokens > 0 && caps.MaxContextTokens > 0 && needs.EstimatedTokens > caps.MaxContextTokens {
			continue
		}
		filtered = append(filtered, a)
	}
	if len(filtered) == 0 {
		return candidates // fail-open
	}
	return filtered
}

// ProbeRequestNeeds scans a request body for capability signals.
// It never reads past 256 KB and never stores anything from the body.
func ProbeRequestNeeds(body []byte, isStream bool) RequestNeeds {
	needs := RequestNeeds{Streaming: isStream}
	if len(body) == 0 {
		return needs
	}

	b := body
	if len(b) > 256*1024 {
		b = b[:256*1024]
	}
	s := string(b)

	if strings.Contains(s, `"tools"`) || strings.Contains(s, `"tool_choice"`) {
		needs.FunctionCalling = true
	}
	if strings.Contains(s, `"image_url"`) || strings.Contains(s, `"type":"image"`) || strings.Contains(s, `"type": "image"`) {
		needs.Vision = true
	}
	if strings.Contains(s, `"extended_thinking"`) || strings.Contains(s, `"reasoning_effort"`) || strings.Contains(s, `"thinking"`) {
		needs.Reasoning = true
	}
	needs.EstimatedTokens = len(body) / 4

	return needs
}
