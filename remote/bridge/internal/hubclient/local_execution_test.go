package hubclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

type localContinuationAgent struct {
	*modelOptionsAgent
	continued []agents.TurnOptions
}

func (a *localContinuationAgent) Descriptor() protocol.AgentDescriptor {
	d := aggregationDescriptor(a.ID())
	d.Capabilities.Continue = true
	return d
}

func (a *localContinuationAgent) ContinueWithOptions(_ context.Context, _, _, _, _ string, opts agents.TurnOptions) (protocol.ContinueResult, error) {
	a.continued = append(a.continued, opts)
	return protocol.ContinueResult{Accepted: true}, nil
}

func TestDirectExecutionUsesAppliedCatalogWhilePhoneSelectionDiffers(t *testing.T) {
	for _, family := range []string{"codex", "claude"} {
		t.Run(family, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer local-synthetic" {
					t.Error("local catalog used another source")
				}
				_, _ = w.Write([]byte(`{"data":[{"id":"local-model"}]}`))
			}))
			defer upstream.Close()
			var phoneCalls atomic.Int32
			phoneUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				phoneCalls.Add(1)
				_, _ = w.Write([]byte(`{"data":[{"id":"phone-model"}]}`))
			}))
			defer phoneUpstream.Close()
			local := config.LocalAccount{ID: "local", Kind: "api", SourceType: "api_key", BaseURL: upstream.URL, Key: "local-synthetic"}
			phone := config.LocalAccount{ID: "phone", Kind: "api", SourceType: "api_key", BaseURL: phoneUpstream.URL, Key: "phone-synthetic"}
			cwd := t.TempDir()
			c := statusFixture(t, config.Config{LocalAccounts: []config.LocalAccount{local, phone}, RemoteAPI: map[string]config.RemoteAPI{family: {AccountID: phone.ID}}, Projects: []protocol.Project{{Path: cwd}}})
			c.o.Store.SetRuntimeGatewayKey("synthetic-process")
			if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.RecordAppliedAPI(family, local, ""); return nil }); err != nil {
				t.Fatal(err)
			}
			worker := &localContinuationAgent{modelOptionsAgent: &modelOptionsAgent{fakeAgent: &fakeAgent{id: family}}}
			c.SetManager(&fakeManager{list: []agents.Agent{worker}})
			ctx := directFixtureContext(t, c)
			if !IsDirectExecution(ctx) || !isLocalExecution(ctx) {
				t.Fatal("direct execution marker did not select isolated local execution")
			}
			result, err := c.Dispatch(ctx, map[string]any{"type": "models.list", "tool": family})
			if err != nil || len(result.(map[string]any)["models"].([]string)) != 1 {
				t.Fatalf("local models unavailable: %v", err)
			}
			phoneHandle := APIHandle(c.o.Store.Get(), phone.ID)
			if _, err := c.Dispatch(ctx, map[string]any{"type": "session.start", "tool": family, "cwd": cwd, "prompt": "local fixture", "model": "local-model", "accountId": phoneHandle}); err != nil {
				t.Fatalf("local start required a phone station: %v", err)
			}
			if _, err := c.Dispatch(ctx, map[string]any{"type": "session.send", "sessionKey": family + ":original", "text": "local fixture", "model": "local-model", "accountId": phoneHandle}); err != nil {
				t.Fatalf("local continuation required a phone station: %v", err)
			}
			if _, err := c.Dispatch(ctx, map[string]any{"type": "session.continue", "sessionKey": family + ":original", "text": "local fixture", "model": "local-model", "expectedTurnId": "completed-turn", "operationId": "11111111-2222-4333-8444-555555555555"}); err != nil {
				t.Fatalf("local terminal continuation used the phone selection: %v", err)
			}
			if len(worker.started) != 1 || len(worker.sent) != 1 || len(worker.continued) != 1 || worker.continued[0].Model != "local-model" || calls.Load() != 1 || phoneCalls.Load() != 0 || c.o.Store.Get().RemoteAPI[family].AccountID != phone.ID {
				t.Fatal("local execution changed the phone selection or lost a legitimate operation")
			}
		})
	}
}

func TestLocalExecutionMarkerCannotOverrideRemoteSource(t *testing.T) {
	c, store, _, env := sourceGuardFixture(t, "api_key")
	cfg := store.Get()
	for _, marker := range []context.Context{WithLocalExecution(context.Background()), WithDirectExecution(context.Background())} {
		if !isLocalExecution(marker) {
			t.Fatal("local execution marker unavailable")
		}
		for _, ctx := range []context.Context{withExecutionSource(marker, env), withCommandGuard(marker, commandGuard{})} {
			if isLocalExecution(ctx) || executionConfig(ctx, cfg).RemoteAPI["codex"].AccountID == "" {
				t.Fatal("local marker overrode an admitted remote command")
			}
		}
	}
	env.Source.Station = &protocol.StationPolicy{Version: 1, APIBases: []string{"https://different.invalid"}}
	env.Command["localExecution"], env.Command["localSettings"] = true, true
	if _, err := c.Dispatch(withExecutionSource(WithDirectExecution(context.Background()), env), env.Command); err == nil {
		t.Fatal("spoofed local flags bypassed station isolation")
	}
}
