package console

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"salcara/bridge/internal/codexaccounts"
)

func TestOfficialTokenPreviewDoesNotExchangeAndImportRotatesDurably(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "ready", true: "refresh-failed"}[fail], func(t *testing.T) {
			s, _ := newTestServer(t)
			calls := 0
			vault, err := codexaccounts.New(codexaccounts.Options{Path: filepath.Join(t.TempDir(), "synthetic.vault"), Protector: consoleProtector{}, Refresher: codexaccounts.RefreshFunc(func(_ context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
				calls++
				if a.Metadata["refresh_token"] != "synthetic-refresh-private" || a.Metadata["access_token"] != "" {
					t.Error("unexpected refresh credential")
				}
				if fail {
					return nil, errors.New("synthetic-upstream-private")
				}
				return &coreauth.Auth{Provider: "codex", Metadata: map[string]any{"access_token": "synthetic-minted-private", "refresh_token": "synthetic-rotated-private", "account_id": "synthetic-provider-private", "expired": "2099-01-01T00:00:00Z"}}, nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(vault.Close)
			s.d.CodexAccounts = vault
			h := s.Handler()
			body := map[string]any{"format": "refresh_token", "credentials": "synthetic-refresh-private"}
			w := localRequest(s, h, "POST", "/api/local/codex/import/preview", body)
			if w.Code != 200 || calls != 0 || len(vault.List()) != 0 {
				t.Fatal("preview exchanged or stored token")
			}
			w = localRequest(s, h, "POST", "/api/local/codex/import", body)
			if w.Code != 200 || calls != 1 || len(vault.List()) != 1 || len(s.d.Store.Get().LocalAccounts) != 1 {
				t.Fatalf("token import incomplete: %d %s", w.Code, w.Body)
			}
			for _, secret := range []string{"synthetic-refresh-private", "synthetic-minted-private", "synthetic-rotated-private", "synthetic-provider-private", "synthetic-upstream-private"} {
				if strings.Contains(w.Body.String(), secret) {
					t.Fatal("token import response exposed secret")
				}
			}
			if fail {
				if !strings.Contains(w.Body.String(), `"saved":true`) || !strings.Contains(w.Body.String(), `"ok":false`) {
					t.Fatal("failed exchange claimed usable credential")
				}
			} else if vault.List()[0].Status != codexaccounts.StatusReady || !strings.Contains(w.Body.String(), `"ok":true`) {
				t.Fatal("successful exchange not published")
			}
			for _, invalid := range []map[string]any{{"format": "guess", "credentials": "fixture"}, {"format": "access_token", "credentials": "sk-fixture"}, {"format": "json", "credentials": "not json"}} {
				w = localRequest(s, h, "POST", "/api/local/codex/import/preview", invalid)
				if w.Code != http.StatusBadRequest {
					t.Fatal("invalid import accepted")
				}
			}
		})
	}
}

func TestOfficialAttachmentDoesNotRecreateDeletedCredential(t *testing.T) {
	s, h := officialConsole(t)
	id := importOfficialConsole(t, s, h)
	row, _ := s.d.CodexAccounts.Get(id)
	if err := s.d.CodexAccounts.Delete(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err := s.attachOfficialRows([]codexaccounts.Account{row}); err == nil {
		t.Fatal("late import attached a deleted protected account")
	}
}
