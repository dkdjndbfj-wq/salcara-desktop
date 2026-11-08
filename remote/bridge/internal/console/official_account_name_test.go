package console

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"salcara/bridge/internal/codexaccounts"
)

func TestOfficialRenameMetadataAndResetToEmail(t *testing.T) {
	s, h := officialConsole(t)
	id := importOfficialConsole(t, s, h)
	before, _ := s.d.CodexAccounts.Get(id)
	for _, name := range []string{"我的开发账号", ""} {
		w := localRequest(s, h, "POST", "/api/local/codex/rename", map[string]any{"id": id, "name": name, "identityEpoch": before.IdentityEpoch})
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) {
			t.Fatalf("rename: %d %s", w.Code, w.Body)
		}
		after, _ := s.d.CodexAccounts.Get(id)
		expected := name
		if expected == "" {
			expected = "fixture@example.test"
		}
		if after.Name != expected || after.IdentityEpoch != before.IdentityEpoch || after.Epoch != before.Epoch {
			t.Fatal("metadata rename changed credentials or ignored name")
		}
		local, ok := s.d.Store.Get().LocalAccount("official-" + id)
		if !ok || local.Name != expected {
			t.Fatal("card metadata not synchronized")
		}
		assertNoOfficialSecrets(t, w.Body.String())
	}
}

func TestOfficialRenameRejectsAmbiguousOrStaleIdentity(t *testing.T) {
	s, h := officialConsole(t)
	id := importOfficialConsole(t, s, h)
	before, _ := s.d.CodexAccounts.Get(id)
	for _, body := range []map[string]any{
		{"id": id, "name": "new"}, {"id": id, "name": nil, "identityEpoch": before.IdentityEpoch},
		{"id": id, "identityEpoch": before.IdentityEpoch}, {"id": id, "name": "new", "identityEpoch": before.IdentityEpoch + 1},
		{"id": id, "name": "new", "identityEpoch": before.IdentityEpoch, "access_token": "ignored-secret"},
	} {
		w := localRequest(s, h, "POST", "/api/local/codex/rename", body)
		if w.Code != http.StatusBadRequest && w.Code != http.StatusConflict {
			t.Fatalf("accepted ambiguous/stale request: %d", w.Code)
		}
		after, _ := s.d.CodexAccounts.Get(id)
		if after.Name != before.Name {
			t.Fatal("stale rename applied")
		}
	}
}

func TestOfficialRenameDoesNotAttachVaultOnlyAccount(t *testing.T) {
	s, h := officialConsole(t)
	p, err := codexaccounts.ParseImport([]byte(officialFixture))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.d.CodexAccounts.Import(context.Background(), p, "")
	if err != nil {
		t.Fatal(err)
	}
	before := len(s.d.Store.Get().LocalAccounts)
	w := localRequest(s, h, "POST", "/api/local/codex/rename", map[string]any{"id": rows[0].ID, "name": "未关联账号", "identityEpoch": rows[0].IdentityEpoch})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("rename: %d %s", w.Code, w.Body)
	}
	if len(s.d.Store.Get().LocalAccounts) != before {
		t.Fatal("metadata edit attached account")
	}
}

func TestOfficialNameReadUsesCurrentVaultAfterSeparateCardSyncFailure(t *testing.T) {
	s, h := officialConsole(t)
	id := importOfficialConsole(t, s, h)
	before, _ := s.d.CodexAccounts.Get(id)
	if _, err := s.d.CodexAccounts.Rename(context.Background(), id, "新名称", before.IdentityEpoch); err != nil {
		t.Fatal(err)
	}
	// Deliberately leave the separate config copy unchanged, as after a sync failure.
	w := localRequest(s, h, "GET", "/api/local/accounts", nil)
	var out struct {
		Accounts []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"accounts"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatal("cannot read cards")
	}
	for _, a := range out.Accounts {
		if a.ID == "official-"+id {
			if a.Name != "新名称" {
				t.Fatal("stale copied name")
			}
			return
		}
	}
	t.Fatal("missing account")
}
