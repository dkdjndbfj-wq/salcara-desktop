package hubclient

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func stationSourceRegressionFixture(t *testing.T, targetHook func(http.ResponseWriter, *http.Request) bool, replies chan<- protocol.Reply) (*Client, config.Config, config.RemoteConnection) {
	t.Helper()
	replyHook := func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/salcara-hub/v1/bridge/reply" {
			return false
		}
		var reply protocol.Reply
		if err := json.NewDecoder(r.Body).Decode(&reply); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return true
		}
		replies <- reply
		w.WriteHeader(http.StatusOK)
		return true
	}
	c, _, target := stationSwitchFixture(t, func(w http.ResponseWriter, r *http.Request) bool {
		if replyHook(w, r) {
			return true
		}
		return targetHook != nil && targetHook(w, r)
	}, func(w http.ResponseWriter, r *http.Request) {
		if !replyHook(w, r) {
			w.WriteHeader(http.StatusNotFound)
		}
	})
	if err := c.o.Store.Update(func(cfg *config.Config) error {
		cfg.LocalAccounts[0].SourceType = "api_key"
		cfg.LocalAccounts[0].ProviderID = "provider-a"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c.o.Store.SetRuntimeGatewayKey("synthetic-station-source-lease")
	return c, c.o.Store.Get(), target
}

func stationSourceRegressionEnvelope(source config.Config, target config.RemoteConnection, mode string) protocol.CommandEnvelope {
	in := stationSwitchInput(source, target)
	env := protocol.CommandEnvelope{CommandID: "source-handover", DeviceID: source.DeviceID, Phone: true,
		BindingID: source.PhoneBindingID, PhoneHash: source.PhoneHash,
		Command: map[string]any{"type": "remote.station.switch", "targetHubUrl": in.TargetHubURL,
			"targetDeviceId": in.TargetDeviceID, "targetComputerId": in.TargetComputerID, "operationId": in.OperationID},
		Source: &protocol.ExecutionSource{Mode: mode, PolicyRevision: "station-policy-r1", Station: &protocol.StationPolicy{Version: 1, APIBases: []string{}}}}
	if mode == "restricted" {
		account := source.LocalAccounts[0]
		handle := APIHandle(activeStationConfig(source, target), account.ID)
		env.Command["agent"], env.Command["accountId"] = "codex", handle
		env.Source.Issuer, env.Source.SourceType, env.Source.CredentialHandle = "synthetic-station", "site_api", handle
		env.Source.Origin, env.Source.ConfigRevision = accountOrigin(account), source.AccountSourceRevision(account)
		env.Source.Agent, env.Source.ControlSurface = "codex", "cli"
	}
	return env
}

func executeStationSourceRegression(c *Client, source config.Config, target config.RemoteConnection, env protocol.CommandEnvelope, path string) {
	if path == "standby" {
		env.DeviceID = target.DeviceID
		c.executeStandby(source, activeStationConfig(source, target), env)
		return
	}
	c.executeFor(env, source)
}

func stationSourceRegressionReply(t *testing.T, replies <-chan protocol.Reply) protocol.Reply {
	t.Helper()
	select {
	case reply := <-replies:
		return reply
	case <-time.After(3 * time.Second):
		t.Fatal("station command did not produce a reply")
		return protocol.Reply{}
	}
}

func TestStationSourceGuardRejectsUnprovenHandover(t *testing.T) {
	for _, path := range []string{"active", "standby"} {
		for _, proof := range []string{"missing", "stale"} {
			t.Run(path+"/"+proof, func(t *testing.T) {
				replies := make(chan protocol.Reply, 1)
				var probes atomic.Int32
				c, source, target := stationSourceRegressionFixture(t, func(_ http.ResponseWriter, r *http.Request) bool {
					if r.URL.Path == "/salcara-hub/v1/ping" || r.URL.Path == "/salcara-hub/v1/bridge/pair/status" {
						probes.Add(1)
					}
					return false
				}, replies)
				env := stationSourceRegressionEnvelope(source, target, "restricted")
				wantError := "API 配置已变化"
				if proof == "missing" {
					env.Source = nil
					wantError = "未提供来源证明"
				} else {
					env.Source.ConfigRevision = "stale-station-revision"
				}
				executeStationSourceRegression(c, source, target, env, path)
				reply := stationSourceRegressionReply(t, replies)
				if reply.OK || reply.Result != nil || !strings.Contains(reply.Error, wantError) {
					t.Fatalf("unproven handover was not rejected by source validation: %+v", reply)
				}
				if !reflect.DeepEqual(source, c.o.Store.Get()) || probes.Load() != 0 || c.stationSwitching.Load() || c.handoverAwaitingAuth.Load() {
					t.Fatal("rejected source proof probed, committed, or blocked a station")
				}
			})
		}
	}
}

func TestStationSourceGuardAllowsAuthorizedHandover(t *testing.T) {
	for _, path := range []string{"active", "standby"} {
		for _, mode := range []string{"restricted", "open"} {
			t.Run(path+"/"+mode, func(t *testing.T) {
				replies := make(chan protocol.Reply, 1)
				c, source, target := stationSourceRegressionFixture(t, nil, replies)
				env := stationSourceRegressionEnvelope(source, target, mode)
				if path == "standby" {
					c.Push(protocol.Event{Type: "message", Tool: "codex", SessionKey: "codex:original", Text: "retained source tail", Final: true})
				}
				executeStationSourceRegression(c, source, target, env, path)
				reply := stationSourceRegressionReply(t, replies)
				next := c.o.Store.Get()
				if !reply.OK || next.DeviceID != target.DeviceID || next.EffectiveHubURL() != target.HubURL || next.RemoteHandoverOperation != str(env.Command, "operationId") {
					t.Fatalf("authorized %s handover failed: reply=%+v", mode, reply)
				}
				if mode == "restricted" && next.RemoteAPI["codex"].AccountID != source.LocalAccounts[0].ID {
					t.Fatal("authorized target API was not committed with the station")
				}
				if !reflect.DeepEqual(source.LocalAccounts, next.LocalAccounts) || !reflect.DeepEqual(source.Projects, next.Projects) || next.PhoneBindingID != source.PhoneBindingID || next.PhoneHash != source.PhoneHash {
					t.Fatal("authorized handover replaced the vault, project, or phone binding")
				}
				if path == "standby" {
					c.qmu.Lock()
					retained := len(c.queue) == 1 && c.queue[0].Text == "retained source tail" && eventIdentity(c.queueConfig) == eventIdentity(next)
					c.qmu.Unlock()
					if !retained || !c.handoverAwaitingAuth.Load() {
						t.Fatal("authorized standby handover lost or prematurely released retained events")
					}
				}
			})
		}
	}
}

func TestStationSourceGuardRechecksProvenanceAtCommit(t *testing.T) {
	for _, path := range []string{"active", "standby"} {
		for _, change := range []string{"provider", "sourceType"} {
			t.Run(path+"/"+change, func(t *testing.T) {
				entered, release := make(chan struct{}), make(chan struct{})
				replies := make(chan protocol.Reply, 1)
				c, source, target := stationSourceRegressionFixture(t, func(_ http.ResponseWriter, r *http.Request) bool {
					if r.URL.Path != "/salcara-hub/v1/bridge/pair/status" {
						return false
					}
					close(entered)
					select {
					case <-release:
						return false
					case <-r.Context().Done():
						return true
					}
				}, replies)
				var releaseOnce sync.Once
				releaseProbe := func() { releaseOnce.Do(func() { close(release) }) }
				t.Cleanup(releaseProbe)
				env := stationSourceRegressionEnvelope(source, target, "restricted")
				done := make(chan struct{})
				go func() {
					executeStationSourceRegression(c, source, target, env, path)
					close(done)
				}()
				select {
				case <-entered:
				case <-time.After(3 * time.Second):
					t.Fatal("authorized handover did not reach the target probe")
				}
				if err := c.o.Store.Update(func(cfg *config.Config) error {
					if change == "provider" {
						cfg.LocalAccounts[0].ProviderID = "provider-b"
					} else {
						cfg.LocalAccounts[0].SourceType = "reverse_proxy"
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				changed := c.o.Store.Get()
				// These fields are absent from catalog identity, so only the execution
				// source guard can reject this race at the final atomic write.
				if catalogAccountFingerprint(source.LocalAccounts[0]) != catalogAccountFingerprint(changed.LocalAccounts[0]) || config.AccountFingerprint(source.LocalAccounts[0]) == config.AccountFingerprint(changed.LocalAccounts[0]) {
					t.Fatal("fixture did not isolate a provenance change from catalog identity")
				}
				releaseProbe()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("handover did not finish after the target probe")
				}
				reply := stationSourceRegressionReply(t, replies)
				wantError := "API 配置已变化"
				if change == "sourceType" {
					wantError = "API 来源类型"
				}
				if reply.OK || reply.Result != nil || !strings.Contains(reply.Error, wantError) {
					t.Fatalf("changed provenance was not rejected at commit: %+v", reply)
				}
				if !reflect.DeepEqual(changed, c.o.Store.Get()) || c.stationSwitching.Load() || c.handoverAwaitingAuth.Load() {
					t.Fatal("failed final validation partially committed or blocked the handover")
				}
			})
		}
	}
}

func TestStationSourceGuardPreservesCommittedRetryReceipt(t *testing.T) {
	for _, mode := range []string{"restricted", "open"} {
		t.Run(mode, func(t *testing.T) {
			var probes atomic.Int32
			c, source, target := stationSourceRegressionFixture(t, func(_ http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path == "/salcara-hub/v1/bridge/pair/status" {
					probes.Add(1)
				}
				return false
			}, make(chan protocol.Reply, 1))
			env := stationSourceRegressionEnvelope(source, target, mode)
			ctx := withExecutionSource(context.Background(), env)
			if _, err := c.Dispatch(ctx, env.Command); err != nil {
				t.Fatal(err)
			}
			committed := c.o.Store.Get()
			c.Push(protocol.Event{Type: "message", Tool: "codex", SessionKey: "codex:after-switch", Text: "target tail", Final: true})
			result, err := c.Dispatch(ctx, env.Command)
			if err != nil {
				t.Fatalf("authorized same-operation retry rejected: %v", err)
			}
			receipt, ok := result.(map[string]any)
			if !ok || receipt["alreadyCommitted"] != true || receipt["operationId"] != committed.RemoteHandoverOperation || receipt["payloadHash"] != committed.RemoteHandoverPayloadHash {
				t.Fatalf("same-operation retry did not return its committed receipt: %v", result)
			}
			if !reflect.DeepEqual(committed, c.o.Store.Get()) || probes.Load() != 1 {
				t.Fatal("receipt retry reprobed or mutated the committed station")
			}
			c.qmu.Lock()
			defer c.qmu.Unlock()
			if len(c.queue) != 1 || c.queue[0].Text != "target tail" {
				t.Fatal("receipt retry lost pending target events")
			}
		})
	}
}
