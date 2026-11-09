// Package protocol holds the wire types shared by the bridge, the hub and the phone app (docs/PROTOCOL.md).
package protocol

type Tool struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Version   string `json:"version,omitempty"`
}

type Project struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

type Device struct {
	DeviceID string    `json:"deviceId"`
	Name     string    `json:"name"`
	OS       string    `json:"os"`
	Version  string    `json:"version"`
	Tools    []Tool    `json:"tools"`
	Projects []Project `json:"projects"`
	// Capabilities are protocol-level guarantees of this Bridge process, not
	// installed-agent features.  They let a Hub fail closed for policies that
	// require a source-guarded execution path.
	Capabilities []string `json:"capabilities,omitempty"`
}

// RemoteStationSwitch is intentionally represented as a normal command on the
// existing authenticated phone channel. The phone supplies only a saved
// station's public URL/device id and an opaque API handle; the desktop resolves
// the device secret and local vault entry itself.

type SessionInfo struct {
	SessionKey    string        `json:"sessionKey"`
	Tool          string        `json:"tool"`
	Client        string        `json:"client"`
	WorkspaceMode WorkspaceMode `json:"workspaceMode,omitempty"` // code | work; an organization label, never a filesystem grant
	SessionScope  string        `json:"sessionScope,omitempty"`  // verified desktop-chat | desktop-cowork history category
	// Client is the history's origin; ControlSurface identifies the actual executor.
	ControlSurface     string `json:"controlSurface,omitempty"`   // cli | desktop | read-only
	ControlExpiresAt   int64  `json:"controlExpiresAt,omitempty"` // explicitly approved native desktop lease, epoch ms
	Title              string `json:"title"`
	Archived           bool   `json:"archived,omitempty"`
	Cwd                string `json:"cwd"`
	Projectless        bool   `json:"projectless,omitempty"` // Bridge-owned independent workspace; never a cwd authorization
	UpdatedAt          int64  `json:"updatedAt"`
	Status             string `json:"status"` // running | idle | waiting_approval | failed | unknown
	Controllable       bool   `json:"controllable"`
	LatestTurnRevert   bool   `json:"latestTurnRevert,omitempty"`
	Model              string `json:"model,omitempty"`
	PermissionMode     string `json:"permissionMode,omitempty"`     // confirmed runtime mode, absent means follow host
	CollaborationMode  string `json:"collaborationMode,omitempty"`  // confirmed runtime mode; absent means unknown/follow host
	ApprovalMode       string `json:"approvalMode,omitempty"`       // Claude worker launch policy, separate from Codex modes
	ActiveTurnID       string `json:"activeTurnId,omitempty"`       // observed live turn, never guessed from status
	LastTerminalTurnID string `json:"lastTerminalTurnId,omitempty"` // latest explicitly observed terminal turn
	ParentSessionKey   string `json:"parentSessionKey,omitempty"`
	// These are reported history metadata, never a grant to control a related
	// session. A host without these fields leaves them empty.
	AgentNickname        string `json:"agentNickname,omitempty"`
	AgentRole            string `json:"agentRole,omitempty"`
	ForkedFromSessionKey string `json:"forkedFromSessionKey,omitempty"`
	PinnedIndex          int    `json:"pinnedIndex,omitempty"`  // native sidebar's one-based pinned order
	SidebarIndex         int    `json:"sidebarIndex,omitempty"` // native directory's one-based order
}

// WorkspaceMode separates Code and Work conversations in the phone directory.
// It does not alter the Bridge's project-directory admission rules.
type WorkspaceMode string

const (
	WorkspaceModeCode WorkspaceMode = "code"
	WorkspaceModeWork WorkspaceMode = "work"
)

// ModelCapability contains reported metadata only. Missing fields are unknown;
// a listed name is not proof of reasoning, tools, image or context support.
type ModelCapability struct {
	Source           string   `json:"source"` // codex-model-list | relay-model-list | unknown
	ReasoningKnown   bool     `json:"reasoningKnown"`
	ReasoningEfforts []string `json:"reasoningEfforts"`
	InputModalities  []string `json:"inputModalities,omitempty"`
}
type ModelCatalog struct {
	Models            []string                   `json:"models"`
	ModelCapabilities map[string]ModelCapability `json:"modelCapabilities"`
}

// Feature entries are reported by the selected adapter for an admitted cwd.
// A returned path is a catalog handle, not permission to read an arbitrary file.
type SkillReference struct {
	Name        string `json:"name"`
	Path        string `json:"path,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
	BrandColor  string `json:"brandColor,omitempty"`
	IconSmall   string `json:"iconSmall,omitempty"`
}

type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Path        string `json:"path,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
	BrandColor  string `json:"brandColor,omitempty"`
	IconSmall   string `json:"iconSmall,omitempty"`
}

type AgentFeatures struct {
	Revision           string   `json:"revision,omitempty"`
	CollaborationModes []string `json:"collaborationModes"`
	Skills             []Skill  `json:"skills"`
	SkillsSupported    bool     `json:"skillsSupported"`
}

// AgentCapabilities describes the normalized remote surface of one adapter.
// It is deliberately separate from a provider/model catalogue: a model name
// cannot prove that an adapter can list history, answer questions, or retain
// image output. Missing fields are false/unknown and must not be inferred by
// the phone.
type AgentCapabilities struct {
	LatestTurnRevert       bool     `json:"latestTurnRevert,omitempty"`
	SideQuestion           bool     `json:"sideQuestion,omitempty"`
	SessionWorkflows       bool     `json:"sessionWorkflows,omitempty"`
	SessionActions         bool     `json:"sessionActions,omitempty"`
	ArchivedSessions       bool     `json:"archivedSessions,omitempty"`
	ContextReferences      bool     `json:"contextReferences,omitempty"`
	ContextAnnotations     bool     `json:"contextAnnotations,omitempty"`
	ContextDiffAnnotations bool     `json:"contextDiffAnnotations,omitempty"`
	Context                bool     `json:"context,omitempty"`
	Compact                bool     `json:"compact,omitempty"`
	Features               bool     `json:"features,omitempty"` // cwd-scoped native Plan/Skill discovery and exact turn transport
	List                   bool     `json:"list"`
	Read                   bool     `json:"read"`
	Send                   bool     `json:"send"`
	Interrupt              bool     `json:"interrupt"`
	PreciseInterrupt       bool     `json:"preciseInterrupt,omitempty"`
	Continue               bool     `json:"continue,omitempty"`
	Steer                  bool     `json:"steer,omitempty"`
	Approval               bool     `json:"approval"`
	Questions              bool     `json:"questions"`
	Subagents              bool     `json:"subagents"`
	Attachments            bool     `json:"attachments"`
	ImageInput             bool     `json:"imageInput"`
	ImageOutput            bool     `json:"imageOutput"`
	ModelOverride          bool     `json:"modelOverride"`
	StartModelOverride     bool     `json:"startModelOverride,omitempty"` // model choice on creation only; existing sessions may pin their model
	PermissionModes        []string `json:"permissionModes,omitempty"`    // adapter protocol support, not an account entitlement
	ApprovalModes          []string `json:"approvalModes,omitempty"`      // Claude v1 next-turn native mode switching
}

// AgentDescriptor is the adapter-owned public identity used by the
// aggregation layer. It contains no executable path, provider URL, API key,
// account id, or local filesystem data.
type AgentDescriptor struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Tool string `json:"tool"`
	// APIFamily is the stable routing family used by the Bridge API directory.
	// It is intentionally separate from Tool: multiple adapters may share one
	// provider wire family (for example a desktop adapter and its CLI worker),
	// while an adapter may also expose a non-provider tool id such as "hermes".
	// Empty means Tool for backwards-compatible adapters.
	APIFamily      string            `json:"apiFamily,omitempty"`
	ControlSurface string            `json:"controlSurface,omitempty"`
	Capabilities   AgentCapabilities `json:"capabilities"`
}

func ValidModelEffort(effort string) bool {
	switch effort {
	case "", "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
		return true
	default:
		return false
	}
}

func UnknownModelCapabilities(models []string, source string) map[string]ModelCapability {
	result := map[string]ModelCapability{}
	for _, id := range models {
		result[id] = ModelCapability{Source: source}
	}
	return result
}

// MessageOrigin carries display provenance. Native delegation may carry an
// explicitly unverified navigation hint. user_cross_task is generated only by
// the Bridge after user-authorized pair admission and exact-ID receipt storage.
// Neither is permission to send another message; every send needs a fresh ticket.
type MessageOrigin struct {
	Kind             string `json:"kind"`
	SourceSessionKey string `json:"sourceSessionKey,omitempty"`
	SourceTitle      string `json:"sourceTitle,omitempty"`
	TargetSessionKey string `json:"targetSessionKey,omitempty"`
	ReceiptID        string `json:"receiptId,omitempty"`
	Verification     string `json:"verification,omitempty"`
}

// BrowserNavigation is a successful, explicit browser tool navigation. It is
// display metadata, not permission to execute a tool or share a desktop cookie.
// PageID, when available, is an opaque server-scoped native tab/page identity.
type BrowserNavigation struct {
	Action string `json:"action"` // open | navigate
	URL    string `json:"url"`
	PageID string `json:"pageId,omitempty"`
}

// ComputerActivity describes an observed provider call, not a control lease.
// Images use the event's existing bounded attachment transport.
type ComputerActivity struct {
	Version  int    `json:"version"`
	Surface  string `json:"surface"` // computer | browser
	Action   string `json:"action"`
	Provider string `json:"provider"`
}

// Event is a timeline item or a features.changed runtime control signal. The
// latter has no session or payload and invalidates only its device/tool catalog.
type Event struct {
	Seq        int64        `json:"seq,omitempty"`
	DeviceID   string       `json:"deviceId,omitempty"`
	SessionKey string       `json:"sessionKey"`
	Tool       string       `json:"tool"`
	TS         int64        `json:"ts"`
	Type       string       `json:"type"` // session.updated | message | reasoning | tool | approval.request | approval.resolved | turn | notice | features.changed
	Session    *SessionInfo `json:"session,omitempty"`

	ID          string `json:"id,omitempty"`
	OperationID string `json:"operationId,omitempty"`
	Role        string `json:"role,omitempty"`
	// ClientID is Codex's reported message client, not an authenticated sender
	// task identity. Receivers must never derive a source thread from its value.
	ClientID      string          `json:"clientId,omitempty"`
	MessageOrigin *MessageOrigin  `json:"messageOrigin,omitempty"`
	MessagePhase  string          `json:"messagePhase,omitempty"` // commentary | final_answer, when reported by Codex
	Text          string          `json:"text,omitempty"`
	Final         bool            `json:"final,omitempty"`
	Attachments   []AttachmentRef `json:"attachments,omitempty"`
	// Skills are exact user-input references from native structured content or
	// a Bridge-owned send receipt. Text resembling /name, $name or @name is not
	// sufficient. A path is a catalog identity, never permission to read a file.
	Skills            []SkillReference   `json:"skills,omitempty"`
	ContextReferences []ContextReference `json:"contextReferences,omitempty"`
	// ImageData is an in-process handoff from an agent adapter to hubclient.
	// It is always converted to opaque AttachmentRef values before an event is
	// serialized; keeping it out of JSON prevents raw image bytes crossing the
	// Hub boundary if a caller forgets to decorate an event.
	ImageData  []string `json:"-"`
	TurnID     string   `json:"turnId,omitempty"`
	DurationMS int64    `json:"durationMs,omitempty"` // native duration, not inferred from request timestamps

	// Kind is a stable tool taxonomy. In addition to the historical read kind,
	// image_view and image_generation identify Codex image events; receivers
	// must preserve unknown kinds and still render their detail/attachments.
	Kind             string             `json:"kind,omitempty"`
	PlanKind         string             `json:"planKind,omitempty"` // proposal for a native plan item; absent means progress/checklist
	Title            string             `json:"title,omitempty"`
	Detail           string             `json:"detail,omitempty"`
	Status           string             `json:"status,omitempty"`
	Output           string             `json:"output,omitempty"`
	OutputFinal      bool               `json:"outputFinal,omitempty"` // authoritative tool output, including an explicit empty result
	Browser          *BrowserNavigation `json:"browser,omitempty"`
	Computer         *ComputerActivity  `json:"computer,omitempty"`
	Diff             string             `json:"diff,omitempty"`
	ExitCode         *int               `json:"exitCode,omitempty"`
	Cwd              string             `json:"cwd,omitempty"`
	ParentID         string             `json:"parentId,omitempty"`
	ChildSessionKeys []string           `json:"childSessionKeys,omitempty"`
	ApprovalID       string             `json:"approvalId,omitempty"`
	Decision         string             `json:"decision,omitempty"`
	By               string             `json:"by,omitempty"`
	Questions        []Question         `json:"questions,omitempty"`
	QuestionMode     string             `json:"questionMode,omitempty"`
	ExpiresAt        int64              `json:"expiresAt,omitempty"`
	// Only a verified, synchronous native hook owns these approvals.
	ApprovalTransport string `json:"approvalTransport,omitempty"`
	// Additive adapter contract. Missing/unknown versions must not imply a
	// remembered grant; an approval response is still validated by its owner.
	ApprovalOptions *ApprovalOptions `json:"approvalOptions,omitempty"`

	Error      string          `json:"error,omitempty"`
	ErrorCode  string          `json:"errorCode,omitempty"`  // stable execution condition, e.g. model_capacity
	RetryAt    int64           `json:"retryAt,omitempty"`    // explicit provider reset time, Unix milliseconds; usage/rate limits only
	Retrying   bool            `json:"retrying,omitempty"`   // provider explicitly reports an active retry
	NoticeKind string          `json:"noticeKind,omitempty"` // typed native lifecycle notice, e.g. context_compaction
	Progress   bool            `json:"progress,omitempty"`   // typed notice is still in progress
	Usage      *Usage          `json:"usage,omitempty"`
	Context    *SessionContext `json:"context,omitempty"`
	Level      string          `json:"level,omitempty"`
}

type ApprovalOptions struct {
	Version    int      `json:"version"`
	Decisions  []string `json:"decisions"`            // allow | allow_session | deny
	DenyEffect string   `json:"denyEffect,omitempty"` // cancel_turn when native only offers cancel, not decline
}

// AttachmentRef is a bounded, opaque reference to bytes held by the Bridge.
// The phone never receives an arbitrary filesystem path; it asks for chunks
// using the id and the Bridge checks the session binding before reading.
type AttachmentRef struct {
	ID     string `json:"id"`
	Mime   string `json:"mime"`
	Name   string `json:"name,omitempty"`
	Size   int64  `json:"size,omitempty"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

// Question contains only the bounded, actionable form sent to a paired phone.
type Question struct {
	ID                 string           `json:"id"`
	Header             string           `json:"header,omitempty"`
	Question           string           `json:"question"`
	Options            []QuestionOption `json:"options,omitempty"`
	MultiSelect        bool             `json:"multiSelect,omitempty"`
	AllowCustom        bool             `json:"allowCustom,omitempty"`
	Required           bool             `json:"required,omitempty"`
	InputType          string           `json:"inputType,omitempty"`
	Min                *float64         `json:"min,omitempty"`
	Max                *float64         `json:"max,omitempty"`
	PreserveWhitespace bool             `json:"preserveWhitespace,omitempty"`
	AllowEmpty         bool             `json:"allowEmpty,omitempty"`
}

type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type Usage struct {
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
	CostUSD      float64 `json:"costUsd,omitempty"`
}

// Command is what the phone asks the bridge to do (type + params kept raw for the handler).
type CommandEnvelope struct {
	CommandID  string         `json:"commandId"`
	DeviceID   string         `json:"deviceId"`
	Command    map[string]any `json:"command"`
	BindingID  string         `json:"bindingId,omitempty"`
	PhoneHash  string         `json:"phoneHash,omitempty"`
	EventScope string         `json:"eventScope,omitempty"`
	Phone      bool           `json:"phone,omitempty"`
	// Source is generated by the Hub after validating its source policy.  It
	// contains only non-secret provenance; a phone command cannot inject it.
	Source *ExecutionSource `json:"source,omitempty"`
}

// ExecutionSource is the non-secret source contract attached by a compatible
// Hub to an execution envelope.  Restricted mode is intentionally fail-closed
// when this contract is absent or does not match the selected local account.
type ExecutionSource struct {
	Station          *StationPolicy `json:"station,omitempty"`
	APIBase          string         `json:"apiBase,omitempty"`
	Mode             string         `json:"mode"` // open | restricted
	PolicyRevision   string         `json:"policyRevision"`
	Issuer           string         `json:"issuer,omitempty"`
	SourceType       string         `json:"sourceType,omitempty"`
	CredentialHandle string         `json:"credentialHandle,omitempty"`
	Origin           string         `json:"origin,omitempty"`
	ConfigRevision   string         `json:"configRevision,omitempty"`
	Agent            string         `json:"agent,omitempty"`
	ControlSurface   string         `json:"controlSurface,omitempty"`
	Model            string         `json:"model,omitempty"`
}

// StationPolicy is supplied by the connected Hub, never by phone command JSON.
// It identifies upstream stations independently of individual API credentials.
type StationPolicy struct {
	Version               int      `json:"version"`
	APIBases              []string `json:"apiBases"`
	ExternalAPIBases      []string `json:"externalApiBases,omitempty"`
	ReverseProxyAPIBases  []string `json:"reverseProxyApiBases,omitempty"`
	AllowOfficialAccounts bool     `json:"allowOfficialAccounts"`
	// AllowAllAccounts is set only by self-hosted Hubs in open mode with no APIBases
	// configured. It bypasses the per-account origin check in StationAllowsAccount,
	// which would otherwise block every api_key/reverse_proxy account when APIBases
	// is empty. The official cloud Hub never sets this field, so it cannot be used
	// to escalate privileges on a hosted station.
	AllowAllAccounts bool `json:"allowAllAccounts,omitempty"`
}

type Reply struct {
	DeviceID  string `json:"deviceId"`
	CommandID string `json:"commandId"`
	OK        bool   `json:"ok"`
	Result    any    `json:"result,omitempty"`
	Error     string `json:"error,omitempty"`
	Code      string `json:"code,omitempty"`
}
