package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"salcara/bridge/internal/codexaccounts"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func cleanupConsoleFixture(t *testing.T) (*Server, http.Handler, codexaccounts.CleanupIdentity) {
	t.Helper()
	s, h := officialConsole(t)
	raw := strings.Replace(officialFixture, `,"refresh_token":"synthetic-refresh-private"`, "", 1)
	raw = strings.Replace(raw, `2099-01-01T00:00:00Z`, `2000-01-01T00:00:00Z`, 1)
	w := localRequest(s, h, "POST", "/api/local/codex/import", map[string]any{"credentials": raw})
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	row := s.d.CodexAccounts.List()[0]
	if _, err := s.d.CodexAccounts.Refresh(context.Background(), row.ID); !errors.Is(err, codexaccounts.ErrNeedsLogin) {
		t.Fatal(err)
	}
	return s, h, codexaccounts.CleanupIdentity{ID: row.ID, Epoch: row.Epoch, IdentityEpoch: row.IdentityEpoch}
}

func TestOfficialCleanupPreviewCommitAndRetry(t *testing.T) {
	s, h, item := cleanupConsoleFixture(t)
	w := localRequest(s, h, "POST", "/api/local/codex/cleanup/preview", map[string]any{})
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	assertNoOfficialSecrets(t, w.Body.String())
	var preview struct {
		Accounts []codexaccounts.CleanupCandidate `json:"accounts"`
	}
	if json.Unmarshal(w.Body.Bytes(), &preview) != nil || len(preview.Accounts) != 1 || preview.Accounts[0].ID != item.ID {
		t.Fatal(w.Body)
	}
	if preview.Accounts[0].Reason != "needs_login" || preview.Accounts[0].Recoverable || len(preview.Accounts[0].Revision) != 64 {
		t.Fatal("preview did not provide current cleanup contract", w.Body)
	}
	item = preview.Accounts[0].CleanupIdentity
	w = localRequest(s, h, "POST", "/api/local/codex/cleanup", map[string]any{"accounts": []codexaccounts.CleanupIdentity{item}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"syncFailed":false`) {
		t.Fatal(w.Code, w.Body)
	}
	assertNoOfficialSecrets(t, w.Body.String())
	if _, err := s.d.CodexAccounts.Get(item.ID); !errors.Is(err, codexaccounts.ErrNotFound) {
		t.Fatal("vault not removed")
	}
	if _, ok := s.d.Store.Get().LocalAccount("official-" + item.ID); ok {
		t.Fatal("matching card remains")
	}
	w = localRequest(s, h, "POST", "/api/local/codex/cleanup", map[string]any{"accounts": []codexaccounts.CleanupIdentity{item}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"skipped":[]`) || !strings.Contains(w.Body.String(), `"syncFailed":false`) {
		t.Fatal(w.Body)
	}
}

func TestCleanupReferenceCASPreservesNewIdentityAndOrdinaryAPI(t *testing.T) {
	id := strings.Repeat("a", 32)
	cfg := config.Config{LocalAccounts: []config.LocalAccount{
		{ID: "old", CredentialRef: id, CredentialEpoch: 2, SourceType: "official_account", ProviderID: "openai"},
		{ID: "new", CredentialRef: id, CredentialEpoch: 3, SourceType: "official_account", ProviderID: "openai"},
		{ID: "ordinary", CredentialRef: id, CredentialEpoch: 2, SourceType: "api_key"},
	}, Projects: []protocol.Project{{Name: "synthetic-project", Path: "synthetic-project"}}}
	removeCleanedOfficialReferences(&cfg, []codexaccounts.CleanupCandidate{{CleanupIdentity: codexaccounts.CleanupIdentity{ID: id, Epoch: 4, IdentityEpoch: 2}}})
	if len(cfg.LocalAccounts) != 2 || cfg.LocalAccounts[0].ID != "new" || cfg.LocalAccounts[1].ID != "ordinary" || len(cfg.Projects) != 1 {
		t.Fatal("cleanup exceeded exact identity boundary")
	}
}

func TestOfficialCleanupConfigFailureNeverRestoresCredentials(t *testing.T) {
	s, h, item := cleanupConsoleFixture(t)
	path := s.d.Store.Path()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	w := localRequest(s, h, "POST", "/api/local/codex/cleanup", map[string]any{"accounts": []codexaccounts.CleanupIdentity{item}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"syncFailed":true`) {
		t.Fatal(w.Code, w.Body)
	}
	if _, err := s.d.CodexAccounts.Get(item.ID); !errors.Is(err, codexaccounts.ErrNotFound) {
		t.Fatal("config failure left usable credentials")
	}
	if _, ok := s.d.Store.Get().LocalAccount("official-" + item.ID); !ok {
		t.Fatal("failed config update was published")
	}
	// Restore only the synthetic test store path. Exact cleanup replay must
	// complete config synchronization even though no live credential remains.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	w = localRequest(s, h, "POST", "/api/local/codex/cleanup", map[string]any{"accounts": []codexaccounts.CleanupIdentity{item}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"syncFailed":false`) {
		t.Fatal(w.Code, w.Body)
	}
	if _, ok := s.d.Store.Get().LocalAccount("official-" + item.ID); ok {
		t.Fatal("receipt did not remove stale card")
	}
}

func TestOfficialCleanupRejectsNestedAmbiguousJSONWithoutDeletion(t *testing.T) {
	s, h, item := cleanupConsoleFixture(t)
	for _, raw := range []string{
		`{"accounts":[]}`, `{"accounts":null}`, `{"accounts":[null]}`, `{"accounts":[{"id":"` + item.ID + `","epoch":1,"identityEpoch":1,"name":"extra"}]}`,
		`{"accounts":[{"ID":"` + item.ID + `","epoch":1,"identityEpoch":1}]}`, `{"accounts":[{"id":"` + item.ID + `","epoch":1,"epoch":2,"identityEpoch":1}]}`,
		`{"accounts":[{"id":"` + item.ID + `","epoch":1,"identityEpoch":1,"revision":null}]}`,
		`{"accounts":[{"id":"` + item.ID + `","epoch":1,"identityEpoch":1,"revision":""}]}`,
		`{"accounts":[{"id":"` + item.ID + `","epoch":1,"identityEpoch":1,"Revision":"` + strings.Repeat("a", 64) + `"}]}`,
		`{"accounts":[{"id":"` + item.ID + `","epoch":1,"identityEpoch":1,"revision":"` + strings.Repeat("A", 64) + `"}]}`,
		`{"accounts":[{"id":"` + item.ID + `","epoch":1,"identityEpoch":1,"revision":"` + strings.Repeat("g", 64) + `"}]}`,
	} {
		w := do(h, "POST", "/api/local/codex/cleanup", "127.0.0.1:47831", map[string]string{"Cookie": CookieName + "=" + s.Token(), "Content-Type": "application/json"}, raw)
		if w.Code != 400 {
			t.Fatal("ambiguous destructive request accepted", raw, w.Code)
		}
	}
	if _, err := s.d.CodexAccounts.Get(item.ID); err != nil {
		t.Fatal("invalid request deleted account")
	}
}
