package codexaccounts

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestExecutionAvailabilityDistinguishesRefreshOnlyUnknownExpiryAndExpiredAccess(t *testing.T) {
	for _, test := range []struct {
		name, raw                           string
		usable, refresh, recommended, login bool
	}{
		{"refresh-only", `{"refresh_token":"synthetic-refresh"}`, false, true, false, false},
		{"refresh-only-future-hint", `{"refresh_token":"synthetic-refresh","expired":"2099-01-01T00:00:00Z"}`, false, true, false, false},
		{"opaque-unknown-expiry", `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh"}`, true, false, false, false},
		{"valid", `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expired":"2099-01-01T00:00:00Z"}`, true, false, false, false},
		{"expired", `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expired":"2000-01-01T00:00:00Z"}`, false, true, false, false},
		{"near-expiry", `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expired":"` + time.Now().Add(40*time.Second).UTC().Format(time.RFC3339) + `"}`, true, false, true, false},
		{"expired-access-only", `{"access_token":"synthetic-access","expired":"2000-01-01T00:00:00Z"}`, false, true, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var refreshes atomic.Int32
			s, protector, _ := newTestService(t, RefreshFunc(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
				refreshes.Add(1)
				return nil, errors.New("must not refresh")
			}), nil)
			p, err := ParseImport([]byte(test.raw))
			if err != nil {
				t.Fatal(err)
			}
			rows, err := s.Import(context.Background(), p, "")
			if err != nil {
				t.Fatal(err)
			}
			before := s.List()
			protector.fail.Store(true)
			a, err := s.ExecutionAvailability(rows[0].ID, rows[0].IdentityEpoch)
			if err != nil || a.AccessUsable != test.usable || a.RequiresRefresh != test.refresh || a.RefreshRecommended != test.recommended || a.NeedsLogin != test.login || a.PendingSave {
				t.Fatalf("incorrect availability: %+v, error=%v", a, err)
			}
			if !reflect.DeepEqual(before, s.List()) || refreshes.Load() != 0 {
				t.Fatal("local availability mutated or refreshed credentials")
			}
			safe, _ := json.Marshal(a)
			if strings.Contains(string(safe), "synthetic-") {
				t.Fatal("availability exposes credential data")
			}
		})
	}
}

func TestExecutionAvailabilityPreservesUsableOldAccessAndBlocksUnsavedOrInvalidRefresh(t *testing.T) {
	for _, future := range []bool{false, true} {
		for _, pending := range []bool{false, true} {
			t.Run(map[bool]string{false: "expired", true: "valid"}[future]+"/"+map[bool]string{false: "needs-login", true: "pending-save"}[pending], func(t *testing.T) {
				s, protector, _ := newTestService(t, RefreshFunc(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
					if !pending {
						return nil, ErrNeedsLogin
					}
					return sdkFixture("synthetic-rotated-access", "synthetic-rotated-refresh"), nil
				}), nil)
				expiry := "2000-01-01T00:00:00Z"
				if future {
					expiry = "2099-01-01T00:00:00Z"
				}
				p, _ := ParseImport([]byte(`{"access_token":"synthetic-old-access","refresh_token":"synthetic-old-refresh","expired":"` + expiry + `"}`))
				rows, err := s.Import(context.Background(), p, "")
				if err != nil {
					t.Fatal(err)
				}
				protector.fail.Store(pending)
				_, err = s.Refresh(context.Background(), rows[0].ID)
				if pending && err != ErrSaveFailed || !pending && err != ErrNeedsLogin {
					t.Fatal("fixture did not reach credential failure state", err)
				}
				a, err := s.ExecutionAvailability(rows[0].ID, rows[0].IdentityEpoch)
				if err != nil || a.AccessUsable != future || a.RequiresRefresh == future || a.RefreshEligible || a.PendingSave != pending || a.NeedsLogin == pending {
					t.Fatalf("old access/failure handling: %+v error=%v", a, err)
				}
			})
		}
	}
}

func TestExecutionAvailabilityFencesIdentityAndClosedService(t *testing.T) {
	s, _, _ := newTestService(t, nil, nil)
	row := importFixture(t, s)
	if _, err := s.ExecutionAvailability(row.ID, 0); err != ErrConflict {
		t.Fatal("zero identity accepted", err)
	}
	p, _ := ParseImport([]byte(fixture))
	if _, err := s.Import(context.Background(), p, row.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExecutionAvailability(row.ID, row.IdentityEpoch); err != ErrConflict {
		t.Fatal("replaced identity accepted", err)
	}
	if _, err := s.ExecutionAvailability("missing", row.IdentityEpoch); err != ErrNotFound {
		t.Fatal(err)
	}
	s.Close()
	if _, err := s.ExecutionAvailability(row.ID, row.IdentityEpoch); err != ErrClosed {
		t.Fatal(err)
	}
}
