package codexaccounts

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

const importCheckConcurrency = 3
const maxActiveImportChecks = 4
const importCheckBatchTimeout = 20 * time.Second
const importCheckRequestTimeout = 5 * time.Second

var ErrImportCheckBusy = errors.New("Codex import checks are busy")

type ImportCheckAccount struct {
	Index  int       `json:"index"`
	Name   string    `json:"name"`
	Mode   string    `json:"mode"`
	Status string    `json:"status"`
	Code   string    `json:"code"`
	Plan   UsagePlan `json:"plan"`
}

type ImportCheckResult struct {
	Accounts  []ImportCheckAccount `json:"accounts"`
	CheckedAt time.Time            `json:"checkedAt"`
}

type importCheckFlight struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// Select validates original indices, then preserves original batch order.
// Parsing is complete before selection, so malformed records cannot be hidden
// by an unchecked checkbox. Omitted selection preserves legacy import-all.
func (p *PreparedImport) Select(indices []int) (*PreparedImport, error) {
	if p == nil || len(p.records) == 0 || len(p.records) > MaxAccounts {
		return nil, ErrInvalidImport
	}
	selected := &PreparedImport{source: p.source, records: []credential{}}
	if indices == nil {
		selected.records = append(selected.records, p.records...)
		return selected, nil
	}
	if len(indices) == 0 || len(indices) > len(p.records) {
		return nil, ErrInvalidImport
	}
	keep := make([]bool, len(p.records))
	for _, index := range indices {
		if index < 0 || index >= len(keep) || keep[index] {
			return nil, ErrInvalidImport
		}
		keep[index] = true
	}
	for i, c := range p.records {
		if keep[i] {
			selected.records = append(selected.records, c)
		}
	}
	return selected, nil
}

// CheckImport authenticates only against the trusted usage endpoint. It never
// stores the import, changes an existing account, refreshes/rotates a token, or
// sends model input. A verified result is not a model-execution guarantee.
func (s *Service) CheckImport(ctx context.Context, p *PreparedImport) (ImportCheckResult, error) {
	return s.checkImport(ctx, p, importCheckBatchTimeout)
}

func (s *Service) checkImport(ctx context.Context, p *PreparedImport, batchTimeout time.Duration) (ImportCheckResult, error) {
	result := ImportCheckResult{Accounts: []ImportCheckAccount{}}
	if p == nil || len(p.records) == 0 || len(p.records) > MaxAccounts {
		return result, ErrInvalidImport
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return result, ErrClosed
	}
	if len(s.importChecks) >= maxActiveImportChecks {
		s.mu.Unlock()
		return result, ErrImportCheckBusy
	}
	if batchTimeout <= 0 || batchTimeout > importCheckBatchTimeout {
		batchTimeout = importCheckBatchTimeout
	}
	batchCtx, cancel := context.WithTimeout(ctx, batchTimeout)
	flight := &importCheckFlight{cancel: cancel, done: make(chan struct{})}
	s.importChecks[flight] = struct{}{}
	fetcher, now, slots := s.usageFetcher, s.now, s.importCheckSlots
	timeout := s.usageTimeout
	if timeout <= 0 || timeout > importCheckRequestTimeout {
		timeout = importCheckRequestTimeout
	}
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		delete(s.importChecks, flight)
		close(flight.done)
		s.mu.Unlock()
	}()
	result.CheckedAt = now().UTC()
	records := append([]credential(nil), p.records...)
	result.Accounts = make([]ImportCheckAccount, len(records))
	for i, c := range records {
		account := c.account(result.CheckedAt)
		// Compare complete labels before display truncation, otherwise a long
		// credential copied from another record could leave its prefix visible.
		name := account.Name
		for _, other := range records {
			for _, secret := range []string{other.AccessToken, other.RefreshToken, other.IDToken, other.AccountID} {
				if secret != "" && strings.Contains(name, secret) {
					name = "Codex account"
				}
			}
		}
		name = safeDisplayString(name, 128)
		if name == "" {
			name = "Codex account"
		}
		result.Accounts[i] = ImportCheckAccount{Index: i, Name: name, Mode: account.Mode, Status: "unknown", Code: "not_checked", Plan: UsagePlan{Family: "unknown", Source: "not_queried"}}
	}
	jobs := make(chan int, len(records))
	for i := range records {
		jobs <- i
	}
	close(jobs)
	var workers sync.WaitGroup
	for worker := 0; worker < importCheckConcurrency; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				if batchCtx.Err() != nil {
					return
				}
				row, c := &result.Accounts[index], records[index]
				refreshable := c.RefreshToken != "" && c.TokenSourceMode != "chatgpt_web_session"
				if c.AccessToken == "" || refreshable && !c.ExpiresAt.IsZero() && !now().Before(c.ExpiresAt) {
					row.Status, row.Code = "needs_refresh", "refresh_required"
					continue
				}
				select {
				case slots <- struct{}{}:
				case <-batchCtx.Done():
					return
				}
				if batchCtx.Err() != nil {
					<-slots
					return
				}
				requestCtx, requestCancel := context.WithTimeout(batchCtx, timeout)
				response, err := fetcher.FetchUsage(requestCtx, c.sdk())
				if requestCtx.Err() != nil {
					err = requestCtx.Err()
				}
				requestCancel()
				<-slots
				if err == nil && c.AccountID != "" && response.ProviderAccountID != "" && c.AccountID != response.ProviderAccountID {
					err = usageError("identity_mismatch", ErrConflict)
				}
				if err != nil {
					row.Code = importCheckErrorCode(err)
					if errors.Is(err, ErrNeedsLogin) {
						row.Status, row.Code = "invalid", "unauthorized"
						if refreshable {
							row.Status, row.Code = "needs_refresh", "refresh_required"
						}
					}
					continue
				}
				_, family := normalizePlan(response.Snapshot.Plan.Raw)
				row.Plan = UsagePlan{Family: family, Source: UsageSourceOfficial, Verified: true}
				row.Status, row.Code = "verified", "authenticated"
				if importCheckLimited(response.Snapshot) {
					row.Status, row.Code = "limited", "quota_limited"
				}
				if c.TokenSourceMode == "chatgpt_web_session" {
					row.Status, row.Code = "quota_only", "web_session"
				}
			}
		}()
	}
	workers.Wait()
	for i := range result.Accounts {
		if result.Accounts[i].Code == "not_checked" && batchCtx.Err() != nil {
			result.Accounts[i].Code = importCheckErrorCode(batchCtx.Err())
		}
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return result, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	return result, nil
}

func importCheckErrorCode(err error) string {
	if errors.Is(err, ErrConflict) {
		return "identity_mismatch"
	}
	switch code := usageErrorCode(err); code {
	case "forbidden", "rate_limited", "network", "timeout", "cancelled", "invalid_response", "invalid_content_type", "upstream_status", "identity_mismatch":
		return code
	default:
		return "unavailable"
	}
}

func importCheckLimited(snapshot UsageSnapshot) bool {
	if snapshot.SpendControl != nil && snapshot.SpendControl.Reached != nil && *snapshot.SpendControl.Reached {
		return true
	}
	limit := snapshot.RateLimit
	if limit == nil {
		return false
	}
	if limit.Allowed != nil && !*limit.Allowed || limit.LimitReached != nil && *limit.LimitReached {
		return true
	}
	for _, window := range []*UsageWindow{limit.Primary, limit.Secondary} {
		if window != nil && window.RemainingPercent != nil && *window.RemainingPercent == 0 {
			return true
		}
	}
	return false
}
