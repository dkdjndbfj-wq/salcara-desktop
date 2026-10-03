package hubclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func TestConversationEntryRefreshDetectsSameKeyGroupChangesAndKeepsThread(t *testing.T) {
	for _, family := range []string{"codex", "claude"} {
		for _, source := range []string{"phone", "applied"} {
			for _, transition := range []string{"claude-to-gpt", "gpt-to-claude"} {
				t.Run(family+"/"+source+"/"+transition, func(t *testing.T) {
					oldModel, newModel := "claude-old", "gpt-new"
					if transition == "gpt-to-claude" {
						oldModel, newModel = "gpt-old", "claude-new"
					}
					var group atomic.Value
					group.Store(oldModel)
					var requests atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						requests.Add(1)
						if r.Method != http.MethodGet || r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer fake-same-key" {
							t.Errorf("unexpected catalog request: %s", r.URL.Path)
						}
						_, _ = w.Write([]byte(`{"data":[{"id":"` + group.Load().(string) + `"}]}`))
					}))
					defer server.Close()
					a := config.LocalAccount{ID: "same", Name: "Same API", Kind: "api", BaseURL: server.URL, Key: "fake-same-key", Model: oldModel, Wire: "auto"}
					cfg := config.Config{DeviceID: "pc", DeviceSecret: "fake-device", GatewayKey: "fake-loopback", LocalAccounts: []config.LocalAccount{a}}
					if source == "phone" {
						_ = cfg.SetRemoteAPI(family, a.ID, "")
					} else {
						effective := cfg.APIForTool(a, family)
						effective.Protocol = config.InferProtocol(oldModel, a.Wire)
						cfg.RecordAppliedAPI(family, effective, "original-fixture-provider")
					}
					originalBinding := cfg.Clone().ToolAPIApplied
					key := family + ":11111111-2222-4333-8444-555555555555"
					c := statusFixture(t, cfg)
					worker := &modelOptionsAgent{fakeAgent: &fakeAgent{id: family, sessions: []protocol.SessionInfo{{SessionKey: key, Status: "idle", Model: oldModel}}}}
					c.SetManager(&fakeManager{list: []agents.Agent{worker}})
					list := func(refresh bool) []string {
						res, err := c.Dispatch(context.Background(), map[string]any{"type": "models.list", "tool": family, "refresh": refresh})
						if err != nil {
							t.Fatal(err)
						}
						return res.(map[string]any)["models"].([]string)
					}
					if got := list(true); !reflect.DeepEqual(got, []string{oldModel}) {
						t.Fatal("initial group unavailable")
					}
					before := agents.SettingsWithGateway(c.o.Store.Get(), 12345)
					group.Store(newModel) // The provider changes only its group, not our key/URL.
					if got := list(false); !reflect.DeepEqual(got, []string{oldModel}) || requests.Load() != 1 {
						t.Fatal("test did not exercise the still-valid five-minute cache")
					}
					if got := list(true); !reflect.DeepEqual(got, []string{newModel}) || requests.Load() != 2 {
						t.Fatal("entry refresh missed a same-key group change")
					}
					if _, err := c.Dispatch(context.Background(), map[string]any{"type": "session.send", "sessionKey": key, "text": "retired model must not run", "model": oldModel}); err == nil {
						t.Fatal("retired model escaped after fresh entry validation")
					}
					if len(worker.sent) != 0 {
						t.Fatal("retired model resumed a worker")
					}
					if _, err := c.Dispatch(context.Background(), map[string]any{"type": "session.send", "sessionKey": key, "text": "continue same thread", "model": newModel}); err != nil {
						t.Fatal(err)
					}
					if requests.Load() != 2 || len(worker.sent) != 1 || worker.sent[0] != strings.TrimPrefix(key, family+":")+"|continue same thread" || worker.sendOptions[0].Model != newModel || len(worker.started)+len(worker.stopped)+len(worker.opened) != 0 {
						t.Fatal("new model replaced the original thread or added per-message polling")
					}
					stored := c.o.Store.Get()
					if stored.LocalAccounts[0].Key != a.Key || stored.LocalAccounts[0].BaseURL != a.BaseURL || !reflect.DeepEqual(originalBinding, stored.ToolAPIApplied) {
						t.Fatal("catalog refresh rewrote the original account or desktop binding")
					}
					after := agents.SettingsWithGateway(stored, 12345)
					if family == "codex" && after.CodexModel != "" || family == "claude" && after.ClaudeModel != "" {
						t.Fatal("worker kept the retired default")
					}
					if config.InferProtocol(newModel, a.Wire) != config.NativeProtocol(family) {
						route := family
						if source == "phone" {
							route = "remote-" + family
						}
						root := "http://127.0.0.1:12345/gateway/" + route
						if family == "codex" && (after.CodexRoot != root || after.UseOriginalCodex) || family == "claude" && after.ClaudeRoot != root {
							t.Fatalf("new cross-family catalog bypassed conversion: before=%+v after=%+v", before, after)
						}
					}
				})
			}
		}
	}
}
