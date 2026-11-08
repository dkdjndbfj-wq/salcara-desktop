package hubclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

// Changing a descriptor while a synthetic HTTP probe is pending models a
// capability revoked by an adapter, without introducing a fixture data race.
type aggregationModelChoiceAgent struct {
	*aggregationFixtureAgent
	modelOverride atomic.Bool
	busy          atomic.Bool
	settings      *config.Store
}

func (a *aggregationModelChoiceAgent) Descriptor() protocol.AgentDescriptor {
	if a.settings != nil {
		// Vendor descriptors may read their settings. These checks must never
		// run from a Store.Update callback while the config write lock is held.
		_ = a.settings.Get()
	}
	d := a.descriptor
	d.Capabilities.ModelOverride = a.modelOverride.Load()
	return d
}

func (a *aggregationModelChoiceAgent) ActiveRemoteTurns() bool { return a.busy.Load() }

func (a *aggregationModelChoiceAgent) SendWithOptions(ctx context.Context, id, text string, _ agents.TurnOptions) error {
	return a.Send(ctx, id, text)
}

func aggregationModelChoice(id, family string, supported bool) *aggregationModelChoiceAgent {
	d := aggregationDescriptor(id)
	d.APIFamily = family
	a := &aggregationModelChoiceAgent{aggregationFixtureAgent: &aggregationFixtureAgent{
		fakeAgent: &fakeAgent{id: id}, descriptor: d,
	}}
	a.modelOverride.Store(supported)
	return a
}

func TestAggregationExplicitModelCapabilityUsesActualExecutor(t *testing.T) {
	for _, tc := range []struct {
		name, agent string
		manager     agents.Manager
		allowed     bool
	}{
		{"legacy-codex", "codex", nil, true},
		{"legacy-claude", "claude", nil, true},
		{"legacy-claude-code-card", "claude-desktop", nil, true},
		{"legacy-extension-denied", "hermes", nil, false},
		{"native-codex-denied", "codex-desktop", nil, false},
		{"missing-builtin-denied", "codex", &fakeManager{}, false},
		{"missing-code-worker-denied", "claude-desktop", &fakeManager{list: []agents.Agent{aggregationModelChoice("codex", "codex", true)}}, false},
		{"explicit-builtin-false", "codex", &fakeManager{list: []agents.Agent{aggregationModelChoice("codex", "codex", false)}}, false},
		{"explicit-code-false", "claude-desktop", &fakeManager{list: []agents.Agent{aggregationModelChoice("claude", "claude", false)}}, false},
		{"code-worker-true", "claude-desktop", &fakeManager{list: []agents.Agent{aggregationModelChoice("claude", "claude", true)}}, true},
		{"native-not-cli-alias", "codex-desktop", &fakeManager{list: []agents.Agent{aggregationModelChoice("codex", "codex", true)}}, false},
		{"shared-family-not-permission", "hermes", &fakeManager{list: []agents.Agent{aggregationModelChoice("codex", "codex", true), aggregationModelChoice("hermes", "codex", false)}}, false},
		{"extension-own-permission", "hermes", &fakeManager{list: []agents.Agent{aggregationModelChoice("codex", "codex", false), aggregationModelChoice("hermes", "codex", true)}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New(Options{})
			c.SetManager(tc.manager)
			err := c.requireExplicitModelCapability(tc.agent, "model-choice")
			if tc.allowed && err != nil {
				t.Fatal(err)
			}
			if !tc.allowed && (err == nil || !strings.Contains(err.Error(), "agent_capability_unavailable")) {
				t.Fatalf("unsupported override accepted: %v", err)
			}
			for _, model := range []string{"", "  "} {
				if err := c.requireExplicitModelCapability(tc.agent, model); err != nil {
					t.Fatalf("key-only choice blocked: %v", err)
				}
			}
		})
	}
}

func TestAggregationAPISetModelCapabilityBeforeProviderAndKeyOnlyCompatible(t *testing.T) {
	var providerCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "model-choice"}}})
	}))
	defer provider.Close()
	for _, agentID := range []string{"hermes", "codex", "claude", "claude-desktop", "codex-desktop"} {
		t.Run(agentID, func(t *testing.T) {
			family, workerID := "codex", agentID
			if strings.HasPrefix(agentID, "claude") {
				family, workerID = "claude", "claude"
			} else if agentID == "codex-desktop" {
				workerID = "codex"
			}
			c := statusFixture(t, config.Config{LocalAccounts: []config.LocalAccount{{
				ID: "fixture-api", Name: "Fixture API", Kind: "api", Key: "synthetic-fixture-key", BaseURL: provider.URL,
			}}})
			a := aggregationModelChoice(workerID, family, false)
			a.settings = c.o.Store
			c.SetManager(&fakeManager{list: []agents.Agent{a}})
			before := c.o.Store.Get()
			handle := APIHandle(before, "fixture-api")
			assertCapabilityRefusal(t, c, map[string]any{"type": "agents.api.set", "agent": agentID, "accountId": handle, "model": "model-choice"})
			if providerCalls.Load() != 0 || !reflect.DeepEqual(c.o.Store.Get().RemoteAPI, before.RemoteAPI) {
				t.Fatal("unsupported model choice reached provider or changed remote API")
			}
			if _, err := c.Dispatch(context.Background(), map[string]any{"type": "agents.api.set", "agent": agentID, "accountId": handle}); err != nil {
				t.Fatalf("key-only selection should remain supported: %v", err)
			}
			if got := c.o.Store.Get().RemoteAPI[family]; got.AccountID != "fixture-api" || got.Model != "" {
				t.Fatalf("wrong key-only choice: %+v", got)
			}
		})
	}
	if providerCalls.Load() != 0 {
		t.Fatal("key-only choice performed a provider call")
	}
}

func TestAggregationExplicitModelWireTypeAndBoundsDoNotBecomeKeyOnly(t *testing.T) {
	for _, commandType := range []string{"agents.api.set", "remote.station.switch"} {
		for _, value := range []any{nil, float64(7), true, []any{"m"}, map[string]any{"id": "m"}, strings.Repeat("m", 201), "m\x00x", "m\nx", "m\tx", "m\u007fx"} {
			t.Run(commandType+"/"+fmt.Sprintf("%T", value), func(t *testing.T) {
				c, source, target := stationSwitchFixture(t, nil, nil)
				_, err := c.Dispatch(context.Background(), map[string]any{"type": commandType, "agent": "codex", "model": value,
					"accountId": APIHandle(source, "vault-b"), "targetHubUrl": target.HubURL, "targetDeviceId": target.DeviceID,
					"targetComputerId": source.ComputerID, "operationId": stationSwitchInput(source, target).OperationID})
				if err == nil || !strings.Contains(err.Error(), "模型选项格式无效") {
					t.Fatalf("malformed model accepted: type=%T err=%v", value, err)
				}
				if next := c.o.Store.Get(); !reflect.DeepEqual(next.RemoteAPI, source.RemoteAPI) || config.RemoteIdentity(next) != config.RemoteIdentity(source) {
					t.Fatal("malformed model changed API or station")
				}
			})
		}
	}
	const exact = " model-choice "
	model, err := explicitModelField(map[string]any{"model": exact})
	if err != nil || model != exact {
		t.Fatal("model parsing changed receipt payload identity")
	}
}

func TestAggregationAPISetRejectsOverrideRevokedDuringCatalog(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "model-choice"}}})
	}))
	defer provider.Close()
	defer unblock.Do(func() { close(release) })
	c := statusFixture(t, config.Config{LocalAccounts: []config.LocalAccount{{
		ID: "fixture-api", Name: "Fixture API", Kind: "api", Key: "synthetic-fixture-key", BaseURL: provider.URL,
	}}})
	a := aggregationModelChoice("hermes", "codex", true)
	a.settings = c.o.Store
	c.SetManager(&fakeManager{list: []agents.Agent{a}})
	before := c.o.Store.Get()
	done := make(chan error, 1)
	go func() {
		_, err := c.Dispatch(context.Background(), map[string]any{"type": "agents.api.set", "agent": "hermes", "accountId": APIHandle(before, "fixture-api"), "model": "model-choice"})
		done <- err
	}()
	aggregationAwaitProbe(t, entered)
	a.modelOverride.Store(false)
	unblock.Do(func() { close(release) })
	aggregationAwaitCapabilityRefusal(t, done)
	if !reflect.DeepEqual(c.o.Store.Get().RemoteAPI, before.RemoteAPI) {
		t.Fatal("catalogue response persisted revoked generation override")
	}
}

func TestAggregationAPISetRejectsWorkerBecomingBusyDuringCatalog(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "model-choice"}}})
	}))
	defer provider.Close()
	defer unblock.Do(func() { close(release) })
	c := statusFixture(t, config.Config{LocalAccounts: []config.LocalAccount{{
		ID: "fixture-api", Name: "Fixture API", Kind: "api", Key: "synthetic-fixture-key", BaseURL: provider.URL,
	}}})
	a := aggregationModelChoice("hermes", "codex", true)
	a.settings = c.o.Store
	c.SetManager(&fakeManager{list: []agents.Agent{a}})
	before := c.o.Store.Get()
	done := make(chan error, 1)
	go func() {
		_, err := c.Dispatch(context.Background(), map[string]any{"type": "agents.api.set", "agent": "hermes", "accountId": APIHandle(before, "fixture-api"), "model": "model-choice"})
		done <- err
	}()
	aggregationAwaitProbe(t, entered)
	a.busy.Store(true)
	unblock.Do(func() { close(release) })
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "正在处理任务") {
			t.Fatalf("late running turn did not block API change: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("final busy check did not finish")
	}
	if !reflect.DeepEqual(c.o.Store.Get().RemoteAPI, before.RemoteAPI) || len(a.stopped) != 0 || !a.busy.Load() {
		t.Fatal("API change altered selection or stopped a newly running worker")
	}
}

func TestAggregationSharedAPISelectionDoesNotAliasIndependentWorkers(t *testing.T) {
	c := statusFixture(t, config.Config{LocalAccounts: []config.LocalAccount{{
		ID: "fixture-api", Name: "Shared API", Kind: "api", Key: "synthetic-fixture-key",
	}}, RemoteAPI: map[string]config.RemoteAPI{"claude": {AccountID: "fixture-api", Model: "claude-original-model"}}})
	codex, claude := aggregationModelChoice("codex", "codex", true), aggregationModelChoice("claude", "claude", true)
	claude.busy.Store(true)
	c.SetManager(&fakeManager{list: []agents.Agent{codex, claude}})
	before := c.o.Store.Get()
	if _, err := c.Dispatch(context.Background(), map[string]any{"type": "agents.api.set", "agent": "codex", "accountId": APIHandle(before, "fixture-api")}); err != nil {
		t.Fatalf("independent-family sharing was mistaken for a busy alias: %v", err)
	}
	after := c.o.Store.Get()
	if after.RemoteAPI["codex"].AccountID != "fixture-api" || after.RemoteAPI["claude"] != before.RemoteAPI["claude"] || !claude.busy.Load() || len(claude.stopped)+len(codex.stopped) != 0 {
		t.Fatal("Codex selection modified or stopped the independent Claude worker")
	}
}

func TestAggregationStationSwitchModelCapabilityBeforeTargetProbe(t *testing.T) {
	var targetCalls atomic.Int32
	c, source, target := stationSwitchFixture(t, func(http.ResponseWriter, *http.Request) bool { targetCalls.Add(1); return false }, nil)
	a := aggregationModelChoice("hermes", "codex", false)
	c.SetManager(&fakeManager{list: []agents.Agent{aggregationModelChoice("codex", "codex", true), a}})
	in := stationSwitchInput(source, target)
	in.Agent, in.AccountID, in.Model = "hermes", APIHandle(source, "vault-b"), "b-model"
	if _, err := c.switchStation(context.Background(), in); err == nil || !strings.Contains(err.Error(), "agent_capability_unavailable") {
		t.Fatalf("shared-family override was not refused: %v", err)
	}
	if targetCalls.Load() != 0 || config.RemoteIdentity(c.o.Store.Get()) != config.RemoteIdentity(source) || c.stationSwitching.Load() {
		t.Fatal("unsupported override reached target or mutated station state")
	}
}

func TestAggregationStationSwitchRejectsOverrideRevokedDuringProbeOrCatalog(t *testing.T) {
	for _, stage := range []string{"probe", "catalog"} {
		t.Run(stage, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var unblock sync.Once
			defer unblock.Do(func() { close(release) })
			c, source, target := stationSwitchFixture(t, func(w http.ResponseWriter, r *http.Request) bool {
				if stage == "probe" && r.URL.Path == "/salcara-hub/v1/bridge/pair/status" {
					close(entered)
					<-release
					_ = json.NewEncoder(w).Encode(map[string]any{"deviceId": r.URL.Query().Get("deviceId"), "paired": true, "bindingId": strings.Repeat("a", 64), "phoneHash": strings.Repeat("c", 64)})
					return true
				}
				if stage == "catalog" && r.URL.Path == "/v1/models" {
					close(entered)
					<-release
					_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "b-model"}}})
					return true
				}
				return false
			}, nil)
			a := aggregationModelChoice("hermes", "codex", true)
			a.settings = c.o.Store
			c.SetManager(&fakeManager{list: []agents.Agent{a}})
			in := stationSwitchInput(source, target)
			in.Agent, in.AccountID, in.Model = "hermes", APIHandle(source, "vault-b"), "b-model"
			done := make(chan error, 1)
			go func() { _, err := c.switchStation(context.Background(), in); done <- err }()
			aggregationAwaitProbe(t, entered)
			a.modelOverride.Store(false)
			unblock.Do(func() { close(release) })
			aggregationAwaitCapabilityRefusal(t, done)
			if next := c.o.Store.Get(); config.RemoteIdentity(next) != config.RemoteIdentity(source) || !reflect.DeepEqual(next.RemoteAPI, source.RemoteAPI) || c.stationSwitching.Load() {
				t.Fatal("revoked override committed partial handover or left admission closed")
			}
		})
	}
}

func TestAggregationStationSwitchRejectsWorkerBecomingBusyWithoutProgressEvent(t *testing.T) {
	for _, stage := range []string{"probe", "catalog"} {
		for _, busyID := range []string{"hermes", "claude"} {
			t.Run(stage+"/"+busyID, func(t *testing.T) {
				entered, release := make(chan struct{}), make(chan struct{})
				var unblock sync.Once
				defer unblock.Do(func() { close(release) })
				c, source, target := stationSwitchFixture(t, func(w http.ResponseWriter, r *http.Request) bool {
					if stage == "probe" && r.URL.Path == "/salcara-hub/v1/bridge/pair/status" {
						close(entered)
						<-release
						_ = json.NewEncoder(w).Encode(map[string]any{"deviceId": r.URL.Query().Get("deviceId"), "paired": true, "bindingId": strings.Repeat("a", 64), "phoneHash": strings.Repeat("c", 64)})
						return true
					}
					if stage == "catalog" && r.URL.Path == "/v1/models" {
						close(entered)
						<-release
						_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "b-model"}}})
						return true
					}
					return false
				}, nil)
				selected, unrelated := aggregationModelChoice("hermes", "codex", true), aggregationModelChoice("claude", "claude", true)
				selected.settings, unrelated.settings = c.o.Store, c.o.Store
				c.SetManager(&fakeManager{list: []agents.Agent{selected, unrelated}})
				busyWorker := selected
				if busyID == "claude" {
					busyWorker = unrelated
				}
				in := stationSwitchInput(source, target)
				in.Agent, in.AccountID, in.Model = "hermes", APIHandle(source, "vault-b"), "b-model"
				done := make(chan error, 1)
				go func() { _, err := c.switchStation(context.Background(), in); done <- err }()
				aggregationAwaitProbe(t, entered)
				busyWorker.busy.Store(true)
				unblock.Do(func() { close(release) })
				select {
				case err := <-done:
					if err == nil || !strings.Contains(err.Error(), "正在处理任务") {
						t.Fatalf("worker became active but handover was not refused: %v", err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("late station busy check did not finish")
				}
				if next := c.o.Store.Get(); config.RemoteIdentity(next) != config.RemoteIdentity(source) || !reflect.DeepEqual(next.RemoteAPI, source.RemoteAPI) || next.RemoteHandoverOperation != source.RemoteHandoverOperation || c.stationSwitching.Load() {
					t.Fatal("late busy worker allowed partial Hub/API/receipt write or stuck admission")
				}
				if !busyWorker.busy.Load() || len(selected.stopped)+len(unrelated.stopped) != 0 {
					t.Fatal("station guard stopped an independently active Agent")
				}
			})
		}
	}
}

func TestAggregationStationSwitchEmptyModelAndCommittedReceiptRemainCompatible(t *testing.T) {
	for _, sourceOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "key-only", true: "source-only"}[sourceOnly], func(t *testing.T) {
			c, source, target := stationSwitchFixture(t, nil, nil)
			if err := c.o.Store.Update(func(cfg *config.Config) error { return cfg.SetRemoteAPIForAgent("codex", "vault-b", "original-model") }); err != nil {
				t.Fatal(err)
			}
			source = c.o.Store.Get()
			a := aggregationModelChoice("hermes", "codex", false)
			a.settings = c.o.Store
			c.SetManager(&fakeManager{list: []agents.Agent{a}})
			in := stationSwitchInput(source, target)
			in.Agent, in.AccountID, in.SourceOnly = "hermes", APIHandle(source, "vault-b"), sourceOnly
			if _, err := c.switchStation(context.Background(), in); err != nil {
				t.Fatal(err)
			}
			choice := c.o.Store.Get().RemoteAPI["codex"]
			wantModel := ""
			if sourceOnly {
				wantModel = "original-model"
			}
			if choice.AccountID != "vault-b" || choice.Model != wantModel {
				t.Fatalf("wrong preserved/changed model: %+v", choice)
			}
		})
	}
	t.Run("committed-receipt-not-reexecution", func(t *testing.T) {
		c, source, target := stationSwitchFixture(t, nil, nil)
		a := aggregationModelChoice("hermes", "codex", true)
		a.settings = c.o.Store
		c.SetManager(&fakeManager{list: []agents.Agent{a}})
		in := stationSwitchInput(source, target)
		in.Agent, in.AccountID, in.Model = "hermes", APIHandle(source, "vault-b"), " b-model "
		if _, err := c.switchStation(context.Background(), in); err != nil {
			t.Fatal(err)
		}
		a.modelOverride.Store(false)
		before := c.o.Store.Get()
		result, err := c.Dispatch(context.Background(), map[string]any{"type": "remote.station.switch", "agent": in.Agent, "accountId": in.AccountID,
			"model": in.Model, "targetHubUrl": in.TargetHubURL, "targetDeviceId": in.TargetDeviceID,
			"targetComputerId": in.TargetComputerID, "operationId": in.OperationID})
		if err != nil || result.(map[string]any)["alreadyCommitted"] != true {
			t.Fatalf("receipt was re-executed against new capability: result=%v err=%v", result, err)
		}
		if next := c.o.Store.Get(); !reflect.DeepEqual(before.RemoteAPI, next.RemoteAPI) || before.RemoteHandoverPayloadHash != next.RemoteHandoverPayloadHash {
			t.Fatal("receipt retry mutated the committed API")
		}
	})
}

func aggregationAwaitProbe(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("synthetic probe was not reached")
	}
}

func aggregationAwaitCapabilityRefusal(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "agent_capability_unavailable") {
			t.Fatalf("revoked override was not refused: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("capability recheck did not finish (possible config-lock re-entry)")
	}
}
