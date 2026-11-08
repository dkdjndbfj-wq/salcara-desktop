package codexaccounts

import "time"

// ExecutionAvailability is a local credential-state snapshot. It contains no
// token or provider metadata and does not establish that an upstream accepts
// the credential. Unknown access expiry remains usable until upstream refusal.
type ExecutionAvailability struct {
	AccessUsable       bool
	RequiresRefresh    bool
	RefreshEligible    bool
	RefreshRecommended bool
	PendingSave        bool
	NeedsLogin         bool
	RefreshFailed      bool
}

// ExecutionAvailability inspects memory only. The expected identity must still
// match the selected account; neither a refresh nor a disk read occurs here.
func (s *Service) ExecutionAvailability(id string, identityEpoch uint64) (ExecutionAvailability, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ExecutionAvailability{}, ErrClosed
	}
	c, ok := s.vault.Accounts[id]
	if !ok {
		return ExecutionAvailability{}, ErrNotFound
	}
	if identityEpoch == 0 || c.IdentityEpoch != identityEpoch {
		return ExecutionAvailability{}, ErrConflict
	}
	if c.TokenSourceMode == "chatgpt_web_session" {
		return ExecutionAvailability{}, ErrConflict
	}
	now := s.now()
	usable := c.AccessToken != "" && (c.ExpiresAt.IsZero() || now.Before(c.ExpiresAt))
	pending := c.Status == StatusSaveFailed || s.pending[id].epoch != 0
	needsLogin := c.Status == StatusNeedsLogin || !usable && c.RefreshToken == ""
	refreshEligible := c.RefreshToken != "" && s.refresher != nil && !pending && !needsLogin
	return ExecutionAvailability{
		AccessUsable: usable, RequiresRefresh: !usable,
		RefreshEligible:    refreshEligible,
		RefreshRecommended: usable && refreshEligible && !c.ExpiresAt.IsZero() && c.ExpiresAt.Before(now.Add(time.Minute)),
		PendingSave:        pending, NeedsLogin: needsLogin, RefreshFailed: c.Status == StatusRefreshFailed,
	}, nil
}
