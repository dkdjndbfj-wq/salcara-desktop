// health_api.go — console API endpoints for the account health dashboard.
//
// Drop this file into internal/console/ alongside server.go.
//
// Wire these routes into Handler() in server.go inside the api mux block:
//   api.HandleFunc("GET /api/local/accounts/health",        s.handleAccountHealth)
//   api.HandleFunc("POST /api/local/accounts/health/reset", s.handleAccountHealthReset)

package console

import (
	"encoding/json"
	"net/http"
	"time"
)

// accountHealthEntry is the JSON shape returned by GET /api/local/accounts/health.
type accountHealthEntry struct {
	AccountID            string  `json:"accountId"`
	AccountName          string  `json:"accountName,omitempty"`
	Score                float64 `json:"score"`
	Available            bool    `json:"available"`
	CooldownRemainingSec int     `json:"cooldownRemainingSec,omitempty"`
	TotalTokens          int64   `json:"totalTokens"`
}

// handleAccountHealth returns the current health snapshot for all tracked accounts.
// GET /api/local/accounts/health
func (s *Server) handleAccountHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	svc := s.apiService
	if svc == nil || svc.Health == nil {
		writeJSON(w, []accountHealthEntry{})
		return
	}

	snap := svc.Health.Snapshot()
	now := time.Now()
	entries := make([]accountHealthEntry, 0, len(snap))

	for _, item := range snap {
		e := accountHealthEntry{
			AccountID:   item.AccountID,
			Score:       item.Score,
			Available:   svc.Health.Available(item.AccountID),
			TotalTokens: item.TotalTokens,
		}
		if !item.CoolUntil.IsZero() && now.Before(item.CoolUntil) {
			e.CooldownRemainingSec = int(time.Until(item.CoolUntil).Seconds())
		}
		// Annotate with the human-readable account name if available.
		if s.d.Store != nil {
			cfg := s.d.Store.Get()
			if acct, ok := cfg.LocalAccount(item.AccountID); ok {
				e.AccountName = acct.Name
			}
		}
		entries = append(entries, e)
	}
	writeJSON(w, entries)
}

// handleAccountHealthReset clears the cooldown for one account, making it
// immediately eligible for selection again. Useful after an operator fixes
// a credential or resolves a billing issue.
//
// POST /api/local/accounts/health/reset
// Body: { "accountId": "..." }
func (s *Server) handleAccountHealthReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		AccountID string `json:"accountId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.AccountID == "" {
		http.Error(w, "bad request: accountId required", http.StatusBadRequest)
		return
	}

	svc := s.apiService
	if svc != nil && svc.Health != nil {
		svc.Health.ClearCooldown(body.AccountID)
	}
	writeJSON(w, map[string]bool{"ok": true})
}
