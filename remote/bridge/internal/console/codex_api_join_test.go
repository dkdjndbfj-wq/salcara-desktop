package console

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
)

func TestAPIJoinCreatesOnlyClickedAccountWithoutStartingOrReturningSecrets(t *testing.T) {
	s, h, id := sourceRevisionFixture(t)
	source, _ := listedSourceRevision(t, s, h, id)
	body := map[string]any{"accountId": id, "expectedSourceRevision": source}
	w := localRequest(s, h, "POST", "/api/local/codex-api/join", body)
	if w.Code != 200 {
		t.Fatalf("join %d %s", w.Code, w.Body)
	}
	c := s.d.Store.Get()
	if len(c.CodexAPI.Clients) != 1 || c.CodexAPI.Enabled {
		t.Fatal("join started service or created extra clients")
	}
	client := c.CodexAPI.Clients[0]
	if client.AccountID != id || !reflect.DeepEqual(client.AccountIDs, []string{id}) || !reflect.DeepEqual(client.ImageAccountIDs, []string{id}) {
		t.Fatal("join used other accounts")
	}
	if strings.Contains(w.Body.String(), client.KeyDigest) || strings.Contains(w.Body.String(), "sk-salcara") || strings.Contains(w.Body.String(), "key") {
		t.Fatal("managed join exposed key")
	}
	w = localRequest(s, h, "POST", "/api/local/codex-api/join", body)
	if w.Code != 409 || len(s.d.Store.Get().CodexAPI.Clients) != 1 {
		t.Fatal("stale first join duplicated default")
	}
}
func TestAPIJoinPreservesPrimaryFallbackAndExistingPolicies(t *testing.T) {
	s, h, primary, target, id := memberRevisionFixture(t)
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.CodexAPI.Clients[0].AccountIDs = nil
		c.CodexAPI.Clients[0].ImageAccountIDs = nil
		c.CodexAPI.Clients[0].Disabled = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.d.Store.Get().APIClient(id)
	body := memberRevisionRequest(t, s, h, target, id)
	delete(body, "mode")
	delete(body, "enabled")
	w := localRequest(s, h, "POST", "/api/local/codex-api/join", body)
	if w.Code != 200 {
		t.Fatalf("join %d %s", w.Code, w.Body)
	}
	after, _ := s.d.Store.Get().APIClient(id)
	if !reflect.DeepEqual(after.AccountIDs, []string{primary, target}) || !reflect.DeepEqual(after.ImageAccountIDs, []string{primary, target}) {
		t.Fatal("join replaced original grant")
	}
	before.AccountIDs, before.ImageAccountIDs, before.AuthEpoch = after.AccountIDs, after.ImageAccountIDs, after.AuthEpoch
	if !reflect.DeepEqual(before, after) {
		t.Fatal("join changed keys, disabled state, or policies")
	}
	w = localRequest(s, h, "POST", "/api/local/codex-api/join", body)
	if w.Code != 409 {
		t.Fatal("stale join accepted")
	}
}
func TestAPIJoinChangedSourceAndFailedSavesCannotGrantAccess(t *testing.T) {
	for _, scenario := range []string{"missing-revision", "changed-source", "failed-save"} {
		t.Run(scenario, func(t *testing.T) {
			s, h, id := sourceRevisionFixture(t)
			source, _ := listedSourceRevision(t, s, h, id)
			body := map[string]any{"accountId": id, "expectedSourceRevision": source}
			if scenario == "missing-revision" {
				delete(body, "expectedSourceRevision")
			}
			if scenario == "changed-source" {
				body["expectedSourceRevision"] = "src-v1-" + strings.Repeat("0", 64)
			}
			if scenario == "failed-save" {
				blockCodexAPIConfigSaves(t, s)
			}
			w := localRequest(s, h, "POST", "/api/local/codex-api/join", body)
			if w.Code < 400 || len(s.d.Store.Get().CodexAPI.Clients) > 0 {
				t.Fatalf("failed join granted access %d", w.Code)
			}
		})
	}
}
func TestAPIListenerNativeRoutesAndDiscoveryStayOnAPIOnlyListener(t *testing.T) {
	s, _ := newTestServer(t)
	n := config.CodexAPINetwork{Ports: []int{48123}}
	configureAPINetwork(t, s, n)
	s.apiNetwork.binding = "native-binding"
	h := s.apiListenerHandler(48123, n, "native-binding")
	for _, path := range []string{"/v1beta/models", "/api/tags", "/codex-api/v1beta/models"} {
		w := networkRequest(h, path, "127.0.0.1:1234", runtimeRevokeClientKey)
		var out map[string]json.RawMessage
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil || out["models"] == nil {
			t.Fatalf("native discovery %s: %d %s", path, w.Code, w.Body)
		}
	}
	for _, path := range []string{"/api/local/accounts", "/api/local/codex-api/join", "/api/delete", "/codex-api/api/local/accounts"} {
		if w := networkRequest(h, path, "127.0.0.1:1234", runtimeRevokeClientKey); w.Code != 404 {
			t.Fatalf("control route exposed %s %d", path, w.Code)
		}
	}
}
