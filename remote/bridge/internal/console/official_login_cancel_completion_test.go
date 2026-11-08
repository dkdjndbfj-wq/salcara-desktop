package console

import (
	"encoding/json"
	"net/http"
	"testing"

	"salcara/bridge/internal/codexaccounts"
)

// A cancellation can arrive after credentials were committed but before the UI
// observed completion. The follow-up status must preserve that successful
// login and attach its protected card, rather than claim cancellation.
func TestOfficialCancelAfterCompletionStatusPreservesAndAttachesAccount(t *testing.T) {
	s, h := officialConsole(t)
	response := localRequest(s, h, http.MethodPost, "/api/local/codex/login", map[string]string{})
	var started codexaccounts.LoginSession
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &started) != nil || started.ID == "" {
		t.Fatal("synthetic login did not start")
	}
	completed := waitLoginSession(t, s, started.ID, func(row codexaccounts.LoginSession) bool { return row.Status != "pending" })
	if completed.Status != "complete" || completed.Account == nil {
		t.Fatal("synthetic login did not commit before cancellation")
	}
	if _, exists := s.d.Store.Get().LocalAccount("official-" + completed.Account.ID); exists {
		t.Fatal("fixture unexpectedly attached the account before status was read")
	}
	for range 2 {
		response = localRequest(s, h, http.MethodPost, "/api/local/codex/login/cancel", map[string]string{"id": started.ID})
		if response.Code != http.StatusOK {
			t.Fatal("completed login could not be cancelled idempotently")
		}
		response = localRequest(s, h, http.MethodPost, "/api/local/codex/login/status", map[string]string{"id": started.ID})
		var observed codexaccounts.LoginSession
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &observed) != nil || observed.Status != "complete" || observed.Account == nil || observed.Account.ID != completed.Account.ID {
			t.Fatal("late cancellation obscured the already committed account")
		}
		assertNoOfficialSecrets(t, response.Body.String())
		card, exists := s.d.Store.Get().LocalAccount("official-" + completed.Account.ID)
		if !exists || card.CredentialRef != completed.Account.ID || card.CredentialEpoch != completed.Account.IdentityEpoch || len(s.d.CodexAccounts.List()) != 1 {
			t.Fatal("completion status lost the protected account association")
		}
	}
}
