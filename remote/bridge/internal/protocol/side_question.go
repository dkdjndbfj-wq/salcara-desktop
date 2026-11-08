package protocol

// A side question is independent of the parent conversation and never appends
// a native turn. loadedSnapshot explicitly describes its bounded context.
type SideQuestionCatalog struct {
	SessionKey        string                    `json:"sessionKey"`
	Revision          string                    `json:"revision"`
	ContextScope      string                    `json:"contextScope"`
	ContextMessages   int                       `json:"contextMessages"`
	ContextTruncated  bool                      `json:"contextTruncated"`
	Available         bool                      `json:"available"`
	UnavailableReason string                    `json:"unavailableReason,omitempty"`
	InputOptions      *SideQuestionInputOptions `json:"inputOptions,omitempty"`
}

// Negotiated only for the ephemeral catalog/ask protocol. Permission is always
// read-only: these requests never gain tools or mutate a native parent task.
type SideQuestionInputOptions struct {
	Version      int                      `json:"version"`
	Model        bool                     `json:"model"`
	Effort       bool                     `json:"effort"`
	Images       bool                     `json:"images"`
	ReadOnly     bool                     `json:"readOnly"`
	DefaultModel string                   `json:"defaultModel"`
	Tools        *SideQuestionToolOptions `json:"tools,omitempty"`
}
type SideQuestionImage struct{ Mime, Data string }
type SideQuestionRequest struct {
	PreviousOperationID string            `json:"previousOperationId,omitempty"`
	Question            string            `json:"question"`
	Selection           *ContextReference `json:"selection,omitempty"`
	OperationID         string            `json:"operationId"`
	ExpectedRevision    string            `json:"expectedRevision"`
	Model               string            `json:"model,omitempty"`
	Effort              string            `json:"effort,omitempty"`
	Attachments         []string          `json:"attachments,omitempty"`
	// Images are resolved from admitted uploads by Bridge, never phone paths.
	Images []SideQuestionImage `json:"-"`
}
type SideQuestionResult struct {
	SessionKey       string `json:"sessionKey"`
	OperationID      string `json:"operationId"`
	State            string `json:"state"` // completed | unconfirmed
	Answer           string `json:"answer"`
	Usage            *Usage `json:"usage,omitempty"`
	ContextScope     string `json:"contextScope"`
	ContextMessages  int    `json:"contextMessages"`
	ContextTruncated bool   `json:"contextTruncated"`
	Error            string `json:"error,omitempty"`
}
