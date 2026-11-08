package codexaccounts

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestExplicitTokenAndJSONImportForms(t *testing.T) {
	for _, row := range []struct {
		kind, raw string
		count     int
	}{
		{"access_token", "Bearer synthetic-access-one\nsynthetic-access-two", 2},
		{"refresh_token", "synthetic-refresh-one\nsynthetic-refresh-two", 2},
		{"json", `[{"accessToken":"synthetic-access"},{"credentials":{"access_token":"synthetic-access-two"}}]`, 2},
		{"json", `{"tokens":{"refreshToken":"synthetic-refresh"}}`, 1},
		{"json", `{"refreshToken":"synthetic-refresh","expired":"2099-01-01T00:00:00Z"}`, 1},
		{"json", `{"metadata":{"refresh_token":"synthetic-refresh","expiresAt":"2099-01-01T00:00:00Z"}}`, 1},
		{"json", `{"personal_access_token":"synthetic-access"}`, 1},
		{"json", `{"accessToken":"synthetic-access","refreshToken":null,"idToken":null,"subscription_active_until":null}`, 1},
	} {
		var p *PreparedImport
		var err error
		if row.kind == "json" {
			p, err = ParseImport([]byte(row.raw))
		} else {
			p, err = ParseTokenImport([]byte(row.raw), row.kind)
		}
		if err != nil || len(p.records) != row.count {
			t.Fatalf("supported form failed: %s %v", row.kind, err)
		}
		safe, _ := json.Marshal(p.Preview())
		if strings.Contains(string(safe), "synthetic-access") || strings.Contains(string(safe), "synthetic-refresh") {
			t.Fatal("preview exposed a secret")
		}
		for _, c := range p.records {
			if c.AccessToken == "" && c.account(time.Now()).Status != StatusExpired {
				t.Fatal("refresh-only preview falsely ready")
			}
		}
	}
	for _, raw := range []string{`{"access_token":"a","accessToken":"b"}`, `{"credentials":{"access_token":"a"},"tokens":{"access_token":"b"}}`, `{"refresh_token":false}`, `[1]`} {
		if _, err := ParseImport([]byte(raw)); err != ErrInvalidImport {
			t.Fatal("ambiguous JSON accepted")
		}
	}
	for _, row := range []struct{ kind, raw string }{{"access_token", "sk-fixture"}, {"refresh_token", "Bearer fixture"}, {"guess", "fixture"}, {"access_token", "fixture\x00private"}, {"refresh_token", strings.Repeat("fixture\n", MaxAccounts+1)}} {
		if _, err := ParseTokenImport([]byte(row.raw), row.kind); err != ErrInvalidImport {
			t.Fatal("invalid/ambiguous token input accepted")
		}
	}
}

func TestRefreshOnlyImportPersistsAndUsesSingleRefreshOwner(t *testing.T) {
	calls := 0
	s, protector, path := newTestService(t, RefreshFunc(func(_ context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
		calls++
		if a.Metadata["access_token"] != "" || a.Metadata["refresh_token"] != "synthetic-refresh-only" {
			t.Fatal("wrong refresh credential")
		}
		out := sdkFixture("synthetic-minted-access", "synthetic-rotated-refresh")
		out.Metadata["account_id"] = "synthetic-first-provider-id"
		return out, nil
	}), nil)
	p, _ := ParseTokenImport([]byte("synthetic-refresh-only"), "refresh_token")
	rows, err := s.Import(context.Background(), p, "")
	if err != nil || rows[0].Status != StatusExpired || calls != 0 {
		t.Fatal("preview/import issued network work or falsely ready", err)
	}
	reopened, err := New(Options{Path: path, Protector: protector})
	if err != nil {
		t.Fatal("refresh-only vault failed reload", err)
	}
	reopened.Close()
	row, err := s.Refresh(context.Background(), rows[0].ID)
	if err != nil || row.Status != StatusReady || row.IdentityEpoch != rows[0].IdentityEpoch || calls != 1 {
		t.Fatal("first refresh rejected trusted provider identity", err)
	}
	a, err := s.SDKRecord(context.Background(), row.ID)
	if err != nil || a.Metadata["account_id"] != "synthetic-first-provider-id" {
		t.Fatal("first identity not persisted", err)
	}
}

func TestRefreshOnlyInvalidGrantRemainsReloadableAndNeedsLogin(t *testing.T) {
	s, protector, path := newTestService(t, RefreshFunc(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) { return nil, ErrNeedsLogin }), nil)
	p, _ := ParseTokenImport([]byte("synthetic-refresh-only"), "refresh_token")
	rows, err := s.Import(context.Background(), p, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Refresh(context.Background(), rows[0].ID); err != ErrNeedsLogin {
		t.Fatal("invalid grant not retired", err)
	}
	reopened, err := New(Options{Path: path, Protector: protector})
	if err != nil {
		t.Fatal("retired refresh-only tombstone blocked vault", err)
	}
	defer reopened.Close()
	if reopened.List()[0].Status != StatusNeedsLogin {
		t.Fatal("retired credential revived")
	}
	if _, err := reopened.SDKRecord(context.Background(), rows[0].ID); err != ErrNeedsLogin {
		t.Fatal("invalid credential exposed to executor")
	}
}
