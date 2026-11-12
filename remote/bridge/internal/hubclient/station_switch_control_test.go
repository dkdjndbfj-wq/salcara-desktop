package hubclient

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

type stationControlAgent struct {
	*fakeAgent
	active   atomic.Bool
	controls atomic.Int32
}

func (a *stationControlAgent) Descriptor() protocol.AgentDescriptor {
	d := aggregationDescriptor(a.id)
	d.Capabilities.PreciseInterrupt = true
	return d
}
func (a *stationControlAgent) ActiveRemoteTurns() bool { return a.active.Load() }
func (a *stationControlAgent) InterruptTurn(context.Context, string, string, string) (protocol.InterruptResult, error) {
	a.controls.Add(1)
	a.active.Store(false)
	return protocol.InterruptResult{}, nil
}
func (a *stationControlAgent) RespondForSession(context.Context, string, string, agents.ApprovalResponse) bool {
	a.controls.Add(1)
	a.active.Store(false)
	return true
}
func (a *stationControlAgent) RespondWithAnswers(string, agents.ApprovalResponse) bool {
	a.controls.Add(1)
	a.active.Store(false)
	return true
}

func blockedStationProbe(t *testing.T) (*Client, config.Config, config.RemoteConnection, <-chan struct{}, func()) {
	t.Helper()
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	c, source, target := stationSwitchFixture(t, func(_ http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/salcara-hub/v1/bridge/pair/status" {
			enteredOnce.Do(func() { close(entered) })
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
		return false
	}, nil)
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	return c, source, target, entered, unblock
}

func TestStationTargetProbeDoesNotBlockSourceControls(t *testing.T) {
	for _, kind := range []string{"session.interrupt", "approval.respond"} {
		t.Run(kind, func(t *testing.T) {
			c, source, target, entered, release := blockedStationProbe(t)
			a := &stationControlAgent{fakeAgent: &fakeAgent{id: "codex"}}
			c.SetManager(&fakeManager{list: []agents.Agent{a}})
			guard := commandGuard{identity: config.RemoteIdentity(source), deviceID: source.DeviceID, bindingID: source.PhoneBindingID, phoneHash: source.PhoneHash}
			ctx, cancel := context.WithCancel(withCommandGuard(context.Background(), guard))
			defer cancel()
			switchDone := make(chan error, 1)
			go func() {
				_, err := c.Dispatch(ctx, map[string]any{"type": "remote.station.switch", "targetHubUrl": target.HubURL, "targetDeviceId": target.DeviceID, "targetComputerId": source.ComputerID, "operationId": "01234567-89ab-4cde-8fab-0123456789ab"})
				switchDone <- err
			}()
			select {
			case <-entered:
			case err := <-switchDone:
				t.Fatalf("switch returned before probe: %v", err)
			case <-time.After(3 * time.Second):
				t.Fatal("probe not reached")
			}
			// A task can become active on the computer after the idle snapshot.
			a.active.Store(true)
			controlDone := make(chan error, 1)
			go func() {
				_, err := c.Dispatch(withCommandGuard(context.Background(), guard), map[string]any{"type": kind, "sessionKey": "codex:original", "controlSurface": "cli", "expectedTurnId": "original-turn", "operationId": "11111111-2222-4333-8444-555555555555", "approvalId": "pending", "decision": "deny"})
				controlDone <- err
			}()
			select {
			case err := <-controlDone:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				release()
				t.Fatal("target network probe blocked source control")
			}
			if a.controls.Load() != 1 {
				t.Fatal("control did not reach the original worker")
			}
			cancel()
			release()
			select {
			case err := <-switchDone:
				if err == nil {
					t.Fatal("cancelled switch committed")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("switch did not finish")
			}
			if config.RemoteIdentity(c.o.Store.Get()) != config.RemoteIdentity(source) {
				t.Fatal("cancelled switch changed station")
			}
		})
	}
}

func TestStationTargetProbeRechecksChangedAdmission(t *testing.T) {
	for _, change := range []string{"identity", "epoch", "phone", "target", "target-bot", "computer", "disabled", "standby-revoked", "late-cancel"} {
		t.Run(change, func(t *testing.T) {
			c, source, target, entered, release := blockedStationProbe(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var revoked atomic.Bool
			ctx = context.WithValue(ctx, stationSwitchGuardKey{}, func(config.Config) bool { return !revoked.Load() })
			if change == "late-cancel" {
				a := &cancellationAPIWorker{fakeAgent: &fakeAgent{id: "codex"}, onCheck: func(n int) {
					if n == 2 {
						cancel()
					}
				}}
				c.SetManager(&fakeManager{list: []agents.Agent{a}})
			}
			done := make(chan error, 1)
			go func() { _, err := c.switchStation(ctx, stationSwitchInput(source, target)); done <- err }()
			select {
			case <-entered:
			case err := <-done:
				t.Fatalf("switch returned before probe: %v", err)
			case <-time.After(3 * time.Second):
				t.Fatal("probe not reached")
			}
			if change == "standby-revoked" {
				revoked.Store(true)
			}
			if err := c.o.Store.Update(func(cfg *config.Config) error {
				switch change {
				case "identity":
					cfg.DeviceID = "replacement-device"
				case "epoch":
					cfg.GatewayRuntimeKey = "replacement-runtime-key"
				case "phone":
					cfg.PhoneBindingID = strings.Repeat("f", 64)
				case "computer":
					cfg.ComputerID = "replacement-computer"
				case "target-bot":
					for i := range cfg.RemoteConnections {
						if cfg.RemoteConnections[i].DeviceID == target.DeviceID {
							cfg.RemoteConnections[i].BotGroupID = "replacement-group"
						}
					}
				case "target":
					for i := range cfg.RemoteConnections {
						if cfg.RemoteConnections[i].DeviceID == target.DeviceID {
							cfg.RemoteConnections[i].DeviceSecret = "replacement-target-secret"
						}
					}
				case "disabled":
					cfg.ActivateRemoteMode("none")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			release()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("changed admission committed")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("switch did not finish")
			}
			after := c.o.Store.Get()
			if after.DeviceID == target.DeviceID || after.RemoteHandoverOperation != source.RemoteHandoverOperation {
				t.Fatal("stale switch persisted target or receipt")
			}
		})
	}
}
