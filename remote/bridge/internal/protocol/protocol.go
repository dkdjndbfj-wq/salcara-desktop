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
}

// RemoteStationSwitch is intentionally represented as a normal command on the
// existing authenticated phone channel. The phone supplies only a saved
// station's public URL/device id and an opaque API handle; the desktop resolves
// the device secret and local vault entry itself.

type SessionInfo struct {
	SessionKey   string `json:"sessionKey"`
	Tool         string `json:"tool"`
	Client       string `json:"client"`
	SessionScope string `json:"sessionScope,omitempty"` // verified desktop-chat | desktop-cowork history category
	// Client is the history's origin; ControlSurface identifies the actual executor.
	ControlSurface   string `json:"controlSurface,omitempty"`   // cli | desktop | read-only
	ControlExpiresAt int64  `json:"controlExpiresAt,omitempty"` // explicitly approved native desktop lease, epoch ms
	Title            string `json:"title"`
	Cwd              string `json:"cwd"`
	UpdatedAt        int64  `json:"updatedAt"`
	Status           string `json:"status"` // running | idle | waiting_approval | failed
	Controllable     bool   `json:"controllable"`
	Model            string `json:"model,omitempty"`
	ParentSessionKey string `json:"parentSessionKey,omitempty"`
	PinnedIndex      int    `json:"pinnedIndex,omitempty"`  // native sidebar's one-based pinned order
	SidebarIndex     int    `json:"sidebarIndex,omitempty"` // native directory's one-based order
}

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

// Event is one timeline item. Only the fields relevant to Type are set.
type Event struct {
	Seq        int64        `json:"seq,omitempty"`
	DeviceID   string       `json:"deviceId,omitempty"`
	SessionKey string       `json:"sessionKey"`
	Tool       string       `json:"tool"`
	TS         int64        `json:"ts"`
	Type       string       `json:"type"` // session.updated | message | reasoning | tool | approval.request | approval.resolved | turn | notice
	Session    *SessionInfo `json:"session,omitempty"`

	ID         string `json:"id,omitempty"`
	Role       string `json:"role,omitempty"`
	Text       string `json:"text,omitempty"`
	Final      bool   `json:"final,omitempty"`
	TurnID     string `json:"turnId,omitempty"`
	DurationMS int64  `json:"durationMs,omitempty"` // native duration, not inferred from request timestamps

	Kind             string   `json:"kind,omitempty"`
	Title            string   `json:"title,omitempty"`
	Detail           string   `json:"detail,omitempty"`
	Status           string   `json:"status,omitempty"`
	Output           string   `json:"output,omitempty"`
	Diff             string   `json:"diff,omitempty"`
	ExitCode         *int     `json:"exitCode,omitempty"`
	Cwd              string   `json:"cwd,omitempty"`
	ParentID         string   `json:"parentId,omitempty"`
	ChildSessionKeys []string `json:"childSessionKeys,omitempty"`

	ApprovalID   string     `json:"approvalId,omitempty"`
	Decision     string     `json:"decision,omitempty"`
	By           string     `json:"by,omitempty"`
	Questions    []Question `json:"questions,omitempty"`
	QuestionMode string     `json:"questionMode,omitempty"`
	ExpiresAt    int64      `json:"expiresAt,omitempty"`
	// Only a verified, synchronous native hook owns these approvals.
	ApprovalTransport string `json:"approvalTransport,omitempty"`

	Error string `json:"error,omitempty"`
	Usage *Usage `json:"usage,omitempty"`
	Level string `json:"level,omitempty"`
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
	CommandID string         `json:"commandId"`
	DeviceID  string         `json:"deviceId"`
	Command   map[string]any `json:"command"`
	BindingID string         `json:"bindingId,omitempty"`
	PhoneHash string         `json:"phoneHash,omitempty"`
	Phone     bool           `json:"phone,omitempty"`
}

type Reply struct {
	DeviceID  string `json:"deviceId"`
	CommandID string `json:"commandId"`
	OK        bool   `json:"ok"`
	Result    any    `json:"result,omitempty"`
	Error     string `json:"error,omitempty"`
}
