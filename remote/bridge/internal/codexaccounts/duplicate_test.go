package codexaccounts

import (
	"context"
	"sync/atomic"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func duplicatePrepared(t *testing.T, access, refresh, name string) *PreparedImport {
	t.Helper()
	p, err := ParseImport([]byte(`{"access_token":"` + access + `","refresh_token":"` + refresh + `","email":"` + name + `","expired":"2099-01-01T00:00:00Z"}`))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestImportRejectsExistingAccessOrRefreshCredentialAtomically(t *testing.T) {
	for _, test := range []struct {
		name    string
		access  string
		refresh string
	}{
		{name: "access", access: "synthetic-access-old", refresh: "synthetic-other-refresh"},
		{name: "refresh", access: "synthetic-other-access", refresh: "synthetic-refresh-old"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, _, _ := newTestService(t, nil, nil)
			original := importFixture(t, s)
			if _, err := s.Import(context.Background(), duplicatePrepared(t, test.access, test.refresh, "duplicate@example.test"), ""); err != ErrConflict {
				t.Fatalf("duplicate credential import returned %v", err)
			}
			rows := s.List()
			if len(rows) != 1 || rows[0].ID != original.ID {
				t.Fatalf("duplicate import changed vault: %+v", rows)
			}
		})
	}
}

func TestImportRejectsDuplicatesInsideBundleWithoutPartialSave(t *testing.T) {
	s, _, _ := newTestService(t, nil, nil)
	p, err := ParseImport([]byte(`[
		{"access_token":"synthetic-batch-access-a","refresh_token":"synthetic-shared-refresh"},
		{"access_token":"synthetic-batch-access-b","refresh_token":"synthetic-shared-refresh"}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Import(context.Background(), p, ""); err != ErrConflict {
		t.Fatalf("duplicate batch returned %v", err)
	}
	if rows := s.List(); len(rows) != 0 {
		t.Fatalf("duplicate batch was partially saved: %+v", rows)
	}
}

func TestExplicitReplaceCannotCopyAnotherAccountsCredential(t *testing.T) {
	s, _, _ := newTestService(t, nil, nil)
	first := importFixture(t, s)
	secondRows, err := s.Import(context.Background(), duplicatePrepared(t, "synthetic-second-access", "synthetic-second-refresh", "second@example.test"), "")
	if err != nil {
		t.Fatal(err)
	}
	second := secondRows[0]
	if _, err = s.Import(context.Background(), duplicatePrepared(t, "synthetic-access-old", "synthetic-different-refresh", "copy@example.test"), second.ID); err != ErrConflict {
		t.Fatalf("replacement copied another account credential: %v", err)
	}
	if rows := s.List(); len(rows) != 2 || rows[0].ID == rows[1].ID {
		t.Fatalf("conflicting replacement changed vault: %+v", rows)
	}
	// Reimporting the target's own exact credential remains an explicit
	// replacement; it does not create another vault reference.
	if _, err = s.Import(context.Background(), duplicatePrepared(t, "synthetic-access-old", "synthetic-refresh-old", "same@example.test"), first.ID); err != nil {
		t.Fatalf("target could not explicitly replace its own credential: %v", err)
	}
}

func TestImportReservesOtherAccountsPendingRotation(t *testing.T) {
	var calls atomic.Int32
	s, protector, _ := newTestService(t, RefreshFunc(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
		calls.Add(1)
		return sdkFixture("synthetic-pending-access", "synthetic-pending-refresh"), nil
	}), nil)
	original := importFixture(t, s)
	protector.fail.Store(true)
	if _, err := s.Refresh(context.Background(), original.ID); err != ErrSaveFailed || calls.Load() != 1 {
		t.Fatalf("fixture did not retain a pending rotation: %v calls=%d", err, calls.Load())
	}
	for _, test := range []struct {
		name    string
		access  string
		refresh string
	}{
		{name: "access", access: "synthetic-pending-access", refresh: "synthetic-unrelated-refresh"},
		{name: "refresh", access: "synthetic-unrelated-access", refresh: "synthetic-pending-refresh"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := s.Import(context.Background(), duplicatePrepared(t, test.access, test.refresh, "pending-copy@example.test"), ""); err != ErrConflict {
				t.Fatalf("pending credential was not reserved: %v", err)
			}
		})
	}

	// The owner may explicitly replace its own failed-save snapshot. A
	// successful replacement fences and discards the older pending rotation.
	protector.fail.Store(false)
	if _, err := s.Import(context.Background(), duplicatePrepared(t, "synthetic-pending-access", "synthetic-pending-refresh", "owner@example.test"), original.ID); err != nil {
		t.Fatalf("owner could not replace its own pending credential: %v", err)
	}
	if rows := s.List(); len(rows) != 1 || rows[0].ID != original.ID {
		t.Fatalf("pending credential replacement created another reference: %+v", rows)
	}
}

func TestLoginRejectsExistingAccessOrRefreshCredential(t *testing.T) {
	for _, test := range []struct {
		name    string
		access  string
		refresh string
	}{
		{name: "access", access: "synthetic-access-old", refresh: "synthetic-login-other-refresh"},
		{name: "refresh", access: "synthetic-login-other-access", refresh: "synthetic-refresh-old"},
	} {
		t.Run(test.name, func(t *testing.T) {
			connector := LoginFunc(func(context.Context) (*coreauth.Auth, error) {
				return sdkFixture(test.access, test.refresh), nil
			})
			s, _, _ := newTestService(t, nil, connector)
			original := importFixture(t, s)
			login, err := s.StartLogin(context.Background(), LoginRequest{})
			if err != nil {
				t.Fatal(err)
			}
			finished := waitLogin(t, s, login.ID)
			if finished.Status != "failed" || finished.Error != ErrConflict.Error() {
				t.Fatalf("duplicate login was not reported as conflict: %+v", finished)
			}
			rows := s.List()
			if len(rows) != 1 || rows[0].ID != original.ID {
				t.Fatalf("duplicate login created another vault reference: %+v", rows)
			}
		})
	}
}

func TestLoginReplaceCannotCopyAnotherAccountsCredential(t *testing.T) {
	connector := LoginFunc(func(context.Context) (*coreauth.Auth, error) {
		return sdkFixture("synthetic-access-old", "synthetic-login-replacement-refresh"), nil
	})
	s, _, _ := newTestService(t, nil, connector)
	_ = importFixture(t, s)
	secondRows, err := s.Import(context.Background(), duplicatePrepared(t, "synthetic-second-access", "synthetic-second-refresh", "second@example.test"), "")
	if err != nil {
		t.Fatal(err)
	}
	login, err := s.StartLogin(context.Background(), LoginRequest{ReplaceID: secondRows[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	finished := waitLogin(t, s, login.ID)
	if finished.Status != "failed" || finished.Error != ErrConflict.Error() || len(s.List()) != 2 {
		t.Fatalf("login replacement copied another credential: %+v", finished)
	}
}
