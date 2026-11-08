package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"salcara/bridge/internal/config"
)

const rotateClientPath = "/api/local/codex-api/clients/rotate"

func TestCodexAPIClientNarrowRotationPreservesPolicy(t *testing.T) {
	s, h := newTestServer(t)
	seedCodexAPIRuntime(t, s)
	if err := s.d.Store.Update(func(c *config.Config) error {
		row := &c.CodexAPI.Clients[0]
		row.Name = "Custom fixture"
		row.AccountIDs = []string{runtimeRevokeAccountID}
		row.ImageAccountIDs = []string{runtimeRevokeAccountID}
		row.PoolStrategy, row.PoolAccountID = "single_account", runtimeRevokeAccountID
		row.ModelPrefix = "team/"
		row.ModelAllowlist, row.ModelDenylist = []string{"gpt-fixture"}, []string{"gpt-blocked"}
		row.ModelAllowPrefixes, row.ModelDenyPrefixes = []string{"gpt-"}, []string{"private-"}
		row.ModelAliases = map[string]string{"friendly": "gpt-fixture"}
		row.MaxConcurrency = 3
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.d.Store.Get().APIClient(runtimeRevokeClientID)
	w := localRequest(s, h, http.MethodPost, rotateClientPath, map[string]string{"id": runtimeRevokeClientID})
	if w.Code != http.StatusOK {
		t.Fatalf("rotation status: %d", w.Code)
	}
	var out struct {
		OK  bool   `json:"ok"`
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || !out.OK || out.ID != runtimeRevokeClientID || !strings.HasPrefix(out.Key, "sk-salcara-client-") {
		t.Fatal("rotation did not return a new one-time client key")
	}
	after, _ := s.d.Store.Get().APIClient(runtimeRevokeClientID)
	if after.KeyDigest == before.KeyDigest || after.KeyDigest != config.APIClientDigest(out.Key) || after.AuthEpoch == before.AuthEpoch {
		t.Fatal("rotation did not atomically fence old credentials")
	}
	if got := codexAPIAdmissionStatus(s, runtimeRevokeClientKey); got != http.StatusUnauthorized {
		t.Fatalf("old key still admitted: %d", got)
	}
	after.KeyDigest, after.AuthEpoch = before.KeyDigest, before.AuthEpoch
	if !reflect.DeepEqual(before, after) {
		t.Fatal("narrow rotation changed routing or permissions")
	}
	status := localRequest(s, h, http.MethodGet, "/api/local/codex-api", nil)
	if strings.Contains(status.Body.String(), out.Key) || strings.Contains(status.Body.String(), "keyDigest") {
		t.Fatal("status response leaked key material")
	}
	persisted, err := os.ReadFile(s.d.Store.Path()) // synthetic temp store only
	if err != nil || strings.Contains(string(persisted), out.Key) {
		t.Fatal("plaintext key was persisted")
	}
}

func TestCodexAPIClientNarrowRotationDoesNotEnableDisabledClient(t *testing.T) {
	s, h := newTestServer(t)
	seedCodexAPIRuntime(t, s)
	if err := s.d.Store.Update(func(c *config.Config) error { c.CodexAPI.Clients[0].Disabled = true; return nil }); err != nil {
		t.Fatal(err)
	}
	w := localRequest(s, h, http.MethodPost, rotateClientPath, map[string]string{"id": runtimeRevokeClientID})
	row, _ := s.d.Store.Get().APIClient(runtimeRevokeClientID)
	if w.Code != http.StatusOK || !row.Disabled {
		t.Fatal("reset revived a disabled client")
	}
	var out struct{ Key string }
	if json.Unmarshal(w.Body.Bytes(), &out) != nil || codexAPIAdmissionStatus(s, out.Key) != http.StatusUnauthorized {
		t.Fatal("disabled replacement key was admitted")
	}
}

func TestCodexAPIClientNarrowRotationCannotRecreateRevokedClient(t *testing.T) {
	s, h := newTestServer(t)
	seedCodexAPIRuntime(t, s)
	revoked := localRequest(s, h, http.MethodPost, "/api/local/codex-api/clients/revoke", map[string]string{"id": runtimeRevokeClientID})
	if revoked.Code != http.StatusOK {
		t.Fatal("fixture revoke failed")
	}
	w := localRequest(s, h, http.MethodPost, rotateClientPath, map[string]string{"id": runtimeRevokeClientID})
	if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "sk-salcara-client-") || len(s.d.Store.Get().CodexAPI.Clients) != 0 {
		t.Fatal("rotation recreated a revoked client or returned a key")
	}
}

func TestCodexAPIClientNarrowRotationSaveFailurePreservesOldState(t *testing.T) {
	s, h := newTestServer(t)
	seedCodexAPIRuntime(t, s)
	before := s.d.Store.Get().CodexAPI
	blockCodexAPIConfigSaves(t, s)
	w := localRequest(s, h, http.MethodPost, rotateClientPath, map[string]string{"id": runtimeRevokeClientID})
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "sk-salcara-client-") || !reflect.DeepEqual(before, s.d.Store.Get().CodexAPI) {
		t.Fatal("failed rotation published an unpersisted key or lost settings")
	}
}

func TestCodexAPIClientNarrowRotationRejectsPolicyPayload(t *testing.T) {
	s, _ := newTestServer(t)
	seedCodexAPIRuntime(t, s)
	before := s.d.Store.Get().CodexAPI
	for _, raw := range []string{
		`{}`, `{"id":null}`, `{"id":""}`, `{"id":"../bad"}`,
		`{"id":"runtime-revoke-client","disabled":false}`,
		`{"id":"runtime-revoke-client","accountId":"other"}`,
		`{"id":"runtime-revoke-client","id":"other"}`,
		`{"ID":"runtime-revoke-client"}`, `{"id":"runtime-revoke-client"}{}`,
	} {
		w := httptest.NewRecorder()
		s.handleCodexAPIClientRotate(w, httptest.NewRequest(http.MethodPost, rotateClientPath, strings.NewReader(raw)))
		if w.Code != http.StatusBadRequest || !reflect.DeepEqual(before, s.d.Store.Get().CodexAPI) {
			t.Fatalf("invalid reset payload accepted: %s", raw)
		}
	}
}

func TestCodexAPIClientNarrowRotationConcurrentPolicyEditsSurvive(t *testing.T) {
	s, h := newTestServer(t)
	seedCodexAPIRuntime(t, s)
	start := make(chan struct{})
	errs := make(chan error, 12)
	statuses := make(chan int, 12)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			statuses <- localRequest(s, h, http.MethodPost, rotateClientPath, map[string]string{"id": runtimeRevokeClientID}).Code
		}()
		go func() {
			defer wg.Done()
			<-start
			errs <- s.d.Store.Update(func(c *config.Config) error {
				c.CodexAPI.Clients[0].MaxConcurrency++
				c.CodexAPI.Clients[0].Disabled = true
				return nil
			})
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	close(statuses)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for status := range statuses {
		if status != http.StatusOK {
			t.Fatalf("concurrent rotation status: %d", status)
		}
	}
	row, _ := s.d.Store.Get().APIClient(runtimeRevokeClientID)
	if row.MaxConcurrency != 12 || !row.Disabled {
		t.Fatal("rotation clobbered concurrent settings")
	}
}

func TestCodexAPIClientFirstCreationIsAtomicAcrossWindows(t *testing.T) {
	s, h := newTestServer(t)
	seedCodexAPIRuntime(t, s)
	if err := s.d.Store.Update(func(c *config.Config) error { c.CodexAPI.Clients = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- localRequest(s, h, http.MethodPost, "/api/local/codex-api/clients", map[string]any{
				"name": "本机客户端", "accountId": runtimeRevokeAccountID, "onlyIfEmpty": true,
			})
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	created := 0
	for result := range results {
		switch result.Code {
		case http.StatusOK:
			created++
		case http.StatusConflict:
			if strings.Contains(result.Body.String(), "sk-salcara-client-") {
				t.Fatal("duplicate first creation returned a key")
			}
		default:
			t.Fatalf("unexpected creation status: %d", result.Code)
		}
	}
	if created != 1 || len(s.d.Store.Get().CodexAPI.Clients) != 1 {
		t.Fatal("concurrent first-use created duplicate default clients")
	}
	// Advanced, explicit multiple-client creation remains supported.
	extra := localRequest(s, h, http.MethodPost, "/api/local/codex-api/clients", map[string]any{
		"name": "Explicit second client", "accountId": runtimeRevokeAccountID,
	})
	if extra.Code != http.StatusOK || len(s.d.Store.Get().CodexAPI.Clients) != 2 {
		t.Fatal("first-use guard broke advanced client creation")
	}
}

func TestCodexAPIClientFirstCreationRejectsEditAndPreservesExisting(t *testing.T) {
	s, h := newTestServer(t)
	seedCodexAPIRuntime(t, s)
	before := s.d.Store.Get().CodexAPI
	for _, payload := range []map[string]any{
		{"id": runtimeRevokeClientID, "accountId": runtimeRevokeAccountID, "onlyIfEmpty": true},
		{"accountId": runtimeRevokeAccountID, "onlyIfEmpty": true, "rotate": true},
	} {
		result := localRequest(s, h, http.MethodPost, "/api/local/codex-api/clients", payload)
		if result.Code != http.StatusBadRequest || !reflect.DeepEqual(before, s.d.Store.Get().CodexAPI) {
			t.Fatal("first-use flag accepted an edit or rotation")
		}
	}
	result := localRequest(s, h, http.MethodPost, "/api/local/codex-api/clients", map[string]any{
		"name": "本机客户端", "accountId": runtimeRevokeAccountID, "onlyIfEmpty": true,
	})
	if result.Code != http.StatusConflict || !reflect.DeepEqual(before, s.d.Store.Get().CodexAPI) {
		t.Fatal("first-use operation replaced existing settings")
	}
}
