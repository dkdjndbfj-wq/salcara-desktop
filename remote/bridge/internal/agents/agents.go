// Package agents drives the coding tools on this computer (Codex via `codex app-server`, Claude Code via
// `claude -p --input-format stream-json`) and reads their local session history. Everything it observes is
// reported as protocol.Event values through the Sink.
package agents

import (
	"context"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"salcara/bridge/internal/config"

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
	DeepSeekPath     string
	HermesPath       string
	// Extensions have explicit per-adapter sources; there is no relay/native-login fallback.
	Extensions map[string]ExtensionSettings
	// ApprovalHelper is how the Claude permission-prompt MCP server reaches the bridge:
	// the bridge binary path (run as `<exe> mcp-approval --port N --token T`) and the local port/token.
	BridgeExe  string
	LocalPort  int
	LocalToken string
}

type ExtensionSettings struct {
	Root              string
	Key               string
	Model             string
	SourceFingerprint string // stable upstream identity; excludes temporary gateway credentials
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

// Agent is one independently addressable tool (Codex, Claude Code, or an
// extension such as Hermes). The manager aggregates these behind one transport
// while the adapter owns native process/history semantics.
type Agent interface {
	ID() string   // stable adapter id, for example "codex" or "hermes"
	Name() string // public display name
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

// DescriptorProvider is the adapter-side capability contract. It is optional
// at the Go interface level so old test doubles and third-party adapters keep
// compiling; adapters without it are exposed as read-only/unknown until they
// provide an explicit descriptor.
type DescriptorProvider interface {
	Descriptor() protocol.AgentDescriptor
}

// Describe returns a conservative default for legacy adapters. The default
// never grants remote send, approvals, attachments, or model switching merely
// because an adapter happens to implement an Agent method.
func Describe(a Agent) protocol.AgentDescriptor {
	if a == nil {
		return protocol.AgentDescriptor{}
	}
	if p, ok := a.(DescriptorProvider); ok {
		d := p.Descriptor()
		if d.ID == "" {
			d.ID = a.ID()
		}
		if d.Name == "" {
			d.Name = a.Name()
		}
		if d.Tool == "" {
			d.Tool = a.ID()
		}
		if d.APIFamily == "" {
			d.APIFamily = d.Tool
		}
		if d.ControlSurface == "" {
			d.ControlSurface = "cli"
		}
		return d
	}
	return protocol.AgentDescriptor{ID: a.ID(), Name: a.Name(), Tool: a.ID(), APIFamily: a.ID(), ControlSurface: "cli"}
}

var adapterIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// CanonicalDescriptor binds the public/session namespace to the actual
// registered adapter. Sharing credentials is explicit through APIFamily, never
// by advertising another adapter's Tool or ID. A legacy descriptor has no
// capabilities; callers must not infer them from Agent's mandatory methods.
func CanonicalDescriptor(a Agent) (protocol.AgentDescriptor, bool) {
	if a == nil || !adapterIDPattern.MatchString(a.ID()) {
		return protocol.AgentDescriptor{}, false
	}
	d := Describe(a)
	if d.ID != a.ID() || d.Tool != a.ID() || d.Name == "" || len([]rune(d.Name)) > 200 || !config.ValidAgentFamily(d.APIFamily) {
		return protocol.AgentDescriptor{}, false
	}
	for _, r := range d.Name {
		if unicode.IsControl(r) {
			return protocol.AgentDescriptor{}, false
		}
	}
	if d.ControlSurface != "cli" && d.ControlSurface != "desktop" && d.ControlSurface != "read-only" {
		return protocol.AgentDescriptor{}, false
	}
	d.APIFamily = config.NormalizeAgentFamily(d.APIFamily)
	return d, true
}

// SessionBelongsTo is used before publishing adapter-owned directory/history
// records. Empty Tool is a compatible omission; an explicit foreign Tool is
// not. No native origin field can change the session's adapter namespace.
func SessionBelongsTo(a Agent, key string, info protocol.SessionInfo) bool {
	if a == nil || key == "" || len(key) > 512 || info.SessionKey != key || (info.Tool != "" && info.Tool != a.ID()) {
		return false
	}
	prefix, id, ok := strings.Cut(key, ":")
	if !ok || prefix != a.ID() || id == "" || id == "__salcara_start_v1__" || strings.HasPrefix(id, "__salcara_start_v1__:") {
		return false
	}
	for _, r := range key {
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return false
		}
	}
	return true
}

// ProjectSessionInfo removes claims that belong to a different executor or
// capability while preserving the original history Client/related metadata.
func ProjectSessionInfo(info protocol.SessionInfo, id string, caps protocol.AgentCapabilities) protocol.SessionInfo {
	info.Tool, info.ControlSurface, info.ControlExpiresAt = id, "cli", 0
	info.Controllable = info.Controllable && caps.Send
	info.LatestTurnRevert = info.LatestTurnRevert && caps.LatestTurnRevert && caps.Send && caps.Read
	return info
}

// ProjectEventMedia applies the same adapter media boundary to snapshot and
// live events before hubclient converts image bytes/paths into opaque refs.
func ProjectEventMedia(e *protocol.Event, caps protocol.AgentCapabilities) {
	if e == nil || caps.ImageOutput || e.Role == "user" && caps.ImageInput && caps.Attachments {
		return
	}
	e.ImageData, e.Attachments = nil, nil
	if e.Type != "tool" {
		return
	}
	if e.Kind == "image_view" || e.Kind == "image_generation" || e.Kind == "read" && strings.HasPrefix(e.Detail, "image-base64:") {
		e.Detail = ""
		return
	}
	if e.Kind == "read" && filepath.IsAbs(e.Detail) {
		switch strings.ToLower(filepath.Ext(e.Detail)) {
		case ".png", ".jpg", ".jpeg", ".webp", ".gif", ".bmp", ".avif":
			e.Detail = ""
		}
	}
}

// Watcher is an optional live-event hook for an extension adapter. The core
// still uses the common Sink; adapters only own how their native source is
// observed.
type Watcher interface {
	Watch(context.Context)
}

// EventSinkBinder lets the manager attach an adapter-owned, namespace-checked
// live sink. The adapter must replace its sink safely; a nil sink detaches it.
// Snapshot-only adapters need not implement this. Merely implementing Watcher
// does not grant a raw path to the common transport.
type EventSinkBinder interface {
	BindEventSink(Sink)
}

// ApprovalResponse keeps question answers separate from approval feedback. API
// keys and other credentials are never supplied through this contract.
type ApprovalResponse struct {
	Decision    string              `json:"decision"`
	Message     string              `json:"message,omitempty"`
	Answers     map[string][]string `json:"answers,omitempty"`
	OperationID string              `json:"operationId,omitempty"`
	By          string              `json:"-"` // trusted dispatch origin, never a submitted wire field
}

// TypedApprovalResponder preserves uncertain delivery instead of reducing an
// accepted-but-unacknowledged native approval to the legacy bool rejection.
type TypedApprovalResponder interface {
	RespondForSessionResult(context.Context, string, string, ApprovalResponse) (bool, error)
}

func RespondForSessionResult(ctx context.Context, agent Agent, sessionKey, approvalID string, response ApprovalResponse) (bool, error) {
	if agent == nil || sessionKey == "" {
		return false, nil
	}
	if responder, ok := agent.(TypedApprovalResponder); ok {
		return responder.RespondForSessionResult(ctx, sessionKey, approvalID, response)
	}
	return RespondForSession(ctx, agent, sessionKey, approvalID, response), nil
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

// Remote approvals require a scope-aware adapter; legacy ID-only responders
// remain usable by the local console but cannot silently discard phone scope.
func RespondForSession(ctx context.Context, agent Agent, sessionKey, approvalID string, response ApprovalResponse) bool {
	if agent == nil || sessionKey == "" {
		return false
	}
	if responder, ok := agent.(interface {
		RespondForSession(context.Context, string, string, ApprovalResponse) bool
	}); ok {
		return responder.RespondForSession(ctx, sessionKey, approvalID, response)
	}
	return false
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
