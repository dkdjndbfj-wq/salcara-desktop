package console

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
)

func TestOfficialMultiFilePreviewAndAtomicImport(t *testing.T) {
	s, h := officialConsole(t)
	files := []string{officialFixture, "\ufeff{\"credentials\":{\"accessToken\":\"synthetic-second-private\"}}"}
	body := map[string]any{"files": files, "format": "json"}
	w := localRequest(s, h, "POST", "/api/local/codex/import/preview", body)
	if w.Code != http.StatusOK || len(s.d.CodexAccounts.List()) != 0 {
		t.Fatalf("preview mutated or rejected batch: %d", w.Code)
	}
	assertNoOfficialSecrets(t, w.Body.String())
	if strings.Contains(w.Body.String(), "synthetic-second-private") {
		t.Fatal("preview exposed a file credential")
	}
	w = localRequest(s, h, "POST", "/api/local/codex/import", body)
	if w.Code != http.StatusOK || len(s.d.CodexAccounts.List()) != 2 || len(s.d.Store.Get().LocalAccounts) != 2 {
		t.Fatalf("batch did not attach both accounts: %d", w.Code)
	}
}

func TestOfficialMultiFileFailuresLeaveVaultUntouched(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"both payloads":         {"credentials": officialFixture, "files": []string{officialFixture}},
		"empty batch":           {"files": []string{}},
		"bad second file":       {"files": []string{officialFixture, "not json"}},
		"duplicate fields":      {"files": []string{officialFixture, `{"access_token":"synthetic-one","access_token":"synthetic-two"}`}},
		"same credential twice": {"files": []string{officialFixture, officialFixture}},
		"unknown format":        {"files": []string{officialFixture}, "format": "guess"},
	} {
		t.Run(name, func(t *testing.T) {
			s, h := officialConsole(t)
			w := localRequest(s, h, "POST", "/api/local/codex/import", body)
			if w.Code != http.StatusBadRequest || len(s.d.CodexAccounts.List()) != 0 || len(s.d.Store.Get().LocalAccounts) != 0 {
				t.Fatalf("invalid batch partially imported: %d", w.Code)
			}
			assertNoOfficialSecrets(t, w.Body.String())
		})
	}
}

func TestOfficialWebSessionImportQueriesOnlyAndRemovesReplacedAPIAssociation(t *testing.T) {
	s, h := officialConsole(t)
	id := importOfficialConsole(t, s, h)
	const aliasID = "custom-official-alias"
	if err := s.d.Store.Update(func(c *config.Config) error {
		alias, _ := c.LocalAccount("official-" + id)
		alias.ID = aliasID
		c.LocalAccounts = append(c.LocalAccounts, alias)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	created := localRequest(s, h, "POST", "/api/local/codex-api/clients", map[string]any{"name": "alias-client", "accountId": aliasID, "accountIds": []string{aliasID}, "imageAccountIds": []string{aliasID}})
	var client struct {
		ID string `json:"id"`
	}
	if created.Code != http.StatusOK || json.Unmarshal(created.Body.Bytes(), &client) != nil || client.ID == "" {
		t.Fatal("could not seed custom API association")
	}
	w := localRequest(s, h, "POST", "/api/local/codex/import", map[string]any{
		"replaceId": id,
		"files":     []string{`{"session":{"accessToken":"synthetic-web-private","user":{"email":"web@example.test"},"expires":"2099-01-01T00:00:00Z"}}`},
	})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"mode":"chatgpt_web_session"`) || len(s.d.Store.Get().LocalAccounts) != 0 {
		t.Fatalf("web session retained API association: %d", w.Code)
	}
	row, err := s.d.CodexAccounts.Get(id)
	if err != nil || row.Mode != "chatgpt_web_session" {
		t.Fatal("web session was not retained for quota lookup")
	}
	bound, exists := s.d.Store.Get().APIClient(client.ID)
	if !exists || !bound.Disabled || containsString(bound.AccountIDs, aliasID) || containsString(bound.ImageAccountIDs, aliasID) {
		t.Fatal("custom alias API references were not retired")
	}
	for _, route := range []string{"/api/local/codex/attach", "/api/local/codex/refresh", "/api/local/codex/reset-credit/prepare"} {
		w = localRequest(s, h, "POST", route, map[string]string{"id": id})
		if w.Code == http.StatusOK {
			t.Fatalf("web session reached OAuth-only action: %s", route)
		}
	}
	w = localRequest(s, h, "POST", "/api/local/codex/reset-credit/consume", map[string]string{"id": id, "redeemRequestId": "11111111-1111-4111-8111-111111111111"})
	if w.Code == http.StatusOK && strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatal("web session accepted a quota-reset mutation")
	}
	// A successful OAuth replacement restores API eligibility through the same path.
	w = localRequest(s, h, "POST", "/api/local/codex/import", map[string]any{"replaceId": id, "credentials": officialFixture})
	if w.Code != http.StatusOK || len(s.d.Store.Get().LocalAccounts) != 1 {
		t.Fatal("OAuth replacement failed to restore association")
	}
}

func TestOfficialMultiFileTokenPreviewRequiresExplicitKind(t *testing.T) {
	s, h := officialConsole(t)
	w := localRequest(s, h, "POST", "/api/local/codex/import/preview", map[string]any{
		"format": "refresh_token", "files": []string{"synthetic-first\n", `["synthetic-second"]`},
	})
	var result struct {
		Accounts []json.RawMessage `json:"accounts"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Accounts) != 2 || len(s.d.CodexAccounts.List()) != 0 {
		t.Fatal("explicit token files did not produce a local-only preview")
	}
	w = localRequest(s, h, "POST", "/api/local/codex/import/preview", map[string]any{"files": []string{"synthetic-first"}})
	if w.Code != http.StatusBadRequest {
		t.Fatal("opaque token kind was guessed")
	}
}
