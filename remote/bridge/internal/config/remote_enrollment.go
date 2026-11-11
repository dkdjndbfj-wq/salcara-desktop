package config

// A pending identity survives a lost registration reply and restart without
// becoming an active connection. Neither an API key nor a grant is stored here.
// Submitted attempts may only be recovered using these exact credentials.
type RemoteEnrollmentAttempt struct {
	AccountID      string           `json:"accountId"`
	SourceRevision string           `json:"sourceRevision"`
	SourceProof    string           `json:"sourceProof"`
	RequestID      string           `json:"requestId"`
	Submitted      bool             `json:"submitted,omitempty"`
	Connection     RemoteConnection `json:"connection"`
}
