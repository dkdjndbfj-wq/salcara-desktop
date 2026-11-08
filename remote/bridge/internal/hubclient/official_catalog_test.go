package hubclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

type officialCatalogAgent struct {
	*fakeAgent
	read  func(context.Context) (protocol.ModelCatalog, error)
	calls atomic.Int32
}

func (a *officialCatalogAgent) ModelCatalog(ctx context.Context) (protocol.ModelCatalog, error) {
	a.calls.Add(1)
	return a.read(ctx)
}

func (a *officialCatalogAgent) SendWithOptions(ctx context.Context, id, text string, _ agents.TurnOptions) error {
	return a.Send(ctx, id, text)
}

func officialCatalogFixture(t *testing.T, models []string) (*Client, config.LocalAccount, *officialCatalogAgent, *atomic.Int32) {
	t.Helper()
	requests := &atomic.Int32{}
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		t.Errorf("official catalogue leaked to HTTP: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(trap.Close)
	a := config.LocalAccount{ID: "official", Name: "Synthetic official", Kind: "api", SourceType: "official_account", ProviderID: "openai", CredentialRef: "synthetic-ref", CredentialEpoch: 7, Key: "protected-marker-not-a-token", BaseURL: trap.URL, Wire: "responses", Models: models}
	c := statusFixture(t, config.Config{DeviceID: "fixture", DeviceSecret: "fixture-device", LocalAccounts: []config.LocalAccount{a}, RemoteAPI: map[string]config.RemoteAPI{"codex": {AccountID: a.ID}}})
	reader := &officialCatalogAgent{fakeAgent: &fakeAgent{id: "codex"}, read: func(context.Context) (protocol.ModelCatalog, error) {
		return protocol.ModelCatalog{Models: []string{"native-model", "native-model", " denied ", "bad\nmodel", strings.Repeat("a", 201)}, ModelCapabilities: map[string]protocol.ModelCapability{"native-model": {Source: "codex-model-list", ReasoningKnown: true, ReasoningEfforts: []string{"high"}, InputModalities: []string{"image"}}}}, nil
	}}
	c.SetManager(&fakeManager{list: []agents.Agent{reader}})
	return c, a, reader, requests
}

func TestOfficialCatalogNativeReadCacheRefreshAndUnknownCapabilities(t *testing.T) {
	c, a, reader, requests := officialCatalogFixture(t, nil)
	a.ModelDenylist = []string{"denied"}
	if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.LocalAccounts[0] = a; return nil }); err != nil {
		t.Fatal(err)
	}
	for _, refresh := range []bool{false, false, true, false} {
		catalog, err := c.OfficialModelCatalog(context.Background(), a, refresh)
		if err != nil || !reflect.DeepEqual(catalog.Models, []string{"native-model"}) {
			t.Fatalf("catalog=%+v err=%v", catalog, err)
		}
		capability := catalog.ModelCapabilities["native-model"]
		if capability.Source != OfficialNativeCatalogSource || capability.ReasoningKnown || len(capability.ReasoningEfforts)+len(capability.InputModalities) != 0 {
			t.Fatalf("picker inherited entitlement/capabilities: %+v", capability)
		}
		catalog.Models[0] = "caller-mutation"
	}
	if reader.calls.Load() != 2 || requests.Load() != 0 {
		t.Fatalf("native=%d HTTP=%d", reader.calls.Load(), requests.Load())
	}
	if !reflect.DeepEqual(c.o.Store.Get().LocalAccounts[0].Models, []string{"native-model"}) {
		t.Fatal("native picker hints not safely persisted")
	}
	if len(reader.started)+len(reader.sent) != 0 {
		t.Fatal("catalogue started a turn")
	}
}

func TestOfficialCatalogSavedHintsAndRefreshNeverFallBack(t *testing.T) {
	c, a, reader, requests := officialCatalogFixture(t, []string{"saved-model"})
	reader.read = func(context.Context) (protocol.ModelCatalog, error) {
		return protocol.ModelCatalog{}, errors.New("secret-fixture-access-token C:\\private\\auth.json")
	}
	catalog, err := c.OfficialModelCatalog(context.Background(), a, false)
	if err != nil || reader.calls.Load() != 0 || catalog.ModelCapabilities["saved-model"].Source != OfficialSavedCatalogSource {
		t.Fatalf("saved=%+v reads=%d err=%v", catalog, reader.calls.Load(), err)
	}
	for _, refresh := range []bool{true, false} {
		catalog, err = c.OfficialModelCatalog(context.Background(), a, refresh)
		if err == nil || len(catalog.Models) != 0 || strings.Contains(err.Error(), "secret-fixture") || strings.Contains(err.Error(), "private") {
			t.Fatalf("unsafe refresh=%v catalog=%+v err=%v", refresh, catalog, err)
		}
	}
	if reader.calls.Load() != 2 || requests.Load() != 0 {
		t.Fatal("failed refresh used cached hints or HTTP fallback")
	}
}

func TestOfficialCatalogUnavailableCannotUseAnotherAgent(t *testing.T) {
	for _, manager := range []agents.Manager{nil, &fakeManager{list: []agents.Agent{&fakeAgent{id: "codex"}}}, &fakeManager{list: []agents.Agent{&officialCatalogAgent{fakeAgent: &fakeAgent{id: "other"}, read: func(context.Context) (protocol.ModelCatalog, error) {
		t.Fatal("used another account's reader")
		return protocol.ModelCatalog{}, nil
	}}}}} {
		c, a, _, requests := officialCatalogFixture(t, nil)
		c.SetManager(manager)
		if _, err := c.OfficialModelCatalog(context.Background(), a, true); err == nil || !strings.Contains(err.Error(), "不可用") {
			t.Fatalf("unavailable error=%v", err)
		}
		if requests.Load() != 0 {
			t.Fatal("unavailable reader called provider")
		}
	}
}

func TestOfficialCatalogFencesIdentityAndConfigurationChanges(t *testing.T) {
	for _, mutation := range []struct {
		name  string
		apply func(*config.Config)
	}{
		{"credential-ref", func(cfg *config.Config) { cfg.LocalAccounts[0].CredentialRef = "replacement-ref" }},
		{"identity-epoch", func(cfg *config.Config) { cfg.LocalAccounts[0].CredentialEpoch++ }},
		{"source", func(cfg *config.Config) { cfg.LocalAccounts[0].SourceType = "oauth" }},
		{"policy", func(cfg *config.Config) { cfg.LocalAccounts[0].ModelDenylist = []string{"native-model"} }},
		{"codex-path", func(cfg *config.Config) { cfg.LocalToolPaths = map[string]string{"codex": "fixture-replaced-cli"} }},
		{"binding", func(cfg *config.Config) { cfg.RemoteAPI["codex"] = config.RemoteAPI{AccountID: "other"} }},
		{"deleted", func(cfg *config.Config) { cfg.LocalAccounts = nil }},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			c, a, reader, requests := officialCatalogFixture(t, nil)
			entered, release := make(chan struct{}), make(chan struct{})
			reader.read = func(context.Context) (protocol.ModelCatalog, error) {
				close(entered)
				<-release
				return protocol.ModelCatalog{Models: []string{"late-model"}}, nil
			}
			done := make(chan error, 1)
			go func() { _, err := c.OfficialModelCatalog(context.Background(), a, true); done <- err }()
			<-entered
			if err := c.o.Store.Update(func(cfg *config.Config) error { mutation.apply(cfg); return nil }); err != nil {
				t.Fatal(err)
			}
			close(release)
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("published a stale catalogue")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("catalogue hung")
			}
			for _, current := range c.o.Store.Get().LocalAccounts {
				if containsKey(current.Models, "late-model") {
					t.Fatal("stale model persisted")
				}
			}
			if requests.Load() != 0 {
				t.Fatal("identity fence leaked to HTTP")
			}
		})
	}
}

func TestOfficialCatalogFailedRefreshFencesOlderRead(t *testing.T) {
	c, a, reader, _ := officialCatalogFixture(t, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	reader.read = func(context.Context) (protocol.ModelCatalog, error) {
		if reader.calls.Load() == 1 {
			close(entered)
			<-release
			return protocol.ModelCatalog{Models: []string{"older-model"}}, nil
		}
		return protocol.ModelCatalog{}, errors.New("synthetic failure")
	}
	done := make(chan error, 1)
	go func() { _, err := c.OfficialModelCatalog(context.Background(), a, false); done <- err }()
	<-entered
	if _, err := c.OfficialModelCatalog(context.Background(), a, true); err == nil {
		t.Fatal("explicit refresh succeeded")
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("older read defeated failed refresh")
	}
	if len(c.o.Store.Get().LocalAccounts[0].Models) != 0 {
		t.Fatal("older result persisted")
	}
}

func TestOfficialExplicitModelDoesNotRequireCatalogueMembership(t *testing.T) {
	c, a, reader, requests := officialCatalogFixture(t, []string{"older-hint"})
	a.ModelAllowlist = []string{"new-explicit-model", "blocked-model"}
	a.ModelDenylist = []string{"blocked-model"}
	if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.LocalAccounts[0] = a; return nil }); err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"", "blocked-model", "unlisted-model", "bad\nmodel", strings.Repeat("a", 201)} {
		if _, err := c.validateManagedModel(context.Background(), c.o.Store.Get(), "codex", model); err == nil {
			t.Fatalf("invalid model allowed: %q", model)
		}
	}
	if _, err := c.validateManagedModel(context.Background(), c.o.Store.Get(), "codex", "new-explicit-model"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Dispatch(context.Background(), map[string]any{"type": "agents.api.set", "agent": "codex", "accountId": APIHandle(c.o.Store.Get(), a.ID), "model": "new-explicit-model"}); err != nil {
		t.Fatal(err)
	}
	if reader.calls.Load() != 0 || requests.Load() != 0 {
		t.Fatal("explicit official model required catalogue/provider call")
	}
}

func TestOfficialModelsDispatchLabelsPickerHints(t *testing.T) {
	c, _, reader, requests := officialCatalogFixture(t, nil)
	result, err := c.Dispatch(context.Background(), map[string]any{"type": "models.list", "tool": "codex", "refresh": true})
	if err != nil {
		t.Fatal(err)
	}
	response := result.(map[string]any)
	if response["nativeSource"] != OfficialNativeCatalogSource {
		t.Fatalf("missing native picker source: %v", response)
	}
	if reader.calls.Load() != 1 || requests.Load() != 0 {
		t.Fatal("dispatch did not use native reader only")
	}
}

func TestOfficialModelsRejectMalformedReferencesWithoutHTTP(t *testing.T) {
	for _, mutation := range []func(*config.LocalAccount){
		func(a *config.LocalAccount) { a.CredentialRef = "" },
		func(a *config.LocalAccount) { a.SourceType = "oauth" },
		func(a *config.LocalAccount) { a.ProviderID = "other" },
	} {
		c, a, reader, requests := officialCatalogFixture(t, nil)
		mutation(&a)
		if _, err := c.OfficialModelCatalog(context.Background(), a, true); err == nil {
			t.Fatal("malformed reference used native catalogue")
		}
		if err := validateOfficialModel(a, "explicit-model"); err == nil {
			t.Fatal("malformed reference allowed official catalogue bypass")
		}
		if reader.calls.Load()+requests.Load() != 0 {
			t.Fatal("invalid reference reached reader/provider")
		}
	}
}
