package apiservice

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

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func callModels(s *Service) *httptest.ResponseRecorder {
	r := apiRequest(context.Background(), syntheticClientKey, nil)
	r.Method, r.URL.Path = http.MethodGet, "/codex-api/v1/models"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestOfficialAPIModelsUsePickerHintsPolicyAndUnknownCapabilities(t *testing.T) {
	s, live := fixtureService(t)
	live.change(func(c *config.Config) {
		c.LocalAccounts[0].ModelAllowlist = []string{"native-model", "denied-model", "gpt-fixture"}
		c.LocalAccounts[0].ModelDenylist = []string{"denied-model"}
	})
	var calls atomic.Int32
	s.ModelCatalog = func(_ context.Context, account config.LocalAccount, refresh bool) (protocol.ModelCatalog, error) {
		calls.Add(1)
		if account.ID != "account-A" || account.CredentialRef != "credential-A" || account.Kind != "api" || refresh {
			t.Error("catalogue selected another account, changed settings or refreshed automatically")
		}
		return protocol.ModelCatalog{Models: []string{"native-model", "native-model", "denied-model", "unlisted-model", "bad\nmodel"}, ModelCapabilities: map[string]protocol.ModelCapability{"native-model": {Source: "codex-native-picker-hint", ReasoningKnown: true, ReasoningEfforts: []string{"high"}, InputModalities: []string{"image"}}}}, nil
	}
	w := callModels(s)
	if w.Code != 200 || calls.Load() != 1 {
		t.Fatalf("models status=%d body=%s", w.Code, w.Body)
	}
	var out struct {
		NativeSource         string `json:"nativeSource"`
		CatalogIsEntitlement bool   `json:"catalogIsEntitlement"`
		Data                 []struct{ ID string }
		ModelCapabilities    map[string]protocol.ModelCapability `json:"modelCapabilities"`
	}
	if json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatal("bad catalogue JSON")
	}
	ids := []string{}
	for _, model := range out.Data {
		ids = append(ids, model.ID)
	}
	if !reflect.DeepEqual(ids, []string{"native-model", "friendly"}) || out.NativeSource != "codex-native-picker-hint" || out.CatalogIsEntitlement {
		t.Fatalf("picker policy/source: %+v", out)
	}
	for _, capability := range out.ModelCapabilities {
		if capability.ReasoningKnown || len(capability.ReasoningEfforts)+len(capability.InputModalities) != 0 {
			t.Fatal("shared picker hints inferred capabilities")
		}
	}
}

func TestOfficialAPIModelsUnavailableAndFailedReaderAreHonestAndSafe(t *testing.T) {
	s, _ := fixtureService(t)
	if w := callModels(s); w.Code != 503 {
		t.Fatalf("missing helper silently used configured models: %d %s", w.Code, w.Body)
	}
	s.ModelCatalog = func(context.Context, config.LocalAccount, bool) (protocol.ModelCatalog, error) {
		return protocol.ModelCatalog{}, errors.New("fixture private access token " + syntheticClientKey)
	}
	if w := callModels(s); w.Code != 502 || strings.Contains(w.Body.String(), syntheticClientKey) || strings.Contains(w.Body.String(), "private") {
		t.Fatalf("unsafe failed reader: %d %s", w.Code, w.Body)
	}
	s.ModelCatalog = func(context.Context, config.LocalAccount, bool) (protocol.ModelCatalog, error) {
		return protocol.ModelCatalog{Models: []string{"saved-hint"}, ModelCapabilities: protocol.UnknownModelCapabilities([]string{"saved-hint"}, "saved-official-picker-hint")}, nil
	}
	if w := callModels(s); w.Code != 200 || !strings.Contains(w.Body.String(), "saved-official-picker-hint") {
		t.Fatalf("saved hint unavailable: %d %s", w.Code, w.Body)
	}
}

func TestOfficialAPIModelsFenceFinalClientAndAccountChanges(t *testing.T) {
	for _, mutation := range []struct {
		name  string
		apply func(*config.Config)
	}{
		{"disable", func(c *config.Config) { c.CodexAPI.Enabled = false }},
		{"disable-client", func(c *config.Config) { c.CodexAPI.Clients[0].Disabled = true }},
		{"client-policy", func(c *config.Config) { c.CodexAPI.Clients[0].ModelDenylist = []string{"late-hint"} }},
		{"revoke", func(c *config.Config) { c.CodexAPI.Clients = c.CodexAPI.Clients[1:] }},
		{"alias", func(c *config.Config) { c.CodexAPI.Clients[0].ModelAliases["friendly"] = "changed" }},
		{"client-account", func(c *config.Config) { c.CodexAPI.Clients[0].AccountID = "account-B" }},
		{"identity", func(c *config.Config) { c.LocalAccounts[0].CredentialEpoch++ }},
		{"reference", func(c *config.Config) { c.LocalAccounts[0].CredentialRef = "credential-B" }},
		{"policy", func(c *config.Config) { c.LocalAccounts[0].ModelDenylist = []string{"late-hint"} }},
		{"delete", func(c *config.Config) { c.LocalAccounts = c.LocalAccounts[1:] }},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			s, live := fixtureService(t)
			entered, release := make(chan struct{}), make(chan struct{})
			s.ModelCatalog = func(context.Context, config.LocalAccount, bool) (protocol.ModelCatalog, error) {
				close(entered)
				<-release
				return protocol.ModelCatalog{Models: []string{"late-hint"}}, nil
			}
			done := make(chan struct{})
			var w *httptest.ResponseRecorder
			go func() { defer close(done); w = callModels(s) }()
			<-entered
			live.change(mutation.apply)
			close(release)
			awaitDone(t, done)
			if w.Code != 409 || strings.Contains(w.Body.String(), "late-hint") {
				t.Fatalf("stale picker result published: %d %s", w.Code, w.Body)
			}
		})
	}
}
