package hubclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAPISelectionCatalogCannotCrossConnectionEpoch(t *testing.T) {
	for _, scope := range []string{"hub-local", "direct"} {
		t.Run(scope, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var unblock sync.Once
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				<-release
				_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "model-choice"}}})
			}))
			defer provider.Close()
			defer unblock.Do(func() { close(release) })
			account := config.LocalAccount{ID: "fixture-api", Name: "Fixture", Kind: "api", Key: "synthetic", BaseURL: provider.URL}
			c := statusFixture(t, config.Config{LocalAccounts: []config.LocalAccount{account}})
			a := aggregationModelChoice("codex", "codex", true)
			a.settings = c.o.Store
			c.SetManager(&fakeManager{list: []agents.Agent{a}})
			ctx := WithLocalExecution(context.Background())
			if scope == "direct" {
				c.o.Store.SetRuntimeGatewayKey("fixture-runtime")
				ctx = directFixtureContext(t, c)
			}
			before := c.o.Store.Get()
			ctx = WithRemoteSettingsEpoch(ctx, before.RemoteConnectionEpoch)
			done := make(chan error, 1)
			go func() {
				_, err := c.Dispatch(ctx, map[string]any{"type": "agents.api.set", "agent": "codex", "accountId": APIHandle(before, account.ID), "model": "model-choice"})
				done <- err
			}()
			aggregationAwaitProbe(t, entered)
			if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.HubURL = "https://changed.fixture.invalid"; return nil }); err != nil {
				t.Fatal(err)
			}
			if c.o.Store.Get().RemoteConnectionEpoch == before.RemoteConnectionEpoch {
				t.Fatal("fixture epoch did not change")
			}
			unblock.Do(func() { close(release) })
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "变化") {
					t.Fatal("stale API selection committed", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("dispatch did not finish")
			}
			after := c.o.Store.Get()
			if !reflect.DeepEqual(before.RemoteAPI, after.RemoteAPI) || !reflect.DeepEqual(before.DirectAPI, after.DirectAPI) {
				t.Fatal("selection changed across scope")
			}
		})
	}
}
