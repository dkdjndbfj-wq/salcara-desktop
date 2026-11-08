package apiservice

import (
	"net/http"
	"sort"
	"time"

	"salcara/bridge/internal/accounthealth"
	"salcara/bridge/internal/apierror"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/gateway"
	"salcara/bridge/internal/usageledger"
)

// initHealth creates the Health tracker.  Called from New().
func (s *Service) initHealth() {
	s.Health = accounthealth.NewTracker()
}

// RecordOutcome classifies the upstream response and records it in the Health
// tracker.  Call once per completed inference request (not image sub-requests).
//
//	accountID — config.LocalAccount.ID
//	status    — HTTP status code written to the client (from statusWriter)
//	resp      — the raw upstream *http.Response, may be nil on network error
//	body      — the full response body already read into memory; nil for streaming
//	latency   — end-to-end request wall time
func (s *Service) RecordOutcome(accountID string, status int, resp *http.Response, body []byte, latency time.Duration) {
	if s.Health == nil || accountID == "" {
		return
	}
	var header http.Header
	if resp != nil {
		header = resp.Header
	} else {
		header = http.Header{}
	}
	result := apierror.Classify(status, header, body, latency)
	s.Health.Record(accountID, result)
}

// RecordStreamOutcome is the streaming variant. Call it after the last SSE
// chunk has been drained and the token counter has been read.
func (s *Service) RecordStreamOutcome(accountID string, status int, resp *http.Response, tokens int, latency time.Duration) {
	if s.Health == nil || accountID == "" {
		return
	}
	var header http.Header
	if resp != nil {
		header = resp.Header
	} else {
		header = http.Header{}
	}
	result := apierror.Classify(status, header, nil, latency)
	apierror.ClassifyStreamEnd(&result, tokens)
	s.Health.Record(accountID, result)
}

// HealthyAccounts excludes cooling credentials. An entirely cooling pool
// remains unavailable until recovery; it never bypasses a provider reset time.
func (s *Service) HealthyAccounts(candidates []config.LocalAccount) []config.LocalAccount {
	if s.Health == nil {
		return candidates
	}
	var healthy []config.LocalAccount
	for _, a := range candidates {
		if scope, valid := gateway.CredentialBudgetIdentity(a); valid {
			s.Health.Bind(a.ID, scope)
		}
		if s.Health.Available(a.ID) {
			healthy = append(healthy, a)
		}
	}
	return healthy
}

// Consume each real attempt, including streaming failure with HTTP 200. Local
// admission and client cancellation never blame the provider or its credential.
func (s *Service) recordExecutionOutcome(account config.LocalAccount, o usageledger.Observation) {
	if s.Health == nil || s.Config == nil || account.ID == "" || len(o.Attempts) == 0 {
		return
	}
	current, exists := s.Config().LocalAccount(account.ID)
	scope, valid := gateway.CredentialBudgetIdentity(account)
	currentScope, currentValid := gateway.CredentialBudgetIdentity(current)
	if !exists || !valid || !currentValid || scope != currentScope {
		return
	}
	attempt := o.Attempts[0]
	if attempt.State == "cancelled" || attempt.State == "rejected" || attempt.Reason == "cancelled" {
		return
	}
	result := accounthealth.Result{Status: attempt.UpstreamHTTPStatus, Latency: time.Duration(attempt.DurationMS) * time.Millisecond, RetryAfter: time.Duration(attempt.RetryAfterMS) * time.Millisecond}
	switch attempt.Reason {
	case "rate_limited":
		result.Kind = accounthealth.RateLimit
	case "auth_rejected":
		result.Kind = accounthealth.Fatal
	case "model_unavailable":
		result.Kind = accounthealth.ModelUnavailable
		result.RetryAfter = 0 // capacity of one model is not an account cooldown
	default:
		if attempt.State == "failed" || attempt.State == "incomplete" || attempt.State == "interrupted" || attempt.State == "unknown" {
			result.Kind = accounthealth.NetworkRetry
		} else {
			result.Kind = accounthealth.OK
		}
	}
	if o.Tokens.Total != nil {
		result.Tokens = int(*o.Tokens.Total)
	}
	s.Health.RecordFor(account.ID, scope, result)
}

// ScoreAccounts returns candidates sorted by descending health score so that
// the pool's selection logic sees the best account first.  The original slice
// is not mutated.
func (s *Service) ScoreAccounts(candidates []config.LocalAccount) []config.LocalAccount {
	if s.Health == nil || len(candidates) <= 1 {
		return candidates
	}
	sorted := make([]config.LocalAccount, len(candidates))
	copy(sorted, candidates)
	sort.SliceStable(sorted, func(i, j int) bool {
		si := s.Health.Score(sorted[i].ID)
		sj := s.Health.Score(sorted[j].ID)
		return si > sj
	})
	return sorted
}

// HealthSnapshot returns a dashboard-friendly snapshot of all tracked account
// health states.  Safe to call from the console API handler.
func (s *Service) HealthSnapshot() []accounthealth.AccountSnapshot {
	if s.Health == nil {
		return nil
	}
	return s.Health.Snapshot()
}
