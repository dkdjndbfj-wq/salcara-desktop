package protocol

// LatestTurnRevertResult only confirms native history removal. Local files are
// not reverted, and an unconfirmed request must never be sent a second time.
type LatestTurnRevertResult struct {
	Status      string       `json:"status"`
	SessionKey  string       `json:"sessionKey"`
	TurnID      string       `json:"turnId"`
	MessageID   string       `json:"messageId"`
	OperationID string       `json:"operationId"`
	Session     *SessionInfo `json:"session,omitempty"`
}

// InterruptResult reports a control fact, not a guessed task outcome. Requested
// means the host accepted an exact-turn interrupt; stopped requires an observed
// terminal state (and, for an owned CLI worker, confirmed OS exit). Adapters
// publish that terminal event before returning stopped; callers must not create
// a second timeline event from this control result.
type InterruptResult struct {
	Status string `json:"status"` // requested | stopped | already_finished | unconfirmed
	TurnID string `json:"turnId"`
}

// ContinueResult is an ordinary same-conversation turn admission. It does not
// imply an autonomous Goal was resumed, or that the admitted turn completed.
type ContinueResult struct {
	Accepted       bool   `json:"accepted"`
	SessionKey     string `json:"sessionKey"`
	PreviousTurnID string `json:"previousTurnId"`
	OperationID    string `json:"operationId"`
	TurnID         string `json:"turnId,omitempty"`
}

// SteerResult confirms admission to this exact running turn, not completion.
type SteerResult struct {
	Accepted    bool   `json:"accepted"`
	SessionKey  string `json:"sessionKey"`
	TurnID      string `json:"turnId"`
	OperationID string `json:"operationId"`
}
