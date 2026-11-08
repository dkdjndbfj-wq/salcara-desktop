package codexaccounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func cleanupNeedsLogin(t *testing.T, s *Service) Account {
	t.Helper()
	row := importFixture(t, s)
	s.mu.Lock()
	c := s.vault.Accounts[row.ID]
	c.Status = StatusNeedsLogin
	c.ExpiresAt = time.Unix(0, 0)
	s.vault.Accounts[row.ID] = c
	s.mu.Unlock()
	row.Status = StatusNeedsLogin
	return row
}
func cleanupIdentity(row Account) CleanupIdentity {
	return CleanupIdentity{ID: row.ID, Epoch: row.Epoch, IdentityEpoch: row.IdentityEpoch}
}

func TestCleanupPreviewExplicitFailuresAndSafeDTO(t *testing.T) {
	s, _, _ := newTestService(t, nil, nil)
	row := cleanupNeedsLogin(t, s)
	for _, status := range []string{StatusReady, StatusExpired, StatusRefreshFailed, StatusSaveFailed, StatusNeedsLogin} {
		s.mu.Lock()
		c := s.vault.Accounts[row.ID]
		c.Status = status
		c.ExpiresAt = time.Unix(1, 0)
		s.vault.Accounts[row.ID] = c
		s.mu.Unlock()
		rows, err := s.CleanupPreview(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if (len(rows) == 1) != (status == StatusNeedsLogin) {
			t.Fatalf("status %s became cleanup candidate: %+v", status, rows)
		}
		data, _ := json.Marshal(rows)
		if strings.Contains(string(data), "synthetic-access") || strings.Contains(string(data), "synthetic-refresh") || strings.Contains(string(data), "explicit-fixture-account") {
			t.Fatal("secret exposed")
		}
	}
}

func TestCleanupRequiresKnownExpiredOrAbsentAccess(t *testing.T) {
	now := time.Unix(1000, 0)
	for _, tc := range []struct {
		name     string
		access   string
		expires  time.Time
		eligible bool
	}{
		{"future-access-only", "synthetic-access", now.Add(time.Hour), false},
		{"unknown-access-only", "synthetic-access", time.Time{}, false},
		{"expired-access-only", "synthetic-access", now.Add(-time.Second), true},
		{"expires-exactly-now", "synthetic-access", now, true},
		{"no-access-future-hint", "", now.Add(time.Hour), true},
		{"no-access-unknown-hint", "", time.Time{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := newTestService(t, nil, nil)
			row := importFixture(t, s)
			s.mu.Lock()
			s.now = func() time.Time { return now }
			c := s.vault.Accounts[row.ID]
			c.AccessToken, c.RefreshToken, c.ExpiresAt = tc.access, "", tc.expires
			s.vault.Accounts[row.ID] = c
			s.mu.Unlock()
			// Exercise the actual missing-refresh status transition rather than
			// assuming needs_login necessarily means the access token is invalid.
			if _, err := s.Refresh(context.Background(), row.ID); !errors.Is(err, ErrNeedsLogin) {
				t.Fatal(err)
			}
			preview, err := s.CleanupPreview(context.Background())
			if err != nil || (len(preview) == 1) != tc.eligible {
				t.Fatal("incorrect preview", preview, err)
			}
			result, err := s.Cleanup(context.Background(), []CleanupIdentity{cleanupIdentity(row)})
			if err != nil || (len(result.Deleted) == 1) != tc.eligible {
				t.Fatal("incorrect commit", result, err)
			}
			if !tc.eligible {
				if len(result.Skipped) != 1 || result.Skipped[0].Reason != "not_eligible" {
					t.Fatal(result)
				}
				if _, err := s.Get(row.ID); err != nil {
					t.Fatal("potentially usable access removed", err)
				}
			}
		})
	}
}

func TestCleanupRechecksAccessExpiryAtCommit(t *testing.T) {
	for _, change := range []string{"future-expiry", "unknown-expiry", "clock-rollback"} {
		t.Run(change, func(t *testing.T) {
			s, _, _ := newTestService(t, nil, nil)
			row := cleanupNeedsLogin(t, s)
			s.mu.Lock()
			s.now = func() time.Time { return time.Unix(1000, 0) }
			c := s.vault.Accounts[row.ID]
			c.ExpiresAt = time.Unix(900, 0)
			s.vault.Accounts[row.ID] = c
			s.mu.Unlock()
			preview, err := s.CleanupPreview(context.Background())
			if err != nil || len(preview) != 1 {
				t.Fatal(preview, err)
			}
			s.mu.Lock()
			c = s.vault.Accounts[row.ID]
			switch change {
			case "future-expiry":
				c.ExpiresAt = time.Unix(2000, 0)
			case "unknown-expiry":
				c.ExpiresAt = time.Time{}
			case "clock-rollback":
				s.now = func() time.Time { return time.Unix(800, 0) }
			}
			s.vault.Accounts[row.ID] = c
			s.mu.Unlock()
			result, err := s.Cleanup(context.Background(), []CleanupIdentity{preview[0].CleanupIdentity})
			if err != nil || len(result.Deleted) != 0 || len(result.Skipped) != 1 || result.Skipped[0].Reason != "not_eligible" {
				t.Fatal(result, err)
			}
			if _, err := s.Get(row.ID); err != nil {
				t.Fatal("late expiry change removed account", err)
			}
		})
	}
}

func TestCleanupExcludesAllPendingCredentialWork(t *testing.T) {
	for _, busy := range []string{"refresh", "usage", "reset", "rotation", "login", "unknown-reset"} {
		t.Run(busy, func(t *testing.T) {
			s, _, _ := newTestService(t, nil, nil)
			row := cleanupNeedsLogin(t, s)
			done := make(chan struct{})
			close(done)
			cancel := func() {}
			s.mu.Lock()
			switch busy {
			case "refresh":
				s.flights[row.ID] = &flight{done: done, cancel: cancel}
			case "usage":
				s.usageFlights[row.ID] = &usageFlight{done: done, cancel: cancel}
			case "reset":
				s.resetFlights[row.ID] = &resetCreditFlight{done: done, cancel: cancel}
			case "rotation":
				s.pending[row.ID] = pendingRotation{epoch: row.Epoch}
			case "login":
				s.logins["fixture"] = &loginState{request: LoginRequest{ReplaceID: row.ID}, session: LoginSession{Status: "pending"}, done: done, cancel: cancel}
			case "unknown-reset":
				s.vault.ResetRedemptions[row.ID] = resetRedemption{IdentityEpoch: row.IdentityEpoch, State: resetStateUnknown}
			}
			s.mu.Unlock()
			rows, err := s.CleanupPreview(context.Background())
			if err != nil || len(rows) != 0 {
				t.Fatal(rows, err)
			}
			result, err := s.Cleanup(context.Background(), []CleanupIdentity{cleanupIdentity(row)})
			if err != nil || len(result.Deleted) != 0 || len(result.Skipped) != 1 || result.Skipped[0].Reason != "not_eligible" {
				t.Fatal(result, err)
			}
		})
	}
}

func TestCleanupCASCannotDeleteReimportOrRecoveredAccount(t *testing.T) {
	for _, change := range []string{"reimport", "epoch", "status", "identity"} {
		t.Run(change, func(t *testing.T) {
			s, _, _ := newTestService(t, nil, nil)
			row := cleanupNeedsLogin(t, s)
			if change == "reimport" {
				p, _ := ParseImport([]byte(fixture))
				if _, err := s.Import(context.Background(), p, row.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				s.mu.Lock()
				c := s.vault.Accounts[row.ID]
				switch change {
				case "epoch":
					c.Epoch++
				case "identity":
					c.IdentityEpoch++
				case "status":
					c.Status = StatusReady
				}
				s.vault.Accounts[row.ID] = c
				s.mu.Unlock()
			}
			result, err := s.Cleanup(context.Background(), []CleanupIdentity{cleanupIdentity(row)})
			if err != nil || len(result.Deleted) != 0 || len(result.Skipped) != 1 {
				t.Fatal(result, err)
			}
			if _, err := s.Get(row.ID); err != nil {
				t.Fatal("newer account deleted", err)
			}
		})
	}
}

func TestCleanupDurableCommitAndFailureAreAtomic(t *testing.T) {
	s, p, path := newTestService(t, nil, nil)
	row := cleanupNeedsLogin(t, s)
	before, _ := os.ReadFile(path)
	p.fail.Store(true)
	if result, err := s.Cleanup(context.Background(), []CleanupIdentity{cleanupIdentity(row)}); !errors.Is(err, ErrSaveFailed) || len(result.Deleted) != 0 {
		t.Fatal(result, err)
	}
	if _, err := s.Get(row.ID); err != nil {
		t.Fatal("failed save published deletion")
	}
	after, _ := os.ReadFile(path)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed save modified vault")
	}
	p.fail.Store(false)
	result, err := s.Cleanup(context.Background(), []CleanupIdentity{cleanupIdentity(row)})
	if err != nil || len(result.Deleted) != 1 || len(result.Skipped) != 0 {
		t.Fatal(result, err)
	}
	if _, err = s.Get(row.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted credential still live", err)
	}
	reloaded, err := New(Options{Path: path, Protector: p})
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	if _, err = reloaded.Get(row.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("deletion not durable")
	}
	if reloaded.vault.Epochs[row.ID] <= row.Epoch {
		t.Fatal("deletion failed to retire epoch")
	}
	p.fail.Store(true) // receipt replay must not need another vault write
	result, err = reloaded.Cleanup(context.Background(), []CleanupIdentity{cleanupIdentity(row)})
	if err != nil || len(result.Deleted) != 1 || len(result.Skipped) != 0 || result.Deleted[0].CleanupIdentity != cleanupIdentity(row) {
		t.Fatal("durable retry receipt was not replayed", result, err)
	}
}

func TestCleanupReceiptCannotDeleteReusedOrNewerIdentity(t *testing.T) {
	for _, replacement := range []string{"live-new-identity", "new-epoch", "wrong-request-epoch", "wrong-request-identity"} {
		t.Run(replacement, func(t *testing.T) {
			s, _, _ := newTestService(t, nil, nil)
			row := cleanupNeedsLogin(t, s)
			item := cleanupIdentity(row)
			s.mu.Lock()
			old := s.vault.Accounts[row.ID]
			s.mu.Unlock()
			if _, err := s.Cleanup(context.Background(), []CleanupIdentity{item}); err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			switch replacement {
			case "live-new-identity":
				old.Epoch += 2
				old.IdentityEpoch = old.Epoch
				s.vault.Accounts[old.ID] = old
				s.vault.Epochs[old.ID] = old.Epoch
			case "new-epoch":
				s.vault.Epochs[old.ID]++
			case "wrong-request-epoch":
				item.Epoch++
			case "wrong-request-identity":
				item.Epoch++
				item.IdentityEpoch++
			}
			s.mu.Unlock()
			result, err := s.Cleanup(context.Background(), []CleanupIdentity{item})
			if err != nil || len(result.Deleted) != 0 || len(result.Skipped) != 1 {
				t.Fatal(result, err)
			}
			if replacement == "live-new-identity" {
				if _, err := s.Get(row.ID); err != nil {
					t.Fatal("new identity deleted")
				}
			}
		})
	}
}

func TestCleanupReceiptsAreBoundedAndValidatedOnLoad(t *testing.T) {
	s, p, path := newTestService(t, nil, nil)
	row := cleanupNeedsLogin(t, s)
	if _, err := s.Cleanup(context.Background(), []CleanupIdentity{cleanupIdentity(row)}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	v := cloneVault(s.vault)
	s.mu.Unlock()
	for i := 1; i <= MaxAccounts+3; i++ {
		id := fmt.Sprintf("%032x", i)
		v.CleanupReceipts[id] = cleanupReceipt{CleanupIdentity: CleanupIdentity{ID: id, Epoch: 1, IdentityEpoch: 1}, CreatedAt: time.Unix(int64(i), 0)}
		v.Epochs[id] = 2
	}
	trimCleanupReceipts(v.CleanupReceipts)
	if len(v.CleanupReceipts) != MaxAccounts {
		t.Fatal("unbounded receipts")
	}
	if _, exists := v.CleanupReceipts[fmt.Sprintf("%032x", 1)]; exists {
		t.Fatal("oldest receipt not trimmed")
	}
	if err := writeVault(path, p, v); err != nil {
		t.Fatal(err)
	}
	if _, err := readVault(path, p); err != nil {
		t.Fatal(err)
	}
	receipt := v.CleanupReceipts[row.ID]
	receipt.Epoch = ^uint64(0)
	v.CleanupReceipts[row.ID] = receipt
	if err := writeVault(path, p, v); err != nil {
		t.Fatal(err)
	}
	if _, err := readVault(path, p); !errors.Is(err, ErrSaveFailed) {
		t.Fatal("invalid receipt accepted", err)
	}
}

func TestCleanupNewReceiptSurvivesClockRollbackAndCapacity(t *testing.T) {
	s, _, _ := newTestService(t, nil, nil)
	row := cleanupNeedsLogin(t, s)
	s.mu.Lock()
	s.now = func() time.Time { return time.Unix(1, 0) }
	for i := 1; i <= MaxAccounts; i++ {
		id := fmt.Sprintf("%032x", i)
		s.vault.CleanupReceipts[id] = cleanupReceipt{CleanupIdentity: CleanupIdentity{ID: id, Epoch: 1, IdentityEpoch: 1}, CreatedAt: time.Unix(100, 0)}
		s.vault.Epochs[id] = 2
	}
	s.mu.Unlock()
	if result, err := s.Cleanup(context.Background(), []CleanupIdentity{cleanupIdentity(row)}); err != nil || len(result.Deleted) != 1 {
		t.Fatal(result, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.vault.CleanupReceipts) != MaxAccounts || s.vault.CleanupReceipts[row.ID].ID != row.ID {
		t.Fatal("new batch receipt evicted")
	}
}

func TestCleanupConcurrentRefreshIsSkippedAndCannotLoseRotation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	s, _, _ := newTestService(t, RefreshFunc(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
		close(entered)
		<-release
		return sdkFixture("synthetic-new", "synthetic-new-refresh"), nil
	}), nil)
	row := cleanupNeedsLogin(t, s)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _, _ = s.Refresh(context.Background(), row.ID) }()
	<-entered
	result, err := s.Cleanup(context.Background(), []CleanupIdentity{cleanupIdentity(row)})
	close(release)
	wg.Wait()
	if err != nil || len(result.Deleted) != 0 || len(result.Skipped) != 1 {
		t.Fatal(result, err)
	}
	if _, err := s.Get(row.ID); err != nil {
		t.Fatal("refresh account deleted")
	}
}

func TestCleanupRejectsAmbiguousAndUnboundedSelections(t *testing.T) {
	s, _, _ := newTestService(t, nil, nil)
	row := cleanupNeedsLogin(t, s)
	good := cleanupIdentity(row)
	for _, items := range [][]CleanupIdentity{nil, {good, good}, {{ID: good.ID, Epoch: 0, IdentityEpoch: good.IdentityEpoch}}, {{ID: "not-a-vault-id", Epoch: 1, IdentityEpoch: 1}}, make([]CleanupIdentity, MaxAccounts+1)} {
		if _, err := s.Cleanup(context.Background(), items); !errors.Is(err, ErrInvalidImport) {
			t.Fatal("invalid selection accepted", err)
		}
	}
}
