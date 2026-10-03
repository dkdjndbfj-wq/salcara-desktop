package hubclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/desktopcompanion"
	"salcara/bridge/internal/protocol"
)

type effortMetadataAgent struct {
	*modelOptionsAgent
	catalog    *protocol.ModelCatalog
	catalogErr error
	knownModel string
}

func (a *effortMetadataAgent) KnownSessionModel(string) string { return a.knownModel }

func effortClient(t *testing.T) (*Client, *effortMetadataAgent, string) {
	t.Helper()
	c, a := surfaceClient(t)
	cwd := filepath.Clean(t.TempDir())
	if err := c.o.Store.Update(func(cfg *config.Config) error {
		cfg.Projects = []protocol.Project{{Path: cwd}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	worker := &effortMetadataAgent{modelOptionsAgent: &modelOptionsAgent{fakeAgent: a}}
	c.SetManager(&fakeManager{list: []agents.Agent{worker}})
	return c, worker, cwd
}

// The fixture explicitly reports per-model metadata; an enum alone no longer
// certifies that every model/provider accepts the same effort values.
func (a *effortMetadataAgent) ModelCatalog(context.Context) (protocol.ModelCatalog, error) {
	if a.catalogErr != nil {
		return protocol.ModelCatalog{}, a.catalogErr
	}
	if a.catalog != nil {
		return *a.catalog, nil
	}
	return protocol.ModelCatalog{Models: []string{"effort-fixture"}, ModelCapabilities: map[string]protocol.ModelCapability{
		"effort-fixture": {Source: "codex-model-list", ReasoningKnown: true, ReasoningEfforts: []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}},
	}}, nil
}

func TestPhoneEffortRequiresExactCurrentModelEvidenceBeforeWorkerStarts(t *testing.T) {
	for _, fixture := range []struct {
		name       string
		models     []string
		capability protocol.ModelCapability
		catalogErr error
	}{
		{"unknown-model", []string{"other-model"}, protocol.ModelCapability{Source: "codex-model-list", ReasoningKnown: true, ReasoningEfforts: []string{"xhigh"}}, nil},
		{"unknown-reasoning", []string{"effort-fixture"}, protocol.ModelCapability{Source: "codex-model-list"}, nil},
		{"relay-source", []string{"effort-fixture"}, protocol.ModelCapability{Source: "relay-model-list", ReasoningKnown: true, ReasoningEfforts: []string{"xhigh"}}, nil},
		{"unreported-strength", []string{"effort-fixture"}, protocol.ModelCapability{Source: "codex-model-list", ReasoningKnown: true, ReasoningEfforts: []string{"low"}}, nil},
		{"explicit-no-reasoning", []string{"effort-fixture"}, protocol.ModelCapability{Source: "codex-model-list", ReasoningKnown: true, ReasoningEfforts: []string{}}, nil},
		{"metadata-failed", nil, protocol.ModelCapability{}, errors.New("fixture provider secret must stay private")},
	} {
		for _, typ := range []string{"session.start", "session.send"} {
			t.Run(fixture.name+"/"+typ, func(t *testing.T) {
				c, worker, cwd := effortClient(t)
				worker.catalog = &protocol.ModelCatalog{Models: fixture.models, ModelCapabilities: map[string]protocol.ModelCapability{"effort-fixture": fixture.capability}}
				worker.catalogErr = fixture.catalogErr
				res, err := c.Dispatch(context.Background(), map[string]any{"type": typ, "tool": "codex", "cwd": cwd, "prompt": "do not start",
					"sessionKey": "codex:1", "text": "do not send", "model": "effort-fixture", "effort": "xhigh"})
				if err == nil || res != nil || strings.Contains(err.Error(), "fixture provider secret") {
					t.Fatalf("res=%v err=%v", res, err)
				}
				if len(worker.started)+len(worker.sent)+len(worker.startOptions)+len(worker.sendOptions) != 0 {
					t.Fatal("unsupported effort reached paid worker")
				}
			})
		}
	}
}

func TestRelayModelNamesCannotAuthorizeEffortOrInheritRuntimeCapabilities(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Errorf("unexpected upstream action: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"effort-fixture","supportedReasoningEfforts":[{"reasoningEffort":"xhigh"}]}]}`))
	}))
	defer server.Close()
	c, worker, cwd := effortClient(t)
	if err := c.o.Store.Update(func(cfg *config.Config) error {
		cfg.LocalAccounts = []config.LocalAccount{{ID: "relay", Kind: "api", Key: "fixture-key", BaseURL: server.URL}}
		cfg.RemoteAPI = map[string]config.RemoteAPI{"codex": {AccountID: "relay"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	res, err := c.Dispatch(context.Background(), map[string]any{"type": "models.list", "tool": "codex"})
	if err != nil {
		t.Fatal(err)
	}
	cap := res.(map[string]any)["modelCapabilities"].(map[string]protocol.ModelCapability)["effort-fixture"]
	if cap.Source != "relay-model-list" || cap.ReasoningKnown || len(cap.ReasoningEfforts) != 0 {
		t.Fatalf("relay borrowed capabilities: %+v", cap)
	}
	for _, typ := range []string{"session.start", "session.send"} {
		res, err = c.Dispatch(context.Background(), map[string]any{"type": typ, "tool": "codex", "cwd": cwd, "prompt": "must not start",
			"sessionKey": "codex:1", "text": "must not send", "model": "effort-fixture", "effort": "xhigh"})
		if err == nil || res != nil {
			t.Fatalf("res=%v err=%v", res, err)
		}
	}
	if len(worker.started)+len(worker.sent)+len(worker.startOptions)+len(worker.sendOptions) != 0 {
		t.Fatal("relay effort escaped guard")
	}
}

func TestPhoneStartAndSendPreserveSupportedEffortOverrides(t *testing.T) {
	for _, effort := range []string{"", "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"} {
		t.Run(effort, func(t *testing.T) {
			c, worker, cwd := effortClient(t)
			model := ""
			if effort != "" {
				model = "effort-fixture"
			}
			if _, err := c.Dispatch(context.Background(), map[string]any{
				"type": "session.start", "tool": "codex", "cwd": cwd, "prompt": "new task", "effort": effort, "model": model,
			}); err != nil {
				t.Fatal(err)
			}
			if len(worker.started) != 1 || len(worker.startOptions) != 1 || worker.startOptions[0].Effort != effort {
				t.Fatalf("start did not preserve effort %q: %+v", effort, worker.startOptions)
			}
			if _, err := c.Dispatch(context.Background(), map[string]any{
				"type": "session.send", "sessionKey": "codex:1", "text": "next task", "effort": effort, "model": model,
			}); err != nil {
				t.Fatal(err)
			}
			if len(worker.sent) != 1 {
				t.Fatalf("send count=%d", len(worker.sent))
			}
			if effort == "" {
				if len(worker.sendOptions) != 0 {
					t.Fatal("default effort should leave the worker's settings in control")
				}
			} else if len(worker.sendOptions) != 1 || worker.sendOptions[0].Effort != effort {
				t.Fatalf("send did not preserve effort %q: %+v", effort, worker.sendOptions)
			}
		})
	}
}

func TestPhoneStartAndSendRejectInvalidEffortBeforeRunning(t *testing.T) {
	for _, effort := range []string{"HIGH", "high\n", "unknown"} {
		for _, typ := range []string{"session.start", "session.send"} {
			t.Run(typ+"/"+effort, func(t *testing.T) {
				c, worker, cwd := effortClient(t)
				result, err := c.Dispatch(context.Background(), map[string]any{
					"type": typ, "tool": "codex", "cwd": cwd, "prompt": "must not start",
					"sessionKey": "codex:1", "text": "must not send", "effort": effort,
				})
				if err == nil || result != nil || !strings.Contains(err.Error(), "无效的推理强度") {
					t.Fatalf("result=%v err=%v", result, err)
				}
				if len(worker.started)+len(worker.sent)+len(worker.startOptions)+len(worker.sendOptions) != 0 {
					t.Fatal("invalid effort reached the worker")
				}
			})
		}
	}
}

func TestDesktopLiveRejectsEffortWithoutSendingOrFallingBack(t *testing.T) {
	key := "codex:01a0ae56-e9f6-4933-9ad4-5e08cd7874e5"
	for _, effort := range []string{"minimal", "low", "medium", "high", "xhigh", "unknown"} {
		t.Run(effort, func(t *testing.T) {
			c, worker, _ := effortClient(t)
			d := &routeDesktop{st: desktopcompanion.NativeConnection{
				Active: true, ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), SessionKeys: []string{key},
			}}
			c.o.Desktop = d
			result, err := c.Dispatch(context.Background(), map[string]any{
				"type": "session.send", "sessionKey": key, "text": "must keep the override", "effort": effort, "controlSurface": "desktop",
			})
			if err == nil || result != nil || !strings.Contains(err.Error(), "不支持手机指定推理强度") {
				t.Fatalf("result=%v err=%v", result, err)
			}
			if len(d.sent)+len(worker.sent)+len(worker.sendOptions) != 0 {
				t.Fatal("desktop live effort was dropped or fell back to CLI")
			}
		})
	}
}

func TestEffortUsesBackgroundOutsideDesktopLiveScope(t *testing.T) {
	c, worker, _ := effortClient(t)
	d := &routeDesktop{st: desktopcompanion.NativeConnection{
		Active: true, ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		SessionKeys: []string{"codex:01a0ae56-e9f6-4933-9ad4-5e08cd7874e5"},
	}}
	c.o.Desktop = d
	result, err := c.Dispatch(context.Background(), map[string]any{
		"type": "session.send", "sessionKey": "codex:1", "text": "continue", "model": "effort-fixture", "effort": "xhigh", "controlSurface": "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.(map[string]any)["via"] != "background" || len(d.sent) != 0 || len(worker.sendOptions) != 1 || worker.sendOptions[0].Effort != "xhigh" {
		t.Fatalf("result=%v native=%v options=%+v", result, d.sent, worker.sendOptions)
	}
}
