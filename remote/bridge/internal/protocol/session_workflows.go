package protocol

type SessionGoal struct {
	ThreadID        string `json:"threadId"`
	Objective       string `json:"objective"`
	Status          string `json:"status"` // active | paused | blocked | usageLimited | budgetLimited | complete
	TokenBudget     *int64 `json:"tokenBudget"`
	TokensUsed      int64  `json:"tokensUsed"`
	TimeUsedSeconds int64  `json:"timeUsedSeconds"`
	CreatedAt       int64  `json:"createdAt"`
	UpdatedAt       int64  `json:"updatedAt"`
}

type SessionWorkflows struct {
	SessionKey        string       `json:"sessionKey"`
	Revision          string       `json:"revision"`
	CanReview         bool         `json:"canReview"`
	GoalSupported     bool         `json:"goalSupported"`
	Goal              *SessionGoal `json:"goal,omitempty"`
	GoalActions       []string     `json:"goalActions"` // set | pause | resume | clear
	UnavailableReason string       `json:"unavailableReason,omitempty"`
}

type SessionReviewTarget struct {
	Type         string `json:"type"` // uncommittedChanges | baseBranch | commit | custom
	Branch       string `json:"branch,omitempty"`
	SHA          string `json:"sha,omitempty"`
	Instructions string `json:"instructions,omitempty"`
}

type SessionWorkflowRequest struct {
	Kind             string               `json:"kind"` // review | goal.set | goal.pause | goal.resume | goal.clear
	ExpectedRevision string               `json:"expectedRevision"`
	OperationID      string               `json:"operationId"`
	Target           *SessionReviewTarget `json:"target,omitempty"`
	Objective        string               `json:"objective,omitempty"`
	TokenBudget      *int64               `json:"tokenBudget,omitempty"`
}

// A review acknowledgement starts a native turn; it is not a completed review.
// A goal acknowledgement confirms goal metadata, not completion of its work.
type SessionWorkflowResult struct {
	SessionKey  string       `json:"sessionKey"`
	OperationID string       `json:"operationId"`
	Kind        string       `json:"kind"`
	State       string       `json:"state"` // requested | completed | unconfirmed
	TurnID      string       `json:"turnId,omitempty"`
	Goal        *SessionGoal `json:"goal,omitempty"`
	Error       string       `json:"error,omitempty"`
}
