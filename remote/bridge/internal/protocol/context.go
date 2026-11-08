package protocol

// ContextSnapshot is a native observation, never cumulative billing usage.
// last_request describes Codex's latest reported request; runtime_estimate is
// Claude's summary estimate. Missing Snapshot/WindowTokens means unknown.
type ContextSnapshot struct {
	UsedTokens   int64  `json:"usedTokens"`
	WindowTokens *int64 `json:"windowTokens,omitempty"`
	Source       string `json:"source"`
	Basis        string `json:"basis"`
	WindowKind   string `json:"windowKind,omitempty"` // model | effective
	MeasuredAt   int64  `json:"measuredAt"`
	Estimated    bool   `json:"estimated,omitempty"`
	Model        string `json:"model,omitempty"`
}

// An accepted native RPC is not evidence that compaction completed. A lost
// connection after dispatch remains unconfirmed and must not be auto-replayed.
type ContextCompaction struct {
	OperationID string `json:"operationId,omitempty"`
	State       string `json:"state"` // requested | running | completed | failed | unconfirmed
	// Blocking describes current runtime occupancy, independently of the result.
	// An unconfirmed result can be idle; a completed result can still be draining.
	Blocking  bool   `json:"blocking"`
	StartedAt int64  `json:"startedAt"`
	UpdatedAt int64  `json:"updatedAt"`
	Error     string `json:"error,omitempty"`
}

type SessionContext struct {
	SessionKey        string             `json:"sessionKey"`
	Revision          string             `json:"revision"`
	Snapshot          *ContextSnapshot   `json:"snapshot,omitempty"`
	Compaction        *ContextCompaction `json:"compaction,omitempty"`
	CanCompact        bool               `json:"canCompact"`
	UnavailableReason string             `json:"unavailableReason,omitempty"`
}
