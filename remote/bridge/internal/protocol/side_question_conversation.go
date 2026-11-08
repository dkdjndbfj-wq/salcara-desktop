package protocol

// Persistent side conversations retain history until an explicit deletion.
// Revision is a fresh execution authorization, never a persisted permission.
type SideQuestionConversationRequest struct {
	Action  string `json:"-"`
	ChainID string `json:"chainId"`
	Cursor  string `json:"cursor,omitempty"`
	SideQuestionRequest
}

type SideQuestionExchange struct {
	OperationID string            `json:"operationId"`
	Question    string            `json:"question"`
	Selection   *ContextReference `json:"selection,omitempty"`
	Answer      string            `json:"answer"`
	State       string            `json:"state"`
	Error       string            `json:"error,omitempty"`
	CreatedAt   int64             `json:"createdAt"`
}

type SideQuestionConversation struct {
	SideQuestionCatalog
	ChainID         string                 `json:"chainId"`
	LastOperationID string                 `json:"lastOperationId,omitempty"`
	Turns           []SideQuestionExchange `json:"turns"`
	NextCursor      string                 `json:"nextCursor,omitempty"`
	Deleted         bool                   `json:"deleted,omitempty"`
}
