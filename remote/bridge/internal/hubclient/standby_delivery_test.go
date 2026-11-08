package hubclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func TestStandbyPollDecodesEmptyAndHandoverResponses(t *testing.T) {
	for _, withCommand := range []bool{false, true} {
		t.Run(fmt.Sprint(withCommand), func(t *testing.T) {
			var command protocol.CommandEnvelope
			replies := make(chan protocol.Reply, 1)
			c, source, target := stationSwitchFixture(t, func(w http.ResponseWriter, r *http.Request) bool {
				switch r.URL.Path {
				case "/salcara-hub/v1/bridge/standby":
					commands := []protocol.CommandEnvelope{}
					if withCommand {
						commands = append(commands, command)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"pairing": map[string]any{"deviceId": r.Header.Get("X-Salcara-Device-Id"), "paired": true, "bindingId": strings.Repeat("a", 64), "phoneHash": strings.Repeat("c", 64)}, "commands": commands})
					return true
				case "/salcara-hub/v1/bridge/reply":
					var reply protocol.Reply
					_ = json.NewDecoder(r.Body).Decode(&reply)
					replies <- reply
					return true
				}
				return false
			}, nil)
			in := stationSwitchInput(source, target)
			command = protocol.CommandEnvelope{CommandID: "standby-http-switch", DeviceID: target.DeviceID, Phone: true, BindingID: source.PhoneBindingID, PhoneHash: source.PhoneHash,
				Command: map[string]any{"type": "remote.station.switch", "targetHubUrl": in.TargetHubURL, "targetDeviceId": in.TargetDeviceID, "targetComputerId": in.TargetComputerID, "operationId": in.OperationID}}
			if err := c.pollStandby(context.Background(), source, target); err != nil {
				t.Fatal(err)
			}
			if !withCommand {
				if c.o.Store.Get().DeviceID != source.DeviceID {
					t.Fatal("empty poll changed station")
				}
				return
			}
			select {
			case reply := <-replies:
				if !reply.OK || reply.CommandID != command.CommandID || c.o.Store.Get().DeviceID != target.DeviceID {
					t.Fatalf("switch was not acknowledged: %+v", reply)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("standby switch was never executed")
			}
		})
	}
}

func TestStandbyPollRejectsIncompleteOrOversizedEnvelopes(t *testing.T) {
	for _, mode := range []string{"status", "oversize", "truncated", "trailing-json"} {
		t.Run(mode, func(t *testing.T) {
			c, source, target := stationSwitchFixture(t, func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != "/salcara-hub/v1/bridge/standby" {
					return false
				}
				switch mode {
				case "status":
					w.WriteHeader(http.StatusServiceUnavailable)
				case "oversize":
					_, _ = w.Write([]byte(strings.Repeat(" ", (32<<10)+1)))
				case "truncated":
					_, _ = w.Write([]byte(`{"pairing":`))
				case "trailing-json":
					_, _ = w.Write([]byte(`{"pairing":{}} {}`))
				}
				return true
			}, nil)
			if err := c.pollStandby(context.Background(), source, target); err == nil {
				t.Fatal("malformed poll accepted")
			}
			if config.RemoteIdentity(c.o.Store.Get()) != config.RemoteIdentity(source) {
				t.Fatal("malformed poll changed station")
			}
		})
	}
}
