package usageledger

import "time"

// Attempt contains bounded metadata only. Append replaces raw account IDs with
// the same anonymous IDs used by records; no upstream error text is accepted.
type Attempt struct {
	AccountID          string `json:"accountId,omitempty"`
	AccountEpoch       uint64 `json:"accountEpoch,omitempty"`
	Model              string `json:"model,omitempty"`
	State              string `json:"state"`
	HTTPStatus         int    `json:"httpStatus"`
	UpstreamHTTPStatus int    `json:"upstreamHttpStatus,omitempty"`
	DurationMS         int64  `json:"durationMs"`
	FirstByteMS        *int64 `json:"firstByteMs,omitempty"`
	Reason             string `json:"reason,omitempty"`
	RetryAfterMS       int64  `json:"retryAfterMs,omitempty"`
}

const MaxAttempts = 3

func diagnosticTime(v *int64) *int64 {
	if v == nil || *v < 0 || *v > 48*time.Hour.Milliseconds() {
		return nil
	}
	n := *v
	return &n
}

func validRoutingReason(v string) bool {
	switch v {
	case "", "fixed_account", "pool_selected", "session_affinity", "failover":
		return true
	}
	return false
}
func validFailureStage(v string) bool {
	switch v {
	case "", "selection", "authorization", "connect", "headers", "stream", "upstream", "cancelled":
		return true
	}
	return false
}
func validAttemptReason(v string) bool {
	if validRoutingReason(v) {
		return true
	}
	switch v {
	case "rate_limited", "unavailable", "auth_rejected", "model_unavailable", "connect_failed", "headers_timeout", "stream_idle_timeout", "stream_interrupted", "cancelled", "unknown_result":
		return true
	}
	return false
}
func boundedDiagnosticDuration(v int64) int64 {
	if v < 0 {
		return 0
	}
	if v > 48*time.Hour.Milliseconds() {
		return 48 * time.Hour.Milliseconds()
	}
	return v
}
func diagnosticStatus(v int) int {
	if v < 100 || v > 599 {
		return 0
	}
	return v
}
func cloneAttempts(values []Attempt) []Attempt {
	if values == nil {
		return nil
	}
	out := append([]Attempt(nil), values...)
	for i := range out {
		out[i].FirstByteMS = diagnosticTime(out[i].FirstByteMS)
	}
	return out
}
func (l *Ledger) addDiagnostics(r *Record, o Observation) {
	r.FirstByteMS = diagnosticTime(o.FirstByteMS)
	if validRoutingReason(o.RoutingReason) {
		r.RoutingReason = o.RoutingReason
	}
	if validFailureStage(o.FailureStage) {
		r.FailureStage = o.FailureStage
	}
	for i, a := range o.Attempts {
		if i >= MaxAttempts {
			break
		}
		a.AccountID = l.anonymous("acct-", a.AccountID)
		if !ValidateModel(a.Model) {
			a.Model = ""
		}
		if !validState(a.State) {
			a.State = "unknown"
		}
		if !validAttemptReason(a.Reason) {
			a.Reason = ""
		}
		a.HTTPStatus, a.UpstreamHTTPStatus = diagnosticStatus(a.HTTPStatus), diagnosticStatus(a.UpstreamHTTPStatus)
		a.DurationMS, a.RetryAfterMS = boundedDiagnosticDuration(a.DurationMS), boundedDiagnosticDuration(a.RetryAfterMS)
		a.FirstByteMS = diagnosticTime(a.FirstByteMS)
		r.Attempts = append(r.Attempts, a)
	}
}
func validDiagnostics(r Record) bool {
	if !validRoutingReason(r.RoutingReason) || !validFailureStage(r.FailureStage) || len(r.Attempts) > MaxAttempts || r.FirstByteMS != nil && diagnosticTime(r.FirstByteMS) == nil {
		return false
	}
	for _, a := range r.Attempts {
		if !validAnonymous(a.AccountID, "acct-") || a.Model != "" && !ValidateModel(a.Model) || !validState(a.State) || !validAttemptReason(a.Reason) || a.HTTPStatus != diagnosticStatus(a.HTTPStatus) || a.UpstreamHTTPStatus != diagnosticStatus(a.UpstreamHTTPStatus) || a.DurationMS != boundedDiagnosticDuration(a.DurationMS) || a.RetryAfterMS != boundedDiagnosticDuration(a.RetryAfterMS) || a.FirstByteMS != nil && diagnosticTime(a.FirstByteMS) == nil {
			return false
		}
	}
	return true
}
