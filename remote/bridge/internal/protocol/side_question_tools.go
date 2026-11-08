package protocol

// Tools are an explicit opt-in for a separate, ephemeral managed Codex worker.
// Missing metadata never enables execution in the ordinary read-only side chat.
type SideQuestionToolOptions struct {
	Version     int      `json:"version"`
	Permissions []string `json:"permissions"`
	Features    bool     `json:"features,omitempty"`
}

type SideQuestionToolRequest struct {
	Action            string           `json:"action"`
	ChainID           string           `json:"chainId"`
	ExpectedRevision  string           `json:"expectedRevision"`
	OperationID       string           `json:"operationId,omitempty"`
	Permission        string           `json:"permission,omitempty"`
	Question          string           `json:"question,omitempty"`
	Model             string           `json:"model,omitempty"`
	Effort            string           `json:"effort,omitempty"`
	CollaborationMode string           `json:"collaborationMode,omitempty"`
	Skills            []SkillReference `json:"skills,omitempty"`
	ExpectedTurnID    string           `json:"expectedTurnId,omitempty"`
	ApprovalID        string           `json:"approvalId,omitempty"`
	Decision          string           `json:"decision,omitempty"`
}

// Events and approvals are bounded to 128 and 16 records respectively. Answer
// is at most 64 KiB; Error at most 4096 bytes. Sequence fences out-of-order reads.
type SideQuestionToolState struct {
	SessionKey      string         `json:"sessionKey"`
	ChainID         string         `json:"chainId"`
	Revision        string         `json:"revision"`
	Permission      string         `json:"permission"`
	State           string         `json:"state"`
	TurnID          string         `json:"turnId,omitempty"`
	Answer          string         `json:"answer"`
	Events          []Event        `json:"events"`
	Approvals       []Event        `json:"approvals"`
	Error           string         `json:"error,omitempty"`
	CanSend         bool           `json:"canSend"`
	ExpiresAt       int64          `json:"expiresAt"`
	LastOperationID string         `json:"lastOperationId,omitempty"`
	Sequence        uint64         `json:"sequence"`
	Truncated       bool           `json:"truncated,omitempty"`
	Features        *AgentFeatures `json:"features,omitempty"`
}
