package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/hubclient"
	"salcara/bridge/internal/protocol"
)

type consoleCatalogAgent struct {
	*sessionAgent
	calls atomic.Int32
	read  func(context.Context) (protocol.ModelCatalog, error)
}

func (a *consoleCatalogAgent) ModelCatalog(ctx context.Context) (protocol.ModelCatalog, error) {
	a.calls.Add(1)
	return a.read(ctx)
}

type consoleCatalogManager struct{ agent agents.Agent }

func (m consoleCatalogManager) Agents() []agents.Agent { return []agents.Agent{m.agent} }
func (m consoleCatalogManager) Get(id string) agents.Agent {
	if m.agent != nil && m.agent.ID() == id {
		return m.agent
	}
	return nil
}
func (consoleCatalogManager) Watch(context.Context)      {}
func (consoleCatalogManager) LocalHandler() http.Handler { return http.NotFoundHandler() }
func (consoleCatalogManager) Close()                     {}

const catalogFixtureClientKey = "synthetic-local-catalogue-client-key"
const catalogFixtureCardID = "synthetic-official-card"

func officialModelsConsole(t *testing.T, savedModels []string) (*Server, http.Handler, *consoleCatalogAgent) {
	t.Helper()
	base, _ := newTestServer(t)
	if err := base.d.Store.Update(func(c *config.Config) error {
		c.LocalAccounts = []config.LocalAccount{{ID: catalogFixtureCardID, Name: "Synthetic catalogue", Kind: "api", SourceType: "official_account", ProviderID: "openai", CredentialRef: "synthetic-vault-ref", CredentialEpoch: 3, BaseURL: "https://chatgpt.com/backend-api/codex", Key: "protected-synthetic-marker", Wire: "responses", Model: "chosen-explicit-model", Models: savedModels}}
		c.CodexAPI = config.CodexAPIService{Enabled: true, Clients: []config.NamedAPIClient{{ID: "synthetic-model-client", Name: "Synthetic client", AccountID: catalogFixtureCardID, KeyDigest: config.APIClientDigest(catalogFixtureClientKey)}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reader := &consoleCatalogAgent{sessionAgent: &sessionAgent{}, read: func(context.Context) (protocol.ModelCatalog, error) {
		return protocol.ModelCatalog{Models: []string{"native-hint"}, ModelCapabilities: map[string]protocol.ModelCapability{"native-hint": {Source: "codex-model-list", ReasoningKnown: true, ReasoningEfforts: []string{"high"}, InputModalities: []string{"image"}}}}, nil
	}}
	hub := hubclient.New(hubclient.Options{Store: base.d.Store})
	hub.SetManager(consoleCatalogManager{agent: reader})
	base.CloseUsageLedger()
	s := New(Deps{Store: base.d.Store, Hub: hub, Version: "test", Port: 47831})
	t.Cleanup(s.CloseUsageLedger)
	return s, s.Handler(), reader
}

func TestOfficialLocalModelsAlwaysRefreshNativeHintsAndPreserveSelection(t *testing.T) {
	s, h, reader := officialModelsConsole(t, []string{"saved-hint"})
	before := s.d.Store.Get()
	for i := 1; i <= 2; i++ {
		w := localRequest(s, h, "POST", "/api/local/models", map[string]any{"id": catalogFixtureCardID, "baseUrl": "https://must-not-contact.invalid", "key": "must-not-send-request-secret"})
		if w.Code != 200 || reader.calls.Load() != int32(i) {
			t.Fatalf("refresh did not read native CLI: %d reads=%d body=%s", w.Code, reader.calls.Load(), w.Body)
		}
		var out struct {
			Models               []string
			NativeSource         string                              `json:"nativeSource"`
			ModelCapabilities    map[string]protocol.ModelCapability `json:"modelCapabilities"`
			CatalogIsEntitlement bool                                `json:"catalogIsEntitlement"`
		}
		if json.Unmarshal(w.Body.Bytes(), &out) != nil || !reflect.DeepEqual(out.Models, []string{"native-hint"}) || out.NativeSource != hubclient.OfficialNativeCatalogSource || out.CatalogIsEntitlement {
			t.Fatalf("native hint response: %s", w.Body)
		}
		for _, capability := range out.ModelCapabilities {
			if capability.ReasoningKnown || len(capability.ReasoningEfforts)+len(capability.InputModalities) != 0 {
				t.Fatal("official picker hint inferred capabilities")
			}
		}
		if strings.Contains(w.Body.String(), "protected-synthetic-marker") || strings.Contains(w.Body.String(), "must-not-send") {
			t.Fatal("credential marker in refresh response")
		}
	}
	after := s.d.Store.Get()
	if after.LocalAccounts[0].Model != before.LocalAccounts[0].Model || !reflect.DeepEqual(after.RemoteAPI, before.RemoteAPI) || !reflect.DeepEqual(after.ToolAPIApplied, before.ToolAPIApplied) || !reflect.DeepEqual(after.ToolAPISelections, before.ToolAPISelections) {
		t.Fatal("read-only catalogue refresh changed tool/account/model selection")
	}
}

func TestOfficialLocalModelsUnavailableOrFailureCannotUseSavedHints(t *testing.T) {
	for _, unavailable := range []bool{true, false} {
		s, h, reader := officialModelsConsole(t, []string{"saved-hint"})
		if unavailable {
			s.d.Hub.SetManager(nil)
		} else {
			reader.read = func(context.Context) (protocol.ModelCatalog, error) {
				return protocol.ModelCatalog{}, errors.New("synthetic-secret-private native auth path")
			}
		}
		w := localRequest(s, h, "POST", "/api/local/models", map[string]string{"id": catalogFixtureCardID})
		if w.Code != 502 || strings.Contains(w.Body.String(), "saved-hint") || strings.Contains(w.Body.String(), "synthetic-secret") || strings.Contains(w.Body.String(), "auth path") {
			t.Fatalf("refresh silently fell back or leaked: %d %s", w.Code, w.Body)
		}
		if !reflect.DeepEqual(s.d.Store.Get().LocalAccounts[0].Models, []string{"saved-hint"}) {
			t.Fatal("failed refresh modified saved hints")
		}
	}
}

func TestOfficialLocalModelsFenceReplacementDuringNativeRead(t *testing.T) {
	s, h, reader := officialModelsConsole(t, []string{"saved-hint"})
	reader.read = func(context.Context) (protocol.ModelCatalog, error) {
		if err := s.d.Store.Update(func(c *config.Config) error { c.LocalAccounts[0].CredentialEpoch++; return nil }); err != nil {
			t.Fatal(err)
		}
		return protocol.ModelCatalog{Models: []string{"old-identity-hint"}}, nil
	}
	w := localRequest(s, h, "POST", "/api/local/models", map[string]string{"id": catalogFixtureCardID})
	if w.Code != 502 || strings.Contains(w.Body.String(), "old-identity-hint") || !reflect.DeepEqual(s.d.Store.Get().LocalAccounts[0].Models, []string{"saved-hint"}) {
		t.Fatalf("stale identity catalogue published: %d %s", w.Code, w.Body)
	}
}

func consoleAPIModels(s *Server) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/codex-api/v1/models", nil)
	r.RemoteAddr = "127.0.0.1:14551"
	r.Header.Set("Authorization", "Bearer "+catalogFixtureClientKey)
	w := httptest.NewRecorder()
	s.apiService.ServeHTTP(w, r)
	return w
}

func TestConsoleWiresOfficialAPIModelHelperWithOrdinaryCachedHints(t *testing.T) {
	s, h, reader := officialModelsConsole(t, []string{"saved-hint"})
	if s.apiService.ModelCatalog == nil {
		t.Fatal("console did not wire official catalogue helper")
	}
	w := consoleAPIModels(s)
	if w.Code != 200 || reader.calls.Load() != 0 || !strings.Contains(w.Body.String(), hubclient.OfficialSavedCatalogSource) {
		t.Fatalf("ordinary API list refreshed or lost saved labels: %d reads=%d %s", w.Code, reader.calls.Load(), w.Body)
	}
	if refreshed := localRequest(s, h, "POST", "/api/local/models", map[string]string{"id": catalogFixtureCardID}); refreshed.Code != 200 {
		t.Fatal(refreshed.Body)
	}
	w = consoleAPIModels(s)
	if w.Code != 200 || reader.calls.Load() != 1 || !strings.Contains(w.Body.String(), hubclient.OfficialNativeCatalogSource) || strings.Contains(w.Body.String(), "protected-synthetic-marker") {
		t.Fatalf("API native cache wiring: %d reads=%d %s", w.Code, reader.calls.Load(), w.Body)
	}
}

func TestOfficialLocalModelsRejectUnsavedOfficialMarker(t *testing.T) {
	s, h := newTestServer(t)
	w := localRequest(s, h, "POST", "/api/local/models", map[string]any{"sourceType": "official_account", "providerId": "openai", "credentialRef": "synthetic-ref", "key": "protected-synthetic-marker", "baseUrl": "https://chatgpt.com/backend-api/codex", "wire": "responses"})
	if w.Code != 400 || strings.Contains(w.Body.String(), "protected-synthetic-marker") {
		t.Fatalf("unsaved marker reached HTTP catalogue probe: %d %s", w.Code, w.Body)
	}
}
