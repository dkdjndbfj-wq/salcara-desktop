package hubclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

type modelOptionsAgent struct {
	*fakeAgent
	startOptions []agents.TurnOptions
	sendOptions  []agents.TurnOptions
}

func (a *modelOptionsAgent) StartWithOptions(ctx context.Context, cwd, prompt, approval string, opts agents.TurnOptions) (string, error) {
	a.startOptions = append(a.startOptions, opts)
	return a.fakeAgent.Start(ctx, cwd, prompt, opts.Model, approval)
}

func (a *modelOptionsAgent) SendWithOptions(ctx context.Context, id, text string, opts agents.TurnOptions) error {
	a.sendOptions = append(a.sendOptions, opts)
	return a.fakeAgent.Send(ctx, id, text)
}

func TestPhoneModelCatalogOnlyReturnsFreshCatalogMembers(t *testing.T) {
	var requests, status atomic.Int32
	status.Store(http.StatusOK)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer fake-new-key" {
			t.Errorf("wrong model request: %s", r.URL.Path)
		}
		if s := int(status.Load()); s != http.StatusOK {
			w.WriteHeader(s)
			_, _ = w.Write([]byte(`{"error":"never surface this provider body"}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"new-model"},{"id":"new-model"}]}`))
	}))
	defer server.Close()
	a := config.LocalAccount{ID: "new", Name: "New API", Kind: "api", Key: "fake-new-key", BaseURL: server.URL,
		Model: "invalid-saved-default", Models: []string{"old-persisted-catalog"}}
	c := statusFixture(t, config.Config{LocalAccounts: []config.LocalAccount{a}, RemoteAPI: map[string]config.RemoteAPI{"codex": {AccountID: a.ID, Model: a.Model}}})
	worker := &modelOptionsAgent{fakeAgent: &fakeAgent{id: "codex"}}
	c.SetManager(&fakeManager{list: []agents.Agent{worker}})
	list := func(refresh bool) ([]string, error) {
		res, err := c.Dispatch(context.Background(), map[string]any{"type": "models.list", "tool": "codex", "refresh": refresh})
		if err != nil {
			return nil, err
		}
		return res.(map[string]any)["models"].([]string), nil
	}
	for i := 0; i < 2; i++ {
		models, err := list(false)
		if err != nil || !reflect.DeepEqual(models, []string{"new-model"}) {
			t.Fatalf("catalog=%v error=%v", models, err)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("normal list did not reuse verified cache: %d", requests.Load())
	}
	if _, err := c.Dispatch(context.Background(), map[string]any{"type": "session.send", "sessionKey": "codex:original", "text": "continue", "model": "new-model"}); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 || len(worker.sent) != 1 {
		t.Fatal("a turn should use the recently verified catalog, not fetch on every send")
	}
	status.Store(http.StatusUnauthorized)
	if _, err := list(true); err == nil || strings.Contains(err.Error(), "provider body") {
		t.Fatalf("refresh did not safely fail: %v", err)
	}
	if _, err := c.Dispatch(context.Background(), map[string]any{"type": "session.send", "sessionKey": "codex:original", "text": "do not send", "model": "new-model"}); err == nil {
		t.Fatal("failed refresh silently reused old verified catalog")
	}
	if len(worker.sent) != 1 || requests.Load() != 3 {
		t.Fatalf("refresh failure escaped: sends=%v requests=%d", worker.sent, requests.Load())
	}
}

func TestPhoneChangingAPIRequiresSupportedModelBeforeStartOrResume(t *testing.T) {
	for _, family := range []string{"codex", "claude"} {
		t.Run(family, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				_, _ = w.Write([]byte(`{"data":[{"id":"deepseek-chat"}]}`))
			}))
			defer server.Close()
			oldAPI := config.LocalAccount{ID: "old", Kind: "api", Key: "fake-old", BaseURL: server.URL, Model: "old-key-model"}
			newAPI := config.LocalAccount{ID: "new", Kind: "api", Key: "fake-new", BaseURL: server.URL, Model: "invalid-vault-default"}
			cwd := filepath.Clean(t.TempDir())
			cfg := config.Config{
				DeviceID: "pc", DeviceSecret: "fake-device-secret", LocalAccounts: []config.LocalAccount{oldAPI, newAPI},
				CodexModel: "legacy-model", ClaudeModel: "legacy-model", Projects: []protocol.Project{{Path: cwd}},
				ToolAPISelections: map[string]string{family: "old", "codex-desktop": "old"},
				ToolModels:        map[string]string{family: "old-key-model", "codex-desktop": "old-key-model"},
				RemoteAPI:         map[string]config.RemoteAPI{family: {AccountID: "old", Model: "old-key-model"}},
			}
			c := statusFixture(t, cfg)
			worker := &modelOptionsAgent{fakeAgent: &fakeAgent{id: family, sessions: []protocol.SessionInfo{{SessionKey: family + ":original", Status: "idle", Model: "old-key-model"}}}}
			c.SetManager(&fakeManager{list: []agents.Agent{worker}})
			if _, err := c.Dispatch(context.Background(), map[string]any{"type": "agents.api.set", "agent": family, "accountId": APIHandle(cfg, "new"), "sessionKey": family + ":original"}); err != nil {
				t.Fatal(err)
			}
			stored := c.o.Store.Get()
			if _, set := stored.ToolModels[family]; set {
				t.Fatal("key-only selection kept old tool model")
			}
			if family == "codex" {
				if _, set := stored.ToolModels["codex-desktop"]; set {
					t.Fatal("Codex desktop card kept the old model")
				}
			}
			if account, _ := stored.RemoteToolAccount(family); account.Model != "" {
				t.Fatalf("new key inherited a default: %q", account.Model)
			}
			settings := agents.SettingsFromConfig(stored)
			if (family == "codex" && settings.CodexModel != "") || (family == "claude" && settings.ClaudeModel != "") {
				t.Fatal("worker inherited legacy or vault default")
			}
			for _, model := range []string{"", "old-key-model", "invalid-vault-default", "legacy-model"} {
				for _, typ := range []string{"session.start", "session.send"} {
					cmd := map[string]any{"type": typ, "tool": family, "cwd": cwd, "prompt": "new task", "sessionKey": family + ":original", "text": "continue", "model": model}
					if _, err := c.Dispatch(context.Background(), cmd); err == nil {
						t.Fatalf("%s allowed unavailable model %q", typ, model)
					}
				}
			}
			if len(worker.sent)+len(worker.started)+len(worker.opened)+len(worker.stopped) != 0 {
				t.Fatal("validation started, resumed or changed the original thread")
			}
			for _, typ := range []string{"session.start", "session.send"} {
				if _, err := c.Dispatch(context.Background(), map[string]any{"type": typ, "tool": family, "cwd": cwd, "prompt": "new task", "sessionKey": family + ":original", "text": "continue", "model": "deepseek-chat"}); err != nil {
					t.Fatal(err)
				}
			}
			if len(worker.startOptions) != 1 || worker.startOptions[0].Model != "deepseek-chat" || len(worker.sendOptions) != 1 || worker.sendOptions[0].Model != "deepseek-chat" || worker.sent[0] != "original|continue" {
				t.Fatalf("new model not sent to the original session: %+v %+v %v", worker.startOptions, worker.sendOptions, worker.sent)
			}
			if requests.Load() != 1 {
				t.Fatalf("validation did not reuse a verified current-key catalog: %d", requests.Load())
			}
			settings = agents.SettingsWithGateway(c.o.Store.Get(), 12345)
			if family == "codex" && settings.CodexRoot != "http://127.0.0.1:12345/gateway/remote-codex" {
				t.Fatalf("cleared default bypassed cross-family conversion: %s", settings.CodexRoot)
			}
			if family == "claude" && settings.ClaudeRoot != "http://127.0.0.1:12345/gateway/remote-claude" {
				t.Fatalf("cleared default bypassed cross-family conversion: %s", settings.ClaudeRoot)
			}
		})
	}
}

func TestPhoneModelValidationDoesNotChangeToolLoginOrNativeDesktop(t *testing.T) {
	cwd := t.TempDir()
	// A pending card selection is not an applied worker API.
	a := config.LocalAccount{ID: "pending", Kind: "api", Key: "fake", BaseURL: "https://fixture.invalid"}
	c := statusFixture(t, config.Config{LocalAccounts: []config.LocalAccount{a}, ToolAPISelections: map[string]string{"codex": a.ID}, Projects: []protocol.Project{{Path: cwd}}})
	worker := &modelOptionsAgent{fakeAgent: &fakeAgent{id: "codex"}}
	c.SetManager(&fakeManager{list: []agents.Agent{worker}})
	res, err := c.Dispatch(context.Background(), map[string]any{"type": "models.list", "tool": "codex"})
	if err != nil || !reflect.DeepEqual(res.(map[string]any)["models"], []string{"m1"}) {
		t.Fatalf("pending card selection replaced the tool login catalog: result=%v error=%v", res, err)
	}
	if _, err := c.Dispatch(context.Background(), map[string]any{"type": "session.send", "sessionKey": "codex:original", "text": "tool login"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Dispatch(context.Background(), map[string]any{"type": "session.start", "tool": "codex", "cwd": cwd, "prompt": "tool login"}); err != nil {
		t.Fatal(err)
	}
	key := "codex:01a0ae56-e9f6-4933-9ad4-5e08cd7874e5"
	d := &routeDesktop{st: liveDesktop(key)}
	c.o.Desktop = d
	if err := c.o.Store.Update(func(cfg *config.Config) error { return cfg.SetRemoteAPI("codex", a.ID, "") }); err != nil {
		t.Fatal(err)
	}
	res, err = c.Dispatch(context.Background(), map[string]any{"type": "session.send", "sessionKey": key, "text": "native owns this model", "controlSurface": "desktop"})
	if err != nil || res.(map[string]any)["via"] != "desktop" || len(d.sent) != 1 || len(worker.sent) != 1 {
		t.Fatalf("native model selection was incorrectly gated: result=%v error=%v", res, err)
	}
}

func TestPhoneCatalogCredentialsChangedDuringReadCannotPublish(t *testing.T) {
	for _, change := range []string{"key", "auth", "wire"} {
		t.Run(change, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				<-release
				_, _ = w.Write([]byte(`{"data":[{"id":"old-key-catalog"}]}`))
			}))
			defer server.Close()
			a := config.LocalAccount{ID: "same-id", Kind: "api", BaseURL: server.URL, Key: "fake-old", Models: []string{"old-persisted"}}
			c := statusFixture(t, config.Config{LocalAccounts: []config.LocalAccount{a}, RemoteAPI: map[string]config.RemoteAPI{"codex": {AccountID: a.ID}}})
			c.SetManager(&fakeManager{list: []agents.Agent{&fakeAgent{id: "codex"}}})
			done := make(chan error, 1)
			go func() {
				_, err := c.Dispatch(context.Background(), map[string]any{"type": "models.list", "tool": "codex"})
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				close(release)
				t.Fatal("catalog did not start")
			}
			if err := c.o.Store.Update(func(cfg *config.Config) error {
				switch change {
				case "key":
					cfg.LocalAccounts[0].Key = "fake-new"
				case "auth":
					cfg.LocalAccounts[0].AuthMode = "api-key"
				case "wire":
					cfg.LocalAccounts[0].Wire = "chat"
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			close(release)
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("old credentials published a model catalog")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("catalog hung")
			}
			if got := c.o.Store.Get().LocalAccounts[0].Models; !reflect.DeepEqual(got, []string{"old-persisted"}) {
				t.Fatalf("old catalog overwrote changed API: %v", got)
			}
			if len(c.modelCatalog[a.ID].models) != 0 {
				t.Fatal("old credential catalog cached after a vault edit")
			}
		})
	}
}

func TestPhoneOlderCatalogReadCannotRestoreFailedRefresh(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			close(entered)
			<-release
			_, _ = w.Write([]byte(`{"data":[{"id":"stale-result"}]}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	a := config.LocalAccount{ID: "same-id", Kind: "api", BaseURL: server.URL, Key: "fake"}
	c := statusFixture(t, config.Config{LocalAccounts: []config.LocalAccount{a}})
	done := make(chan error, 1)
	go func() { _, err := c.verifiedAPIModels(context.Background(), a, false); done <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("catalog did not start")
	}
	if _, err := c.verifiedAPIModels(context.Background(), a, true); err == nil {
		t.Fatal("refresh failure accepted")
	}
	close(release)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("older success replaced failed explicit refresh")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("catalog hung")
	}
	if len(c.modelCatalog[a.ID].models) != 0 || len(c.o.Store.Get().LocalAccounts[0].Models) != 0 {
		t.Fatal("older catalog persisted after explicit refresh")
	}
}

func TestPhoneDeletedRemoteAPIRequiresReplacementNotOldToolFallback(t *testing.T) {
	c := statusFixture(t, config.Config{RemoteAPI: map[string]config.RemoteAPI{"codex": {AccountID: "deleted"}}})
	a := &fakeAgent{id: "codex"}
	c.SetManager(&fakeManager{list: []agents.Agent{a}})
	for _, typ := range []string{"session.send", "models.list"} {
		if _, err := c.Dispatch(context.Background(), map[string]any{"type": typ, "tool": "codex", "sessionKey": "codex:original", "text": "do not resume another API", "model": "old-model"}); err == nil {
			t.Fatalf("%s fell back from deleted selected API", typ)
		}
	}
	if len(a.sent)+len(a.started) != 0 {
		t.Fatal("deleted selected API resumed with old credentials")
	}
}

func TestPhoneFreshClaudeGPTCatalogUsesConversionWithoutDefaultModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5-codex"}]}`))
	}))
	defer server.Close()
	a := config.LocalAccount{ID: "gpt-api", Kind: "api", BaseURL: server.URL, Key: "fake-gpt", Models: []string{"claude-old"}}
	c := statusFixture(t, config.Config{LocalAccounts: []config.LocalAccount{a}, GatewayKey: "fake-loopback", RemoteAPI: map[string]config.RemoteAPI{"claude": {AccountID: a.ID}}})
	if _, err := c.verifiedAPIModels(context.Background(), a, false); err != nil {
		t.Fatal(err)
	}
	s := agents.SettingsWithGateway(c.o.Store.Get(), 12345)
	if s.ClaudeRoot != "http://127.0.0.1:12345/gateway/remote-claude" || s.ClaudeKey != "fake-loopback" || s.ClaudeModel != "" {
		t.Fatalf("fresh GPT catalog should route Claude through conversion, without picking a default: %+v", s)
	}
}

func TestPhoneUnsupportedOrUnavailableCatalogCannotUseSavedModels(t *testing.T) {
	for _, response := range []string{`{"data":[]}`, `{"error":"no catalog"}`} {
		t.Run(response, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(response, "error") {
					w.WriteHeader(http.StatusNotFound)
				}
				_, _ = w.Write([]byte(response))
			}))
			defer server.Close()
			a := config.LocalAccount{ID: "api", Kind: "api", BaseURL: server.URL, Key: "fake", Model: "saved", Models: []string{"saved"}}
			c := statusFixture(t, config.Config{LocalAccounts: []config.LocalAccount{a}, RemoteAPI: map[string]config.RemoteAPI{"codex": {AccountID: a.ID}}})
			worker := &modelOptionsAgent{fakeAgent: &fakeAgent{id: "codex"}}
			c.SetManager(&fakeManager{list: []agents.Agent{worker}})
			for _, typ := range []string{"models.list", "session.send"} {
				if _, err := c.Dispatch(context.Background(), map[string]any{"type": typ, "tool": "codex", "sessionKey": "codex:original", "text": "do not run", "model": "saved"}); err == nil {
					t.Fatalf("%s used saved model after absent/empty catalog", typ)
				}
			}
			if len(worker.sent)+len(worker.started) != 0 {
				t.Fatal("missing provider catalog started a worker")
			}
		})
	}
}

func TestPhoneAppliedAPIModelsAreValidatedWithoutUsingLegacyDefault(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-new"}]}`))
	}))
	defer server.Close()
	a := config.LocalAccount{ID: "applied", Name: "Applied API", Kind: "api", BaseURL: server.URL, Key: "fake", Model: "gpt-old"}
	cfg := config.Config{LocalAccounts: []config.LocalAccount{a}, CodexModel: "legacy", Projects: []protocol.Project{{Path: t.TempDir()}}}
	cfg.RecordAppliedAPI("codex", cfg.APIForTool(a, "codex"), "fixture-provider")
	c := statusFixture(t, cfg)
	worker := &modelOptionsAgent{fakeAgent: &fakeAgent{id: "codex"}}
	c.SetManager(&fakeManager{list: []agents.Agent{worker}})
	for _, model := range []string{"", "gpt-old", "legacy"} {
		if _, err := c.Dispatch(context.Background(), map[string]any{"type": "session.start", "tool": "codex", "cwd": cfg.Projects[0].Path, "prompt": "task", "model": model}); err == nil {
			t.Fatalf("applied API reused invalid fallback %q", model)
		}
	}
	if _, err := c.Dispatch(context.Background(), map[string]any{"type": "session.send", "sessionKey": "codex:original", "text": "continue", "model": "gpt-new"}); err != nil {
		t.Fatal(err)
	}
	if len(worker.started) != 0 || len(worker.sendOptions) != 1 || worker.sendOptions[0].Model != "gpt-new" {
		t.Fatal("applied API validation did not preserve the actual session")
	}
}

func TestPhoneExpiredCatalogAndReselectedKeyNeedFreshModels(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"data":[{"id":"new-model"}]}`))
	}))
	defer server.Close()
	a := config.LocalAccount{ID: "same", Kind: "api", BaseURL: server.URL, Key: "fake"}
	cfg := config.Config{DeviceID: "pc", DeviceSecret: "secret", LocalAccounts: []config.LocalAccount{a}, RemoteAPI: map[string]config.RemoteAPI{"codex": {AccountID: a.ID}}}
	c := statusFixture(t, cfg)
	c.SetManager(&fakeManager{list: []agents.Agent{&fakeAgent{id: "codex"}}})
	for i := 0; i < 2; i++ {
		if _, err := c.verifiedAPIModels(context.Background(), a, false); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			c.modelCatalogMu.Lock()
			cached := c.modelCatalog[a.ID]
			cached.expiresAt = time.Now().Add(-time.Second)
			c.modelCatalog[a.ID] = cached
			c.modelCatalogMu.Unlock()
		}
	}
	if requests.Load() != 2 {
		t.Fatal("expired cache was reused")
	}
	if _, err := c.Dispatch(context.Background(), map[string]any{"type": "agents.api.set", "agent": "codex", "accountId": APIHandle(cfg, a.ID)}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Dispatch(context.Background(), map[string]any{"type": "models.list", "tool": "codex"}); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 3 {
		t.Fatal("reselected key silently reused its previous catalog")
	}
}
