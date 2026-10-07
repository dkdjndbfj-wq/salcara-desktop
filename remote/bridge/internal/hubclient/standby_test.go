package hubclient

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func TestStandbySwitchTransfersFrozenEventsWithoutContactingSource(t *testing.T) {
	replies := make(chan protocol.Reply, 1)
	c, source, target := stationSwitchFixture(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/salcara-hub/v1/bridge/reply" {
			return false
		}
		var reply protocol.Reply
		_ = json.NewDecoder(r.Body).Decode(&reply)
		replies <- reply
		w.WriteHeader(200)
		return true
	}, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("offline source was contacted")
		w.WriteHeader(503)
	})
	c.Push(protocol.Event{Type: "message", Tool: "codex", SessionKey: "codex:original", ID: "final", Text: "retained reply", Final: true})
	c.qmu.Lock()
	c.inFlight, c.inFlightBatchID = 1, "old-a-batch"
	c.inFlightPayload = json.RawMessage(`{"events":[]}`)
	before := append([]protocol.Event(nil), c.queue...)
	c.qmu.Unlock()
	in := stationSwitchInput(source, target)
	env := protocol.CommandEnvelope{CommandID: "standby-handover", DeviceID: target.DeviceID, Phone: true, BindingID: source.PhoneBindingID, PhoneHash: source.PhoneHash, Command: map[string]any{"type": "remote.station.switch", "targetHubUrl": in.TargetHubURL, "targetDeviceId": in.TargetDeviceID, "targetComputerId": in.TargetComputerID, "operationId": in.OperationID}}
	c.executeStandby(source, activeStationConfig(source, target), env)
	replied := <-replies
	if !replied.OK || c.o.Store.Get().DeviceID != target.DeviceID {
		t.Fatalf("switch rejected: %s", replied.Error)
	}
	c.qmu.Lock()
	defer c.qmu.Unlock()
	if !reflect.DeepEqual(before, c.queue) || c.inFlight != 0 || c.inFlightBatchID != "" || c.inFlightPayload != nil || eventIdentity(c.queueConfig) != eventIdentity(c.o.Store.Get()) || !c.handoverAwaitingAuth.Load() {
		t.Fatal("frozen events lost or uploaded before target authorization")
	}
}

func TestStandbyRejectsWrongPhoneAndNonHandoverCommand(t *testing.T) {
	for _, change := range []string{"phone", "task", "target", "source"} {
		t.Run(change, func(t *testing.T) {
			c, source, target := stationSwitchFixture(t, nil, nil)
			in := stationSwitchInput(source, target)
			env := protocol.CommandEnvelope{CommandID: "handover", DeviceID: target.DeviceID, Phone: true, BindingID: source.PhoneBindingID, PhoneHash: source.PhoneHash, Command: map[string]any{"type": "remote.station.switch", "targetHubUrl": in.TargetHubURL, "targetDeviceId": in.TargetDeviceID, "targetComputerId": in.TargetComputerID, "operationId": in.OperationID}}
			switch change {
			case "phone":
				env.PhoneHash = strings.Repeat("d", 64)
			case "task":
				env.Command["type"] = "session.send"
			case "target":
				env.Command["targetDeviceId"] = source.DeviceID
			case "source":
				_ = c.o.Store.Update(func(cfg *config.Config) error { cfg.PhoneHash = strings.Repeat("e", 64); return nil })
			}
			before := c.o.Store.Get()
			c.executeStandby(source, activeStationConfig(source, target), env)
			if !reflect.DeepEqual(before, c.o.Store.Get()) {
				t.Fatal("rejected standby mutated state")
			}
		})
	}
}

func TestStandbyPollUsesOnlySavedCredentialAndRejectsCrossBinding(t *testing.T) {
	var calls atomic.Int32
	c, source, target := stationSwitchFixture(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/salcara-hub/v1/bridge/standby" {
			return false
		}
		calls.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Salcara-Device-Id") == "" || r.Header.Get("X-Salcara-Device-Secret") == "" || r.Header.Get("X-Salcara-Computer-Id") != "physical-pc" {
			t.Error("wrong standby credentials")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"pairing": map[string]any{"deviceId": r.Header.Get("X-Salcara-Device-Id"), "paired": true, "bindingId": strings.Repeat("a", 64), "phoneHash": strings.Repeat("d", 64)}, "commands": []any{}})
		return true
	}, nil)
	if err := c.pollStandby(context.Background(), source, target); err == nil || calls.Load() != 1 {
		t.Fatal("cross-binding standby accepted")
	}
	if !reflect.DeepEqual(source, c.o.Store.Get()) {
		t.Fatal("poll modified active station")
	}
}
