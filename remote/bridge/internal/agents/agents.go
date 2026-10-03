// Package agents drives the coding tools on this computer (Codex via `codex app-server`, Claude Code via
// `claude -p --input-format stream-json`) and reads their local session history. Everything it observes is
// reported as protocol.Event values through the Sink.
package agents

import (
	"context"
	"net/http"

	"salcara/bridge/internal/protocol"
)

// Sink receives every event produced by any agent (the core forwards them to the hub and the local console).
type Sink func(protocol.Event)

// Settings the core passes in; read again on every new session (the user may change them in the console).
type Settings struct {
	RelayRoot        string // e.g. https://relay.example.com (no /v1)
	CodexRoot        string // applied local API, independent of Hub authentication
	ClaudeRoot       string
	ClaudeAuthMode   string
	UseOriginalCodex bool // honor original provider/config to retain history
	CodexProvider    string
	CodexPath        string // optional runtime CLI override from the workbench
	ClaudePath       string
	CodexKey         string // relay API key used for Codex (OpenAI group); may equal ClaudeKey
	ClaudeKey        string // relay API key used for Claude Code (Anthropic group)
	Approval         string // default policy for new sessions: ask | auto_edits | auto_all
	CodexModel       string // optional default model
	ClaudeModel      string
	// ApprovalHelper is how the Claude permission-prompt MCP server reaches the bridge:
	// the bridge binary path (run as `<exe> mcp-approval --port N --token T`) and the local port/token.
	BridgeExe  string
	LocalPort  int
	LocalToken string
}

func (s Settings) codexRoot() string {
	if s.CodexRoot != "" || s.UseOriginalCodex {
		return s.CodexRoot
	}
	return s.RelayRoot
}
func (s Settings) claudeRoot() string {
	if s.ClaudeRoot != "" {
		return s.ClaudeRoot
	}
	return s.RelayRoot
}

// codexProvider is the model_provider passed on thread start/resume. Empty means
// "Codex's own configuration": nothing is configured in Bridge, so the worker
// behaves exactly like Codex on the desktop (same login, same provider).
func (s Settings) codexProvider() string {
	if !s.UseOriginalCodex && s.codexRoot() == "" {
		return ""
	}
	if s.CodexProvider != "" {
		return s.CodexProvider
	}
	if s.UseOriginalCodex {
		return "openai"
	}
	return "salcara"
}

// Agent is one tool (codex / claude).
type Agent interface {
	ID() string   // "codex" | "claude"
	Name() string // "Codex" | "Claude Code"
	// Detect finds the executable and version; cheap enough to call on every register.
	Detect(ctx context.Context) protocol.Tool
	// Sessions lists this tool's sessions on this computer (external ones included), newest first.
	Sessions(ctx context.Context) ([]protocol.SessionInfo, error)
	// Open returns a session's history as events (≤400, oldest first).
	Open(ctx context.Context, id string) (protocol.SessionInfo, []protocol.Event, error)
	// Start runs a new task in cwd; returns the session id (without the "codex:"/"claude:" prefix).
	Start(ctx context.Context, cwd, prompt, model, approval string) (string, error)
	// Send continues a session (resumes it if it isn't running under the bridge).
	Send(ctx context.Context, id, text string) error
	Interrupt(ctx context.Context, id string) error
	// Respond answers an approval this agent raised. Returns false if the approval id isn't this agent's.
	Respond(approvalID, decision, message string) bool
	// Models is optional; nil when unknown.
	Models(ctx context.Context) []string
	// Close stops every child process.
	Close()
}

// ApprovalResponse keeps question answers separate from approval feedback. API
// keys and other credentials are never supplied through this contract.
type ApprovalResponse struct {
	Decision string              `json:"decision"`
	Message  string              `json:"message,omitempty"`
	Answers  map[string][]string `json:"answers,omitempty"`
}

// RespondWithAnswers preserves the existing Agent interface for older adapters.
// Adapters without question support cannot silently discard a submitted answer.
func RespondWithAnswers(agent Agent, approvalID string, response ApprovalResponse) bool {
	if responder, ok := agent.(interface {
		RespondWithAnswers(string, ApprovalResponse) bool
	}); ok {
		return responder.RespondWithAnswers(approvalID, response)
	}
	if response.Answers != nil {
		return false
	}
	return agent.Respond(approvalID, response.Decision, response.Message)
}

// Manager owns all agents; the core talks only to it.
type Manager interface {
	Agents() []Agent
	Get(tool string) Agent
	// Watch starts background watching of local session files for live updates of external sessions.
	Watch(ctx context.Context)
	// LocalHandler serves the bridge-internal endpoints under /internal/agents/ on 127.0.0.1 (e.g. the
	// Claude permission-prompt MCP helper asking for a decision). It checks Settings.LocalToken itself.
	LocalHandler() http.Handler
	Close()
}

// NewManager(sink Sink, settings func() Settings) Manager is implemented in manager.go.
