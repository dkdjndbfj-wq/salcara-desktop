package console

import (
	"strings"
	"testing"
)

func TestClientPrefixConsoleRoundTrip(t *testing.T) {
	s, h := officialConsole(t)
	id := importOfficialConsole(t, s, h)
	w := localRequest(s, h, "POST", "/api/local/codex-api/clients", map[string]any{"name": "prefix fixture", "accountId": "official-" + id, "modelAllowPrefixes": []string{"gpt-"}, "modelDenyPrefixes": []string{"gpt-blocked"}})
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	client := s.d.Store.Get().CodexAPI.Clients[0]
	if len(client.ModelAllowPrefixes) != 1 || len(client.ModelDenyPrefixes) != 1 {
		t.Fatal("prefix rules not saved")
	}
	w = localRequest(s, h, "GET", "/api/local/codex-api", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"modelAllowPrefixes":["gpt-"]`) || !strings.Contains(w.Body.String(), `"modelDenyPrefixes":["gpt-blocked"]`) {
		t.Fatalf("status lost policy: %s", w.Body)
	}
	assertNoOfficialSecrets(t, w.Body.String())
	w = localRequest(s, h, "POST", "/api/local/codex-api/clients", map[string]any{"id": client.ID, "name": "bad", "accountId": "official-" + id, "modelAllowPrefixes": []string{"*"}})
	if w.Code != 400 || s.d.Store.Get().CodexAPI.Clients[0].ModelAllowPrefixes[0] != "gpt-" {
		t.Fatal("invalid wildcard replaced working policy")
	}
}
