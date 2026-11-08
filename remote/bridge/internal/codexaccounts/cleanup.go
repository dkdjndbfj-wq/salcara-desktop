package codexaccounts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"
)

// CleanupIdentity freezes the local credential version the user reviewed.
// Epoch changes on login/rotation; IdentityEpoch changes on replacement.
type CleanupIdentity struct {
	ID            string `json:"id"`
	Epoch         uint64 `json:"epoch"`
	IdentityEpoch uint64 `json:"identityEpoch"`
	Revision      string `json:"revision,omitempty"`
}

// CleanupStatus is an explicit account/access observation, not a provider ban
// verdict. Network failures never qualify. Recoverable access/quota candidates
// must be explicitly selected.
type CleanupStatus struct {
	Eligible    bool   `json:"eligible"`
	Reason      string `json:"reason"`
	Recoverable bool   `json:"recoverable"`
}

type CleanupCandidate struct {
	CleanupIdentity
	Name        string `json:"name"`
	Reason      string `json:"reason"`
	Recoverable bool   `json:"recoverable"`
}
type CleanupSkipped struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}
type CleanupResult struct {
	Deleted []CleanupCandidate `json:"deleted"`
	Skipped []CleanupSkipped   `json:"skipped"`
}

// A receipt contains no credentials. It only allows an exact retry to finish
// removing an old config reference after the vault deletion already committed.
type cleanupReceipt struct {
	CleanupIdentity
	CreatedAt time.Time `json:"created_at"`
}

func validCleanupIdentity(item CleanupIdentity) bool {
	decoded, err := hex.DecodeString(item.ID)
	if err != nil || len(decoded) != 16 || hex.EncodeToString(decoded) != item.ID || item.Epoch == 0 || item.Epoch == ^uint64(0) || item.IdentityEpoch == 0 || item.IdentityEpoch > item.Epoch {
		return false
	}
	if item.Revision == "" {
		return true // Legacy clients retain only the original needs-login scope.
	}
	revision, err := hex.DecodeString(item.Revision)
	return err == nil && len(revision) == sha256.Size && hex.EncodeToString(revision) == item.Revision
}

func trimCleanupReceipts(receipts map[string]cleanupReceipt) {
	if len(receipts) <= MaxAccounts {
		return
	}
	ids := make([]string, 0, len(receipts))
	for id := range receipts {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := receipts[ids[i]], receipts[ids[j]]
		if a.CreatedAt.Equal(b.CreatedAt) {
			return ids[i] < ids[j]
		}
		return a.CreatedAt.Before(b.CreatedAt)
	})
	for _, id := range ids[:len(ids)-MaxAccounts] {
		delete(receipts, id)
	}
}

func (s *Service) cleanupEligibleLocked(c credential) bool {
	return s.cleanupStatusLocked(c) != nil
}

func (s *Service) cleanupBusyLocked(c credential) bool {
	if s.flights[c.ID] != nil || s.usageFlights[c.ID] != nil || s.cleanupUsageWork[c.ID] > 0 || s.subscriptionFlights[c.ID] != nil || s.subscriptionWork[c.ID] > 0 || s.resetFlights[c.ID] != nil || s.pending[c.ID].epoch != 0 || c.Status == StatusSaveFailed {
		return true
	}
	for _, login := range s.logins {
		if login.request.ReplaceID == c.ID && login.session.Status == "pending" {
			return true
		}
	}
	if reset, ok := s.vault.ResetRedemptions[c.ID]; ok && reset.IdentityEpoch == c.IdentityEpoch && (reset.State == resetStateConsuming || reset.State == resetStateUnknown) {
		return true
	}
	return false
}

func (s *Service) cleanupLegacyEligibleLocked(c credential) bool {
	if c.Status != StatusNeedsLogin || s.cleanupBusyLocked(c) {
		return false
	}
	// Missing refresh credentials can mark an access-only account needs_login
	// while its access token still works. Unknown expiry is not evidence of
	// expiry either. Preserve both cases even if an earlier preview allowed it.
	if c.AccessToken != "" && (c.ExpiresAt.IsZero() || c.ExpiresAt.After(s.now())) {
		return false
	}
	return true
}

func (s *Service) cleanupStatusLocked(c credential) *CleanupStatus {
	if s.cleanupBusyLocked(c) {
		return nil
	}
	status := func(reason string, recoverable bool) *CleanupStatus {
		return &CleanupStatus{Eligible: true, Reason: reason, Recoverable: recoverable}
	}
	if s.cleanupLegacyEligibleLocked(c) {
		return status("needs_login", false)
	}
	if entry, ok := s.usage[c.ID]; ok && entry.epoch == c.Epoch && entry.identityEpoch == c.IdentityEpoch {
		u := entry.snapshot
		if u.LastAttemptAt != nil && !u.LastAttemptAt.IsZero() && !u.LastAttemptAt.After(s.now()) && u.ErrorCode != "" {
			switch u.ErrorCode {
			case "workspace_revoked":
				return status("workspace_revoked", false)
			case "account_unavailable", "invalid_credential":
				return status("account_unavailable", false)
			case "needs_login":
				// A cancelled waiter or failed recovery leg can leave a first
				// 401 behind. Only finalized evidence for this exact credential
				// and observation can create a destructive cleanup candidate.
				if !entry.finalUnauthorized {
					return nil
				}
				return status("usage_unauthorized", false)
			case "permission_denied":
				return status("usage_forbidden", true)
			}
		}
		// Mere expiry/staleness of a successful observation proves no failure.
		// Only an authenticated, still-current overall limit can be exhausted;
		// a single model-specific limit does not invalidate the whole account.
		if u.Status == UsageStatusFresh && u.Verified && u.Source == UsageSourceOfficial && u.FreshUntil != nil && s.now().Before(*u.FreshUntil) && u.ErrorCode == "" {
			limited := u.RateLimit != nil && (u.RateLimit.Allowed != nil && !*u.RateLimit.Allowed || u.RateLimit.LimitReached != nil && *u.RateLimit.LimitReached)
			limited = limited || u.SpendControl != nil && u.SpendControl.Reached != nil && *u.SpendControl.Reached
			if limited {
				return status("quota_exhausted", true)
			}
		}
	}
	return nil
}

func (s *Service) cleanupRevisionLocked(c credential, status *CleanupStatus) string {
	entry := s.usage[c.ID]
	// No credentials enter this digest. A per-process nonce plus observation
	// generation rejects old confirmations even if a frozen clock yields an
	// identical subsequent failure; a restart must obtain a new preview.
	data, _ := json.Marshal(struct {
		Nonce                                           string
		ID                                              string
		Epoch, IdentityEpoch, NameRevision, Observation uint64
		Status                                          string
		ExpiresAt                                       time.Time
		HasAccess                                       bool
		Eligibility                                     *CleanupStatus
		Usage                                           UsageSnapshot
		FinalUnauthorized                               bool
	}{s.cleanupNonce, c.ID, c.Epoch, c.IdentityEpoch, c.NameRevision, s.cleanupObservations[c.ID], c.Status, c.ExpiresAt, c.AccessToken != "", status, entry.snapshot, entry.finalUnauthorized})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (s *Service) bumpCleanupObservationLocked(id string) {
	if _, exists := s.vault.Accounts[id]; !exists {
		delete(s.cleanupObservations, id)
		return
	}
	if s.cleanupObservations == nil {
		s.cleanupObservations = make(map[string]uint64)
	}
	s.cleanupObservations[id]++
}

func (s *Service) cleanupCandidateLocked(c credential) CleanupCandidate {
	status := s.cleanupStatusLocked(c)
	row := CleanupCandidate{CleanupIdentity: CleanupIdentity{ID: c.ID, Epoch: c.Epoch, IdentityEpoch: c.IdentityEpoch}, Name: c.account(s.now()).Name}
	if status != nil {
		row.Reason, row.Recoverable = status.Reason, status.Recoverable
		row.Revision = s.cleanupRevisionLocked(c, status)
	}
	return row
}

// CleanupPreview is local/read-only. Explicit permission denial and exhausted
// quota are recoverable candidates, not verified invalid credentials. The UI
// must leave them unchecked until the user deliberately selects them. Network,
// timeout, generic HTTP 403/429/5xx, and unverified failures never qualify.
func (s *Service) CleanupPreview(ctx context.Context) ([]CleanupCandidate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows := []CleanupCandidate{}
	for _, c := range s.vault.Accounts {
		if s.cleanupEligibleLocked(c) {
			rows = append(rows, s.cleanupCandidateLocked(c))
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows, nil
}

// Cleanup deletes only reviewed, still-eligible versions in one atomic vault
// write. No credential/fence is published before persistence succeeds.
func (s *Service) Cleanup(ctx context.Context, identities []CleanupIdentity) (CleanupResult, error) {
	result := CleanupResult{Deleted: []CleanupCandidate{}, Skipped: []CleanupSkipped{}}
	if len(identities) == 0 || len(identities) > MaxAccounts {
		return result, ErrInvalidImport
	}
	seen := map[string]bool{}
	for _, item := range identities {
		if !validCleanupIdentity(item) || seen[item.ID] {
			return result, ErrInvalidImport
		}
		seen[item.ID] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return result, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	next := cloneVault(s.vault)
	changed := false
	receiptTime := s.now().UTC()
	for _, receipt := range next.CleanupReceipts {
		// A clock adjustment or coarse test clock must not evict the receipt
		// for this just-committed batch in preference to an older one.
		if !receiptTime.After(receipt.CreatedAt) {
			receiptTime = receipt.CreatedAt.Add(time.Nanosecond)
		}
	}
	for _, item := range identities {
		c, ok := s.vault.Accounts[item.ID]
		reason := ""
		if !ok {
			if receipt, exists := s.vault.CleanupReceipts[item.ID]; exists && receipt.CleanupIdentity == item && s.vault.Epochs[item.ID] == item.Epoch+1 {
				result.Deleted = append(result.Deleted, CleanupCandidate{CleanupIdentity: item})
				continue
			}
			reason = "not_found"
		} else if c.Epoch != item.Epoch || c.IdentityEpoch != item.IdentityEpoch {
			reason = "changed"
		} else if !s.cleanupEligibleLocked(c) || item.Revision == "" && !s.cleanupLegacyEligibleLocked(c) {
			reason = "not_eligible"
		} else if item.Revision != "" && item.Revision != s.cleanupRevisionLocked(c, s.cleanupStatusLocked(c)) {
			reason = "changed"
		}
		if reason != "" {
			result.Skipped = append(result.Skipped, CleanupSkipped{ID: item.ID, Reason: reason})
			continue
		}
		deleted := s.cleanupCandidateLocked(c)
		deleted.CleanupIdentity = item // Preserve the exact reviewed/retry handle.
		result.Deleted = append(result.Deleted, deleted)
		delete(next.Accounts, item.ID)
		delete(next.ResetRedemptions, item.ID)
		next.Epochs[item.ID]++
		next.CleanupReceipts[item.ID] = cleanupReceipt{CleanupIdentity: item, CreatedAt: receiptTime}
		changed = true
	}
	if !changed {
		return result, nil
	}
	trimCleanupReceipts(next.CleanupReceipts)
	if err := writeVault(s.path, s.protector, next); err != nil {
		result.Deleted = []CleanupCandidate{}
		return result, err
	}
	s.vault = next
	for _, deleted := range result.Deleted {
		s.fenceLocked(deleted.ID)
	}
	return result, nil
}
