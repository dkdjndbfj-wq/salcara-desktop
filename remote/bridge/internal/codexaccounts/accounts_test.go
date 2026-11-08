package codexaccounts

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// All credentials here are invented fixtures, never a user's auth files.
const fixture = `{"type":"codex","email":"fixture@example.test","access_token":"synthetic-access-old","refresh_token":"synthetic-refresh-old","account_id":"explicit-fixture-account","expired":"2099-01-01T00:00:00Z"}`

type testProtector struct{ fail atomic.Bool }

func (p *testProtector) Protect(raw []byte) ([]byte, error) {
	if p.fail.Load() {
		return nil, errors.New("private implementation detail synthetic-refresh-DO-NOT-LEAK")
	}
	out := append([]byte(nil), raw...)
	for i := range out {
		out[i] ^= 0xa5
	}
	return out, nil
}
func (*testProtector) Unprotect(raw []byte) ([]byte, error) {
	out := append([]byte(nil), raw...)
	for i := range out {
		out[i] ^= 0xa5
	}
	return out, nil
}
func newTestService(t *testing.T, refresh Refresher, login LoginConnector) (*Service, *testProtector, string) {
	t.Helper()
	p := &testProtector{}
	path := filepath.Join(t.TempDir(), "private", "vault.json")
	s, err := New(Options{Path: path, Protector: p, Refresher: refresh, Login: login})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, p, path
}
func importFixture(t *testing.T, s *Service) Account {
	t.Helper()
	p, err := ParseImport([]byte(fixture))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.Import(context.Background(), p, "")
	if err != nil {
		t.Fatal(err)
	}
	return rows[0]
}
func sdkFixture(access, refresh string) *coreauth.Auth {
	return &coreauth.Auth{Provider: "codex", Metadata: map[string]any{"type": "codex", "access_token": access, "refresh_token": refresh, "email": "fixture@example.test", "expired": "2099-01-01T00:00:00Z"}}
}

func TestImportFormatsAndUnverifiedDisplay(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"jwt-display@example.test","exp":4070908800,"https://api.openai.com/auth":{"chatgpt_plan_type":"fixture-plan","chatgpt_account_id":"UNTRUSTED-ROUTE"}}`))
	jwt := "eyJhbGciOiJub25lIn0." + payload + ".synthetic-signature"
	standard, err := json.Marshal(map[string]any{"tokens": map[string]any{"access_token": jwt, "refresh_token": "synthetic-refresh", "id_token": jwt, "account_id": "explicit-fixture-account"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range [][]byte{[]byte(fixture), standard, []byte(`{"version":1,"provider":"codex","accounts":[` + fixture + `]}`), []byte(`{"provider":"codex","metadata":` + fixture + `}`)} {
		p, err := ParseImport(raw)
		if err != nil {
			t.Fatal(err)
		}
		preview := p.Preview()
		if len(preview.Accounts) != 1 || preview.Version != 1 || preview.Accounts[0].Mode != "legacy_codex_cli" {
			t.Fatalf("bad preview: %+v", preview)
		}
		safe, _ := json.Marshal(preview)
		if strings.Contains(string(safe), "synthetic-access") || strings.Contains(string(safe), "synthetic-refresh") || strings.Contains(string(safe), "id_token") {
			t.Fatalf("secret in DTO: %s", safe)
		}
	}
	p, err := ParseImport(standard)
	if err != nil {
		t.Fatal(err)
	}
	if p.records[0].Name != "jwt-display@example.test" || p.records[0].Plan != "fixture-plan" || p.records[0].AccountID != "explicit-fixture-account" {
		t.Fatalf("JWT display/authority separation: %+v", p.Preview())
	}
	opaque, _ := json.Marshal(p)
	if string(opaque) != "{}" {
		t.Fatalf("prepared import is not opaque: %s", opaque)
	}
}

func TestImportRejectsDuplicatesBoundsAndInvalidTypes(t *testing.T) {
	bad := []string{`{"access_token":"first","access_token":"second"}`, `{"tokens":{"access_token":"first","access_token":"second"}}`, `{"version":2,"accounts":[]}`, `{"version":1,"accounts":[]}`, `{"type":"claude","access_token":"fixture"}`, `{"access_token":"fixture","refresh_token":42}`, `{"tokens":{"access_token":"fixture"},"junk":{"x":1,"x":2}}`, `{"access_token":"fixture"} {}`}
	for _, raw := range bad {
		if _, err := ParseImport([]byte(raw)); err != ErrInvalidImport {
			t.Fatalf("accepted invalid fixture %s: %v", raw, err)
		}
	}
	if _, err := ParseImport([]byte(strings.Repeat(" ", MaxImportBytes+1))); err != ErrInvalidImport {
		t.Fatal("size bound")
	}
	deep := strings.Repeat(`{"nested":`, 18) + `{"access_token":"fixture"}` + strings.Repeat("}", 18)
	if _, err := ParseImport([]byte(deep)); err != ErrInvalidImport {
		t.Fatal("depth bound")
	}
}

func TestDisplayMetadataCannotEchoImportedSecrets(t *testing.T) {
	p, err := ParseImport([]byte(`{"access_token":"synthetic-secret","refresh_token":"synthetic-refresh-secret","email":"synthetic-secret","plan_type":"synthetic-refresh-secret"}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(p.Preview())
	if strings.Contains(string(raw), "synthetic-secret") || strings.Contains(string(raw), "synthetic-refresh-secret") {
		t.Fatal("display metadata echoed credential")
	}
}

func TestIdentityEpochChangesOnReplacementButNotTokenRefresh(t *testing.T) {
	s, p, path := newTestService(t, RefreshFunc(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
		return sdkFixture("synthetic-new-access", "synthetic-new-refresh"), nil
	}), nil)
	original := importFixture(t, s)
	if original.IdentityEpoch == 0 || original.IdentityEpoch != original.Epoch {
		t.Fatal("initial identity not assigned")
	}
	rotated, err := s.Refresh(context.Background(), original.ID)
	if err != nil || rotated.Epoch <= original.Epoch || rotated.IdentityEpoch != original.IdentityEpoch {
		t.Fatalf("token refresh changed identity: %+v %v", rotated, err)
	}
	snapshot, err := s.SDKRecord(context.Background(), original.ID)
	if err != nil || snapshot.Attributes["salcara_identity_epoch"] != "1" {
		t.Fatal("SDK identity attribute", err)
	}
	prepared, _ := ParseImport([]byte(fixture))
	replaced, err := s.Import(context.Background(), prepared, original.ID)
	if err != nil || replaced[0].IdentityEpoch <= original.IdentityEpoch || replaced[0].IdentityEpoch != replaced[0].Epoch {
		t.Fatal("reimport did not change identity", err)
	}
	reload, err := New(Options{Path: path, Protector: p})
	if err != nil {
		t.Fatal(err)
	}
	defer reload.Close()
	row, _ := reload.Get(original.ID)
	if row.IdentityEpoch != replaced[0].IdentityEpoch {
		t.Fatal("identity not persisted")
	}
}

func TestOlderProtectedVaultIdentityEpochMigrates(t *testing.T) {
	s, p, path := newTestService(t, nil, nil)
	row := importFixture(t, s)
	s.mu.Lock()
	legacy := cloneVault(s.vault)
	c := legacy.Accounts[row.ID]
	c.IdentityEpoch = 0
	legacy.Accounts[row.ID] = c
	err := writeVault(path, p, legacy)
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := New(Options{Path: path, Protector: p})
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	migrated, _ := reloaded.Get(row.ID)
	if migrated.IdentityEpoch != row.Epoch {
		t.Fatalf("old identity migration: %+v", migrated)
	}
}

func TestProtectedAtomicPersistenceReplaceDeleteAndEpoch(t *testing.T) {
	s, p, path := newTestService(t, nil, nil)
	account := importFixture(t, s)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "synthetic-") || strings.Contains(string(raw), "fixture@example") {
		t.Fatal("plaintext credentials on disk")
	}
	reopened, err := New(Options{Path: path, Protector: p})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if rows := reopened.List(); len(rows) != 1 || rows[0].ID != account.ID {
		t.Fatal("reload failed")
	}
	prepared, err := ParseImport([]byte(strings.ReplaceAll(fixture, "synthetic-access-old", "synthetic-access-new")))
	if err != nil {
		t.Fatal(err)
	}
	p.fail.Store(true)
	if _, err = s.Import(context.Background(), prepared, account.ID); err != ErrSaveFailed {
		t.Fatal("failed save not surfaced", err)
	}
	record, _ := s.SDKRecord(context.Background(), account.ID)
	if record.Metadata["access_token"] != "synthetic-access-old" {
		t.Fatal("failed save published rotation")
	}
	p.fail.Store(false)
	rows, err := s.Import(context.Background(), prepared, account.ID)
	if err != nil || rows[0].Epoch <= account.Epoch {
		t.Fatal("replace epoch", err)
	}
	if err = s.Delete(context.Background(), account.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SDKRecord(context.Background(), account.ID); err != ErrNotFound {
		t.Fatal("deleted credential remains")
	}
	reload, err := New(Options{Path: path, Protector: p})
	if err != nil {
		t.Fatal(err)
	}
	defer reload.Close()
	if len(reload.List()) != 0 || reload.vault.Epochs[account.ID] <= rows[0].Epoch {
		t.Fatal("delete/tombstone not persistent")
	}
}

func TestRefreshSingleflightHTTPAndPersistBeforePublish(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		close(entered)
		<-release
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"codex","access_token":"synthetic-access-rotated","refresh_token":"synthetic-refresh-rotated","expired":"2099-01-01T00:00:00Z"}`)
	}))
	defer server.Close()
	refresh := RefreshFunc(func(ctx context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
		if a.Metadata["refresh_token"] != "synthetic-refresh-old" {
			return nil, errors.New("wrong fixture token")
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, nil)
		resp, err := server.Client().Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		p, err := ParseImport(data)
		if err != nil {
			return nil, err
		}
		return p.records[0].sdk(), nil
	})
	s, _, path := newTestService(t, refresh, nil)
	account := importFixture(t, s)
	var wg sync.WaitGroup
	errorsCh := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.Refresh(context.Background(), account.ID); errorsCh <- err }()
	}
	<-entered
	old, _ := s.SDKRecord(context.Background(), account.ID)
	if old.Metadata["access_token"] != "synthetic-access-old" {
		t.Fatal("rotation visible early")
	}
	// All waiters must have joined before releasing the synthetic upstream.
	time.Sleep(30 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("refresh calls: %d", calls.Load())
	}
	newRecord, _ := s.SDKRecord(context.Background(), account.ID)
	if newRecord.Metadata["refresh_token"] != "synthetic-refresh-rotated" || newRecord.RegistrationEpoch <= old.RegistrationEpoch {
		t.Fatal("rotation not installed")
	}
	reload, err := New(Options{Path: path, Protector: s.protector})
	if err != nil {
		t.Fatal(err)
	}
	defer reload.Close()
	persisted, _ := reload.SDKRecord(context.Background(), account.ID)
	if persisted.Metadata["refresh_token"] != "synthetic-refresh-rotated" {
		t.Fatal("published before persistence")
	}
}

func TestRefreshSaveFailureRetainsOldAndRetrySavesRotation(t *testing.T) {
	var calls atomic.Int32
	s, p, _ := newTestService(t, RefreshFunc(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
		calls.Add(1)
		return sdkFixture("synthetic-new-access", "synthetic-new-refresh"), nil
	}), nil)
	account := importFixture(t, s)
	p.fail.Store(true)
	a, err := s.Refresh(context.Background(), account.ID)
	if err != ErrSaveFailed || a.Status != StatusSaveFailed {
		t.Fatalf("save failure: %+v %v", a, err)
	}
	old, _ := s.SDKRecord(context.Background(), account.ID)
	if old.Metadata["access_token"] != "synthetic-access-old" {
		t.Fatal("unsaved rotation published")
	}
	if _, err = s.Refresh(context.Background(), account.ID); err != ErrSaveFailed || calls.Load() != 1 {
		t.Fatal("old refresh token reused after failed persistence")
	}
	p.fail.Store(false)
	a, err = s.RetrySave(context.Background(), account.ID)
	if err != nil || a.Status != StatusReady {
		t.Fatal("retry save", err)
	}
	current, _ := s.SDKRecord(context.Background(), account.ID)
	if current.Metadata["refresh_token"] != "synthetic-new-refresh" {
		t.Fatal("pending refresh lost")
	}
}

func TestRefreshFailureKeepsOldAccessAndSanitizesErrors(t *testing.T) {
	s, _, _ := newTestService(t, RefreshFunc(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
		return nil, fmt.Errorf("private synthetic-refresh-secret: %w", ErrNeedsLogin)
	}), nil)
	account := importFixture(t, s)
	a, err := s.Refresh(context.Background(), account.ID)
	if err != ErrNeedsLogin || a.Status != StatusNeedsLogin {
		t.Fatal("needs-login missing", err)
	}
	raw, _ := json.Marshal(a)
	if strings.Contains(string(raw), "private") || strings.Contains(err.Error(), "secret") {
		t.Fatal("error leaked")
	}
	old, err := s.SDKRecord(context.Background(), account.ID)
	if err != nil || old.Metadata["access_token"] != "synthetic-access-old" {
		t.Fatal("valid old access lost")
	}
}

func TestInvalidGrantRetiresRefreshTokenAcrossRestart(t *testing.T) {
	var calls atomic.Int32
	s, p, path := newTestService(t, RefreshFunc(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
		calls.Add(1)
		return nil, fmt.Errorf("private synthetic-refresh-secret: %w", ErrNeedsLogin)
	}), nil)
	before := importFixture(t, s)
	after, err := s.Refresh(context.Background(), before.ID)
	if err != ErrNeedsLogin || after.Status != StatusNeedsLogin {
		t.Fatalf("invalid grant state: %+v %v", after, err)
	}
	if after.Epoch <= before.Epoch || after.IdentityEpoch != before.IdentityEpoch {
		t.Fatalf("credential fence was not advanced without replacing identity: before=%+v after=%+v", before, after)
	}
	record, err := s.SDKRecord(context.Background(), before.ID)
	if err != nil || record.Metadata["access_token"] != "synthetic-access-old" || record.Metadata["refresh_token"] != "" {
		t.Fatalf("invalid refresh credential remained visible: %+v %v", record, err)
	}

	s.Close()
	reloaded, err := New(Options{Path: path, Protector: p, Refresher: RefreshFunc(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
		calls.Add(1)
		return nil, errors.New("invalid_grant should never be called after reload")
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	persisted, err := reloaded.SDKRecord(context.Background(), before.ID)
	if err != nil || persisted.Metadata["refresh_token"] != "" || persisted.Metadata["access_token"] != "synthetic-access-old" {
		t.Fatalf("retired refresh credential reappeared after reload: %+v %v", persisted, err)
	}
	afterRestart, err := reloaded.Refresh(context.Background(), before.ID)
	if err != ErrNeedsLogin || afterRestart.Status != StatusNeedsLogin || calls.Load() != 1 {
		t.Fatalf("reloaded service retried rejected refresh token: %+v calls=%d err=%v", afterRestart, calls.Load(), err)
	}
}

func TestInvalidGrantSaveFailureFailsClosedUntilRetry(t *testing.T) {
	var calls atomic.Int32
	s, p, path := newTestService(t, RefreshFunc(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
		calls.Add(1)
		return nil, fmt.Errorf("private synthetic-refresh-secret: %w", ErrNeedsLogin)
	}), nil)
	before := importFixture(t, s)
	p.fail.Store(true)
	after, err := s.Refresh(context.Background(), before.ID)
	if err != ErrSaveFailed || after.Status != StatusSaveFailed {
		t.Fatalf("failed retirement was not surfaced: %+v %v", after, err)
	}
	record, err := s.SDKRecord(context.Background(), before.ID)
	if err != nil || record.Metadata["access_token"] != "synthetic-access-old" || record.Metadata["refresh_token"] != "" {
		t.Fatalf("current process retained rejected refresh credential: %+v %v", record, err)
	}
	if _, err = s.Refresh(context.Background(), before.ID); err != ErrSaveFailed || calls.Load() != 1 {
		t.Fatalf("failed retirement retried provider token: calls=%d err=%v", calls.Load(), err)
	}

	p.fail.Store(false)
	after, err = s.RetrySave(context.Background(), before.ID)
	if err != nil || after.Status != StatusNeedsLogin || after.Epoch <= before.Epoch || after.IdentityEpoch != before.IdentityEpoch {
		t.Fatalf("retirement retry did not commit: before=%+v after=%+v err=%v", before, after, err)
	}
	s.Close()
	reloaded, err := New(Options{Path: path, Protector: p, Refresher: RefreshFunc(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
		calls.Add(1)
		return nil, errors.New("retired token must not be retried")
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	persisted, err := reloaded.SDKRecord(context.Background(), before.ID)
	if err != nil || persisted.Metadata["refresh_token"] != "" {
		t.Fatalf("retried retirement was not durable: %+v %v", persisted, err)
	}
	if _, err = reloaded.Refresh(context.Background(), before.ID); err != ErrNeedsLogin || calls.Load() != 1 {
		t.Fatalf("reloaded service reused retired token: calls=%d err=%v", calls.Load(), err)
	}
}

func TestDeleteAndReplacementFenceSlowRefresh(t *testing.T) {
	for _, operation := range []string{"delete", "replace", "cancel"} {
		t.Run(operation, func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			s, _, _ := newTestService(t, RefreshFunc(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
				close(entered)
				<-release
				return sdkFixture("stale-access", "stale-refresh"), nil
			}), nil)
			account := importFixture(t, s)
			done := make(chan error)
			go func() { _, err := s.Refresh(context.Background(), account.ID); done <- err }()
			<-entered
			switch operation {
			case "delete":
				if err := s.Delete(context.Background(), account.ID); err != nil {
					t.Fatal(err)
				}
			case "replace":
				p, _ := ParseImport([]byte(strings.ReplaceAll(fixture, "synthetic-access-old", "relogin-access")))
				if _, err := s.Import(context.Background(), p, account.ID); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				s.CancelRefresh(account.ID)
			}
			close(release)
			if err := <-done; err == nil {
				t.Fatal("stale refresh accepted")
			}
			if operation != "delete" {
				current, _ := s.SDKRecord(context.Background(), account.ID)
				if current.Metadata["access_token"] == "stale-access" {
					t.Fatal("stale credential installed")
				}
			}
		})
	}
}

func TestCancelledRefreshCannotOverlapIgnoringConnector(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s, _, _ := newTestService(t, RefreshFunc(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
		calls.Add(1)
		close(entered)
		<-release
		return sdkFixture("cancelled-access", "cancelled-refresh"), nil
	}), nil)
	account := importFixture(t, s)
	done := make(chan error)
	go func() { _, err := s.Refresh(context.Background(), account.ID); done <- err }()
	<-entered
	s.CancelRefresh(account.ID)
	if _, err := s.Refresh(context.Background(), account.ID); err != ErrConflict || calls.Load() != 1 {
		t.Fatal("cancelled connector overlapped another refresh", err)
	}
	close(release)
	if err := <-done; err != ErrConflict {
		t.Fatal("cancelled rotation accepted", err)
	}
}

func waitLogin(t *testing.T, s *Service, id string) LoginSession {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		l, err := s.LoginStatus(id)
		if err != nil {
			t.Fatal(err)
		}
		if l.Status != "pending" {
			return l
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("login did not complete")
	return LoginSession{}
}
func TestLoginCancelDeleteReloginFencing(t *testing.T) {
	for _, operation := range []string{"cancel", "delete", "replace"} {
		t.Run(operation, func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			s, _, _ := newTestService(t, nil, LoginFunc(func(context.Context) (*coreauth.Auth, error) {
				close(entered)
				<-release
				return sdkFixture("stale-login-access", "stale-login-refresh"), nil
			}))
			account := importFixture(t, s)
			l, err := s.StartLogin(context.Background(), LoginRequest{ReplaceID: account.ID})
			if err != nil {
				t.Fatal(err)
			}
			<-entered
			switch operation {
			case "cancel":
				if err = s.CancelLogin(l.ID); err != nil {
					t.Fatal(err)
				}
			case "delete":
				if err = s.Delete(context.Background(), account.ID); err != nil {
					t.Fatal(err)
				}
			case "replace":
				p, _ := ParseImport([]byte(fixture))
				if _, err = s.Import(context.Background(), p, account.ID); err != nil {
					t.Fatal(err)
				}
			}
			close(release)
			if finished := waitLogin(t, s, l.ID); finished.Status != "cancelled" {
				t.Fatalf("stale login state: %+v", finished)
			}
		})
	}
}

func TestLoginSuccessAndReadonlySDKStore(t *testing.T) {
	s, _, _ := newTestService(t, nil, LoginFunc(func(context.Context) (*coreauth.Auth, error) {
		return sdkFixture("synthetic-login-access", "synthetic-login-refresh"), nil
	}))
	l, err := s.StartLogin(context.Background(), LoginRequest{Name: "Fixture login"})
	if err != nil {
		t.Fatal(err)
	}
	finished := waitLogin(t, s, l.ID)
	if finished.Status != "complete" || finished.Account == nil || finished.Account.Name != "Fixture login" {
		t.Fatalf("login result: %+v", finished)
	}
	store := s.SDKStore()
	records, err := store.List(context.Background())
	if err != nil || len(records) != 1 {
		t.Fatal("SDK snapshots", err)
	}
	if _, err = store.Save(context.Background(), records[0]); err != ErrConflict {
		t.Fatal("competing SDK write accepted")
	}
	if err = store.Delete(context.Background(), records[0].ID); err != ErrConflict {
		t.Fatal("competing SDK delete accepted")
	}
	raw, _ := json.Marshal(finished)
	if strings.Contains(string(raw), "synthetic-login") {
		t.Fatal("login DTO leaked credentials")
	}
}
