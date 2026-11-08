package codexaccounts

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func nameJWT(email string) string {
	raw, _ := json.Marshal(map[string]any{"email": email})
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(raw) + ".synthetic"
}

func TestAccountNameImportAndLoginDefaultToEmailBeforeLabels(t *testing.T) {
	for _, field := range []string{"name", "label"} {
		raw, _ := json.Marshal(map[string]any{field: "export filename", "access_token": "synthetic-access", "id_token": nameJWT("actual@example.test")})
		p, err := ParseImport(raw)
		if err != nil || p.records[0].Name != "actual@example.test" || p.Preview().Accounts[0].Email != "actual@example.test" || p.Preview().Accounts[0].NameCustomized {
			t.Fatalf("default naming should prefer email: %v", err)
		}
	}
	record := sdkFixture("synthetic-login", "synthetic-refresh")
	record.Label = "SDK filename label"
	c, err := credentialFromSDK(record)
	if err != nil || c.Name != "fixture@example.test" || c.Email != "fixture@example.test" || c.NameMode != nameModeDefault {
		t.Fatalf("SDK label overrode provider email: %v", err)
	}
}

func TestAccountNameRenameIsDurableMetadataOnlyAndRestoresDefault(t *testing.T) {
	s, p, path := newTestService(t, nil, nil)
	a := importFixture(t, s)
	before, _ := s.SDKRecord(context.Background(), a.ID)
	after, err := s.Rename(context.Background(), a.ID, "  工作账号  ", a.IdentityEpoch)
	if err != nil || after.Name != "工作账号" || !after.NameCustomized || after.Email != "fixture@example.test" || after.Epoch != a.Epoch || after.IdentityEpoch != a.IdentityEpoch {
		t.Fatalf("metadata rename = %+v %v", after, err)
	}
	record, _ := s.SDKRecord(context.Background(), a.ID)
	if record.Metadata["email"] != "fixture@example.test" || record.Label != "工作账号" || record.Metadata["access_token"] != before.Metadata["access_token"] || record.RegistrationEpoch != before.RegistrationEpoch {
		t.Fatal("rename changed auth or mixed label with email")
	}
	reload, err := New(Options{Path: path, Protector: p})
	if err != nil {
		t.Fatal(err)
	}
	defer reload.Close()
	loaded, _ := reload.Get(a.ID)
	if loaded.Name != "工作账号" || !loaded.NameCustomized {
		t.Fatal("custom name lost on reload")
	}
	reset, err := s.Rename(context.Background(), a.ID, "", a.IdentityEpoch)
	if err != nil || reset.Name != "fixture@example.test" || reset.NameCustomized || reset.Epoch != a.Epoch {
		t.Fatal("restore email changed identity", err)
	}
}

func TestAccountNameRenameFencesInvalidStaleFailedAndCancelledWrites(t *testing.T) {
	s, p, _ := newTestService(t, nil, nil)
	a := importFixture(t, s)
	for _, epoch := range []uint64{0, a.IdentityEpoch + 1} {
		if _, err := s.Rename(context.Background(), a.ID, "different", epoch); err != ErrConflict {
			t.Fatal("stale identity accepted")
		}
	}
	for _, value := range []string{"bad\nname", "bad\tname", strings.Repeat("x", 321)} {
		if _, err := s.Rename(context.Background(), a.ID, value, a.IdentityEpoch); err != ErrInvalidImport {
			t.Fatal("invalid name accepted")
		}
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Rename(cancelled, a.ID, "cancelled", a.IdentityEpoch); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled rename accepted")
	}
	p.fail.Store(true)
	if _, err := s.Rename(context.Background(), a.ID, "unsaved", a.IdentityEpoch); err != ErrSaveFailed {
		t.Fatal("unsaved name published")
	}
	p.fail.Store(false)
	current, _ := s.Get(a.ID)
	if current.Name != a.Name || current.NameCustomized || current.Epoch != a.Epoch {
		t.Fatal("failed rename mutated account")
	}
	if err := s.Delete(context.Background(), a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Rename(context.Background(), a.ID, "deleted", a.IdentityEpoch); err != ErrNotFound {
		t.Fatal("deleted account renamed")
	}
}

func TestAccountNameLateRefreshPreservesConcurrentRename(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	s, _, _ := newTestService(t, RefreshFunc(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
		close(entered)
		<-release
		return sdkFixture("synthetic-rotated-access", "synthetic-rotated-refresh"), nil
	}), nil)
	a := importFixture(t, s)
	done := make(chan error, 1)
	go func() { _, err := s.Refresh(context.Background(), a.ID); done <- err }()
	<-entered
	if _, err := s.Rename(context.Background(), a.ID, "during refresh", a.IdentityEpoch); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	current, _ := s.Get(a.ID)
	if current.Name != "during refresh" || !current.NameCustomized || current.IdentityEpoch != a.IdentityEpoch {
		t.Fatal("late refresh reverted name")
	}
}

func TestAccountNamePendingRotationRetryPreservesNewNameAndDefaultReset(t *testing.T) {
	for _, name := range []string{"after failed save", ""} {
		t.Run(name, func(t *testing.T) {
			s, p, _ := newTestService(t, RefreshFunc(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
				return sdkFixture("synthetic-rotated-access", "synthetic-rotated-refresh"), nil
			}), nil)
			a := importFixture(t, s)
			if _, err := s.Rename(context.Background(), a.ID, "before refresh", a.IdentityEpoch); err != nil {
				t.Fatal(err)
			}
			p.fail.Store(true)
			if _, err := s.Refresh(context.Background(), a.ID); err != ErrSaveFailed {
				t.Fatal(err)
			}
			p.fail.Store(false)
			if _, err := s.Rename(context.Background(), a.ID, name, a.IdentityEpoch); err != nil {
				t.Fatal(err)
			}
			current, err := s.RetrySave(context.Background(), a.ID)
			want := name
			if want == "" {
				want = a.Email
			}
			if err != nil || current.Name != want || current.NameCustomized != (name != "") || current.IdentityEpoch != a.IdentityEpoch {
				t.Fatal("retry reverted current naming preference", err)
			}
		})
	}
}

func TestAccountNameImportReplacePreservesCustomButUpdatesDefault(t *testing.T) {
	for _, custom := range []bool{false, true} {
		s, _, _ := newTestService(t, nil, nil)
		a := importFixture(t, s)
		if custom {
			if _, err := s.Rename(context.Background(), a.ID, "user name", a.IdentityEpoch); err != nil {
				t.Fatal(err)
			}
		}
		p, err := ParseImport([]byte(strings.ReplaceAll(fixture, "fixture@example.test", "new@example.test")))
		if err != nil {
			t.Fatal(err)
		}
		rows, err := s.Import(context.Background(), p, a.ID)
		want := "new@example.test"
		if custom {
			want = "user name"
		}
		if err != nil || rows[0].Name != want || rows[0].Email != "new@example.test" || rows[0].NameCustomized != custom {
			t.Fatal("replace lost display preference", err)
		}
		if _, err := s.Rename(context.Background(), a.ID, "stale", a.IdentityEpoch); err != ErrConflict {
			t.Fatal("old dialog renamed replacement")
		}
	}
}

func TestAccountNameLoginReplacePreservesCustomAndConcurrentDefaultReset(t *testing.T) {
	for _, reset := range []bool{false, true} {
		entered, release := make(chan struct{}), make(chan struct{})
		s, _, _ := newTestService(t, nil, LoginFunc(func(context.Context) (*coreauth.Auth, error) {
			close(entered)
			<-release
			record := sdkFixture("synthetic-login", "synthetic-refresh")
			record.Metadata["email"] = "new@example.test"
			return record, nil
		}))
		a := importFixture(t, s)
		if _, err := s.Rename(context.Background(), a.ID, "keep my name", a.IdentityEpoch); err != nil {
			t.Fatal(err)
		}
		login, err := s.StartLogin(context.Background(), LoginRequest{ReplaceID: a.ID, Name: "stale login form name"})
		if err != nil {
			t.Fatal(err)
		}
		<-entered
		if reset {
			if _, err := s.Rename(context.Background(), a.ID, "", a.IdentityEpoch); err != nil {
				t.Fatal(err)
			}
		}
		close(release)
		finished := waitLogin(t, s, login.ID)
		want := "keep my name"
		if reset {
			want = "new@example.test"
		}
		if finished.Status != "complete" || finished.Account == nil || finished.Account.Name != want || finished.Account.NameCustomized == reset {
			t.Fatalf("late login reverted preference: %+v", finished)
		}
	}
}

func TestAccountNameLegacyVaultConservativelyPreservesLabel(t *testing.T) {
	s, p, path := newTestService(t, nil, nil)
	a := importFixture(t, s)
	s.mu.Lock()
	next := cloneVault(s.vault)
	c := next.Accounts[a.ID]
	c.Name, c.Email, c.NameMode = "legacy\tuser label", "", ""
	c.IDToken = nameJWT("legacy@example.test")
	next.Accounts[a.ID] = c
	err := writeVault(path, p, next)
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	reload, err := New(Options{Path: path, Protector: p})
	if err != nil {
		t.Fatal(err)
	}
	defer reload.Close()
	row, _ := reload.Get(a.ID)
	if row.Name != "legacy\tuser label" || row.Email != "legacy@example.test" || !row.NameCustomized || row.IdentityEpoch != a.IdentityEpoch {
		t.Fatal("legacy name overwritten")
	}
	if _, err := reload.Rename(context.Background(), a.ID, "", a.IdentityEpoch); err != nil {
		t.Fatal(err)
	}
	row, _ = reload.Get(a.ID)
	if row.Name != "legacy@example.test" || row.NameCustomized {
		t.Fatal("legacy email default unavailable")
	}
}

func TestAccountNameNoEmailUsesSafeDefaultAndEmailIsSanitized(t *testing.T) {
	c := credential{Name: "Custom", NameMode: nameModeCustom, AccessToken: "secret", Email: "secret@example.test"}
	if c.account(time.Now()).Email != "" {
		t.Fatal("email leaked credential")
	}
	s, _, _ := newTestService(t, nil, nil)
	p, err := ParseImport([]byte(`{"access_token":"synthetic-no-email","name":"file name"}`))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.Import(context.Background(), p, "")
	if err != nil {
		t.Fatal(err)
	}
	row, err := s.Rename(context.Background(), rows[0].ID, "", rows[0].IdentityEpoch)
	if err != nil || row.Name != "Codex account" || row.Email != "" || row.NameCustomized {
		t.Fatal("missing email manufactured a label", err)
	}
}
