package protocol

// SessionActions is an observed native management surface, scoped to one
// session and runtime revision. An empty action list never grants a fallback
// to a slash command, local alias, or another executor.
type SessionActions struct {
	SessionKey        string   `json:"sessionKey"`
	Revision          string   `json:"revision"`
	Title             string   `json:"title"`
	Archived          bool     `json:"archived"`
	Actions           []string `json:"actions"` // rename | archive | unarchive | fork
	UnavailableReason string   `json:"unavailableReason,omitempty"`
}

type SessionActionRequest struct {
	Action           string `json:"action"`
	ExpectedRevision string `json:"expectedRevision"`
	OperationID      string `json:"operationId"`
	Name             string `json:"name,omitempty"`
}

// A completed response requires a confirmed native acknowledgement. A lost
// acknowledgement is unconfirmed and its operation must not be replayed.
type SessionActionResult struct {
	SessionKey  string       `json:"sessionKey"`
	OperationID string       `json:"operationId"`
	Action      string       `json:"action"`
	State       string       `json:"state"` // completed | unconfirmed
	Session     *SessionInfo `json:"session,omitempty"`
	Error       string       `json:"error,omitempty"`
}
