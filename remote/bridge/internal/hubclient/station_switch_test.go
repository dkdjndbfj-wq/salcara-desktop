package hubclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func TestStationSwitchVerifiesTargetAndPreservesVaultAndPhoneBinding(t *testing.T) {
	makeHub := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/salcara-hub/v1/ping":
				_ = json.NewEncoder(w).Encode(map[string]any{"service": "salcara-hub", "protocol": "salcara-remote", "protocolVersion": 1, "authModes": []string{"device-pairing"}, "capabilities": []string{"device.identity.v1", "pair.qr.v1", "session.remote.v1", "pair.revoke.v1"}})
			case "/salcara-hub/v1/bridge/pair/status":
				if r.Header.Get("X-Salcara-Device-Secret") == "" || r.Header.Get("X-Salcara-Device-Id") == "" {
					t.Fatal("target probe did not use the saved device credential")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"deviceId": r.URL.Query().Get("deviceId"), "paired": true, "bindingId": strings.Repeat("a", 64), "phoneHash": strings.Repeat("c", 64)})
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
	}
	a, b := makeHub(), makeHub()
	defer a.Close()
	defer b.Close()
	store, err := config.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(cfg *config.Config) error {
		cfg.ComputerID = "physical-pc"
		cfg.PhoneBindingID, cfg.PhoneHash = strings.Repeat("a", 64), strings.Repeat("c", 64)
		cfg.LocalAccounts = []config.LocalAccount{{ID: "vault", Name: "B API", Kind: "api", Key: "private"}}
		cfg.ConnectRemote(a.URL, a.URL+"/salcara-hub")
		cfg.ConnectRemote(b.URL, b.URL+"/salcara-hub")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// ConnectRemote leaves B active; switch back to A for the handover fixture.
	if err := store.Update(func(cfg *config.Config) error { cfg.ConnectRemote(a.URL, a.URL+"/salcara-hub"); return nil }); err != nil {
		t.Fatal(err)
	}
	c := New(Options{Store: store, HTTP: a.Client()})
	bCfg, ok := store.Get().SavedRemoteConnection(b.URL+"/salcara-hub", store.Get().RemoteConnections[1].DeviceID)
	if !ok {
		t.Fatal("target station missing")
	}
	result, err := c.switchStation(context.Background(), stationSwitchCommand{TargetHubURL: bCfg.HubURL, TargetDeviceID: bCfg.DeviceID, TargetComputerID: "physical-pc", OperationID: "01234567-89ab-4cde-8fab-0123456789ab"})
	if err != nil {
		t.Fatal(err)
	}
	if result["switched"] != true || store.Get().EffectiveHubURL() != bCfg.HubURL || store.Get().PhoneHash != strings.Repeat("c", 64) || store.Get().LocalAccounts[0].Key != "private" {
		t.Fatalf("handover changed the wrong state: result=%v cfg=%+v", result, store.Get())
	}
	// A retry with the same operation is safe only for the same payload. This
	// prevents a lost-reply retry from silently applying a different API/model.
	retry, err := c.switchStation(context.Background(), stationSwitchCommand{TargetHubURL: bCfg.HubURL, TargetDeviceID: bCfg.DeviceID, TargetComputerID: "physical-pc", OperationID: "01234567-89ab-4cde-8fab-0123456789ab"})
	if err != nil || retry["alreadyCommitted"] != true {
		t.Fatalf("same handover retry was not idempotent: result=%v err=%v", retry, err)
	}
	if _, err := c.switchStation(context.Background(), stationSwitchCommand{TargetHubURL: bCfg.HubURL, TargetDeviceID: bCfg.DeviceID, TargetComputerID: "physical-pc", Agent: "codex", AccountID: APIHandle(store.Get(), "vault"), OperationID: "01234567-89ab-4cde-8fab-0123456789ab"}); err == nil {
		t.Fatal("different payload reused a committed handover operation")
	}
	aCfg, ok := store.Get().SavedRemoteConnection(a.URL+"/salcara-hub", store.Get().RemoteConnections[0].DeviceID)
	if !ok {
		t.Fatal("source station missing after handover")
	}
	if _, err := c.switchStation(context.Background(), stationSwitchCommand{TargetHubURL: aCfg.HubURL, TargetDeviceID: aCfg.DeviceID, TargetComputerID: "physical-pc", OperationID: "01234567-89ab-4cde-8fab-0123456789ab"}); err == nil {
		t.Fatal("committed operation ID was reused for another target station")
	}
}

// All credentials in this fixture are local-only synthetic values. No model
// generation endpoint is exposed, even in the model-validation tests.
func stationSwitchFixture(t *testing.T, targetHook func(http.ResponseWriter, *http.Request) bool, sourceHook func(http.ResponseWriter, *http.Request)) (*Client, config.Config, config.RemoteConnection) {
	t.Helper()
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sourceHook != nil {
			sourceHook(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(a.Close)
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if targetHook != nil && targetHook(w, r) {
			return
		}
		switch r.URL.Path {
		case "/salcara-hub/v1/ping":
			_ = json.NewEncoder(w).Encode(map[string]any{"service": "salcara-hub", "protocol": "salcara-remote", "protocolVersion": 1, "authModes": []string{"device-pairing"}, "capabilities": []string{"device.identity.v1", "pair.qr.v1", "session.remote.v1", "pair.revoke.v1"}})
		case "/salcara-hub/v1/bridge/pair/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"deviceId": r.URL.Query().Get("deviceId"), "paired": true, "bindingId": strings.Repeat("a", 64), "phoneHash": strings.Repeat("c", 64)})
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "b-model"}}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(b.Close)
	store, err := config.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(cfg *config.Config) error {
		cfg.ComputerID = "physical-pc"
		cfg.PhoneBindingID, cfg.PhoneHash = strings.Repeat("a", 64), strings.Repeat("c", 64)
		cfg.LocalAccounts = []config.LocalAccount{{ID: "vault-b", Name: "B API", Kind: "api", Key: "synthetic-api-secret", BaseURL: b.URL, Models: []string{"retired-model"}}}
		cfg.Projects = []protocol.Project{{Path: t.TempDir(), Name: "Original project"}}
		cfg.ConnectRemote(a.URL, a.URL+"/salcara-hub")
		cfg.ConnectRemote(b.URL, b.URL+"/salcara-hub")
		cfg.ConnectRemote(a.URL, a.URL+"/salcara-hub")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	source := store.Get()
	target := source.RemoteConnections[1]
	c := New(Options{Store: store, HTTP: a.Client()})
	c.publishPair(PairStatus{DeviceID: source.DeviceID, Paired: true}, eventIdentity(source))
	return c, source, target
}

func stationSwitchInput(source config.Config, target config.RemoteConnection) stationSwitchCommand {
	return stationSwitchCommand{TargetHubURL: target.HubURL, TargetDeviceID: target.DeviceID,
		TargetComputerID: source.ComputerID, OperationID: "01234567-89ab-4cde-8fab-0123456789ab"}
}

func TestStationSwitchCommitsAPIAndHubTogetherWithoutReplacingProject(t *testing.T) {
	c, source, target := stationSwitchFixture(t, nil, nil)
	in := stationSwitchInput(source, target)
	in.Agent, in.AccountID, in.Model, in.SessionKey = "codex", APIHandle(source, "vault-b"), "b-model", "codex:original"
	result, err := c.switchStation(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	next := c.o.Store.Get()
	if result["apiChanged"] != true || next.DeviceID != target.DeviceID || next.EffectiveHubURL() != target.HubURL || next.RemoteAPI["codex"].AccountID != "vault-b" || next.RemoteAPI["codex"].Model != "b-model" {
		t.Fatal("API and Hub were not committed together")
	}
	if !reflect.DeepEqual(source.Projects, next.Projects) || next.LocalAccounts[0].Key != source.LocalAccounts[0].Key || APIHandle(next, "vault-b") != in.AccountID {
		t.Fatal("handover changed project/vault/opaque API identity")
	}
}

func TestStationSwitchRetainsEventsUntilTargetPairIsAuthorized(t *testing.T) {
	var uploads atomic.Int32
	c, source, target := stationSwitchFixture(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/salcara-hub/v1/bridge/events" {
			return false
		}
		uploads.Add(1)
		w.WriteHeader(http.StatusOK)
		return true
	}, nil)
	if _, err := c.switchStation(context.Background(), stationSwitchInput(source, target)); err != nil {
		t.Fatal(err)
	}
	c.setState(StateConnected, "")
	c.o.FlushInterval = 10 * time.Millisecond
	c.Push(protocol.Event{Type: "message", SessionKey: "codex:after-switch", Text: "target tail", Final: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.flushLoop(ctx)
	time.Sleep(60 * time.Millisecond)
	c.qmu.Lock()
	retained := len(c.queue) == 1 && c.queue[0].Text == "target tail"
	c.qmu.Unlock()
	if !retained || uploads.Load() != 0 {
		t.Fatalf("event was cleared or sent before B authorization: retained=%v uploads=%d", retained, uploads.Load())
	}
	active := c.o.Store.Get()
	c.publishPair(PairStatus{DeviceID: active.DeviceID, Paired: true}, eventIdentity(active))
	waitForQueue := time.Now().Add(time.Second)
	for time.Now().Before(waitForQueue) {
		c.qmu.Lock()
		done := len(c.queue) == 0
		c.qmu.Unlock()
		if done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if uploads.Load() != 1 {
		t.Fatal("authorized target did not receive retained event")
	}
}

func TestStationSwitchLateEventDuringModelReadStaysOnSource(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	c, source, target := stationSwitchFixture(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/v1/models" {
			return false
		}
		close(entered)
		select {
		case <-release:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "b-model"}}})
		case <-r.Context().Done():
		}
		return true
	}, nil)
	in := stationSwitchInput(source, target)
	in.Agent, in.AccountID, in.Model = "codex", APIHandle(source, "vault-b"), "b-model"
	done := make(chan error, 1)
	go func() { _, err := c.switchStation(context.Background(), in); done <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("target model probe did not start")
	}
	c.Push(protocol.Event{Type: "message", SessionKey: "codex:original", Text: "late source tail", Final: true})
	close(release)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("late event was ignored during handover commit")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("handover did not release its queue gate")
	}
	if config.RemoteIdentity(c.o.Store.Get()) != config.RemoteIdentity(source) || c.o.Store.Get().RemoteAPI["codex"].AccountID != "" {
		t.Fatal("rejected handover partially applied API or Hub")
	}
	c.qmu.Lock()
	defer c.qmu.Unlock()
	if len(c.queue) != 1 || c.queue[0].Text != "late source tail" || config.RemoteIdentity(c.queueConfig) != config.RemoteIdentity(source) || c.stationSwitching.Load() {
		t.Fatal("source tail was lost, misrouted or permanently blocked")
	}
}

func TestStationSwitchRejectsInFlightUpload(t *testing.T) {
	c, source, target := stationSwitchFixture(t, nil, nil)
	c.inFlight = 1
	if _, err := c.switchStation(context.Background(), stationSwitchInput(source, target)); err == nil {
		t.Fatal("in-flight source upload allowed handover")
	}
	if config.RemoteIdentity(c.o.Store.Get()) != config.RemoteIdentity(source) || c.stationSwitching.Load() {
		t.Fatal("rejected upload handover changed or blocked the active station")
	}
}

func TestStationSwitchCommittedThenUnboundStillKicksSourceStream(t *testing.T) {
	var reply protocol.Reply
	c, source, target := stationSwitchFixture(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/salcara-hub/v1/bridge/reply" || r.Header.Get("X-Salcara-Device-Id") == "" {
			t.Errorf("reply did not use source station: %s", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&reply)
		w.WriteHeader(http.StatusOK)
	})
	c.o.Store.OnChange(func(_, next config.Config) {
		if next.RemoteHandoverOperation != "" && next.PhoneHash != "" {
			if err := c.o.Store.Update(func(cfg *config.Config) error {
				cfg.PhoneBindingID, cfg.PhoneHash = strings.Repeat("d", 64), ""
				return nil
			}); err != nil {
				t.Error(err)
			}
		}
	})
	in := stationSwitchInput(source, target)
	cancelled := false
	c.cancelConn = func() { cancelled = true }
	c.executeFor(protocol.CommandEnvelope{CommandID: "source-command", DeviceID: source.DeviceID,
		BindingID: source.PhoneBindingID, PhoneHash: source.PhoneHash,
		Command: map[string]any{"type": "remote.station.switch", "targetHubUrl": in.TargetHubURL,
			"targetDeviceId": in.TargetDeviceID, "targetComputerId": in.TargetComputerID, "operationId": in.OperationID}}, source)
	if reply.OK || reply.DeviceID != source.DeviceID || reply.Result != nil {
		t.Fatal("revoked phone received a success/private handover result")
	}
	if c.o.Store.Get().EffectiveHubURL() != target.HubURL || c.o.Store.Get().PhoneHash != "" || !cancelled {
		t.Fatal("committed/unbound handover left the SSE attached to A")
	}
}
