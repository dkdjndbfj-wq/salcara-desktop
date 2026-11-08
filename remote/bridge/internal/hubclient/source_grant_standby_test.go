package hubclient

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func standbyGrantFixture(t *testing.T, alterChallenge func(*sourceGrantChallenge), issuerHook func(*config.Store), challengeHook ...func(*config.Store)) (*Client, config.Config, config.Config, protocol.CommandEnvelope, <-chan protocol.Reply, *atomic.Int64) {
	t.Helper()
	c, store, account, _ := sourceGuardFixture(t, "api_key")
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	var challenge sourceGrantChallenge
	var policy sourceGrantPolicy
	var station config.Config
	replies := make(chan protocol.Reply, 2)
	issuerCalls := &atomic.Int64{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/salcara-hub/v1/bridge/source/challenge":
			if r.Header.Get("X-Salcara-Device-Id") != station.DeviceID || r.Header.Get("X-Salcara-Device-Secret") != station.DeviceSecret || r.URL.Query().Get("challengeId") != "challenge-standby" {
				t.Error("challenge did not use the saved standby credential")
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if len(challengeHook) != 0 {
				challengeHook[0](store)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"challenge": challenge, "policy": policy})
		case "/grants":
			issuerCalls.Add(1)
			if r.Header.Get("X-Salcara-Device-Secret") != "" || r.Header.Get("Authorization") != "" || r.Header.Get("X-Salcara-Pair-Token") != "" {
				t.Error("station credentials reached issuer")
			}
			var input struct {
				Version    int                 `json:"version"`
				Snapshot   sourceGrantSnapshot `json:"snapshot"`
				Credential struct {
					Type, Value string
				} `json:"credential"`
			}
			if json.NewDecoder(r.Body).Decode(&input) != nil || input.Version != 1 || input.Credential.Type != "api_key" || input.Credential.Value != "key-a" || input.Snapshot.DeviceID != station.DeviceID || input.Snapshot.BindingID != station.PhoneBindingID || input.Snapshot.ChallengeID != challenge.ChallengeID || input.Snapshot.CommandHash != challenge.CommandHash || input.Snapshot.RequestID != challenge.RequestID {
				t.Error("issuer received an unbound source request")
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if issuerHook != nil {
				issuerHook(store)
			}
			input.Snapshot.IssuedAt = time.Now().Unix()
			input.Snapshot.ExpiresAt = input.Snapshot.IssuedAt + 60
			payload, _ := json.Marshal(input.Snapshot)
			_ = json.NewEncoder(w).Encode(signedSourceGrant{Payload: base64.RawURLEncoding.EncodeToString(payload), Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, payload))})
		case "/salcara-hub/v1/bridge/reply":
			var reply protocol.Reply
			if json.NewDecoder(r.Body).Decode(&reply) != nil {
				t.Error("bad standby reply")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			replies <- reply
			w.WriteHeader(http.StatusOK)
		case "/salcara-hub/v1/ping":
			_ = json.NewEncoder(w).Encode(map[string]any{"service": "salcara-hub", "protocol": "salcara-remote", "protocolVersion": 1, "authModes": []string{"device-pairing"}, "capabilities": []string{"device.identity.v1", "pair.qr.v1", "session.remote.v1", "pair.revoke.v1", "station.guard.v1"}, "remoteAccess": map[string]any{"mode": "restricted", "revision": policy.Revision, "station": policy.Station}})
		case "/salcara-hub/v1/bridge/pair/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"deviceId": station.DeviceID, "paired": true, "bindingId": station.PhoneBindingID, "phoneHash": station.PhoneHash})
		default:
			t.Errorf("unexpected route, no model endpoint should be contacted: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	// The adapter creates its own direct HTTPS client. Install only this local
	// TLS fixture's transport for the duration of these serial tests.
	previousTransport := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = previousTransport })
	c.o.HTTP = server.Client()
	if err := store.Update(func(cfg *config.Config) error {
		cfg.ComputerID = "physical-source-grant"
		cfg.PhoneBindingID, cfg.PhoneHash = strings.Repeat("a", 64), strings.Repeat("c", 64)
		cfg.LocalAccounts[0].BaseURL = server.URL + "/tenant"
		cfg.ConnectRemote("http://127.0.0.1:1", "http://127.0.0.1:1/salcara-hub")
		cfg.ConnectRemote(server.URL, server.URL+"/salcara-hub")
		cfg.ConnectRemote("http://127.0.0.1:1", "http://127.0.0.1:1/salcara-hub")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	base := store.Get()
	station = activeStationConfig(base, base.RemoteConnections[1])
	c.publishPair(PairStatus{DeviceID: base.DeviceID, Paired: true}, eventIdentity(base))
	command, _ := json.Marshal(map[string]any{"type": "remote.station.switch", "targetHubUrl": station.EffectiveHubURL(), "targetDeviceId": station.DeviceID, "targetComputerId": base.ComputerID, "operationId": "10000000-0000-4000-8000-000000000001", "agent": "codex", "accountId": APIHandle(station, account.ID)})
	hash, _ := sourceCommandHash(command)
	challenge = sourceGrantChallenge{ChallengeID: "challenge-standby", Audience: "standby-hub", PolicyRevision: "standby-policy", DeviceID: station.DeviceID, BindingID: station.PhoneBindingID, RequestID: "10000000-0000-4000-8000-000000000001", CommandHash: hash, Command: command, ExpiresAt: time.Now().Unix() + 120}
	policy = sourceGrantPolicy{Mode: "restricted", Revision: challenge.PolicyRevision, Audience: challenge.Audience, Issuers: []sourceIssuer{{ID: "local-issuer", PublicKey: base64.RawURLEncoding.EncodeToString(public), SourceTypes: []string{"site_api"}, AgentIDs: []string{"codex"}, Origins: []string{server.URL}, APIBases: []string{server.URL + "/tenant"}, AuthorizationURL: server.URL + "/grants"}}}
	policy.Station = &protocol.StationPolicy{Version: 1, APIBases: []string{server.URL + "/tenant"}}
	if alterChallenge != nil {
		alterChallenge(&challenge)
	}
	env := protocol.CommandEnvelope{CommandID: "standby-acquisition", DeviceID: station.DeviceID, Phone: true, BindingID: base.PhoneBindingID, PhoneHash: base.PhoneHash, Command: map[string]any{"type": "source.grant.acquire", "challengeId": "challenge-standby"}}
	return c, base, station, env, replies, issuerCalls
}

func TestStandbySourceGrantUsesSavedStationAndExactChallenge(t *testing.T) {
	c, base, station, env, replies, issuerCalls := standbyGrantFixture(t, nil, nil)
	c.executeStandby(base, station, env)
	reply := stationSourceRegressionReply(t, replies)
	if !reply.OK || issuerCalls.Load() != 1 || reply.DeviceID != station.DeviceID || reply.CommandID != env.CommandID {
		t.Fatalf("standby acquisition failed: %#v calls=%d", reply, issuerCalls.Load())
	}
	grantRaw, _ := json.Marshal(reply.Result)
	var grant signedSourceGrant
	if exactSourceJSON(grantRaw, []string{"payload", "signature"}, &grant) != nil {
		t.Fatal("standby did not return only the public grant")
	}
	payload, _ := base64.RawURLEncoding.DecodeString(grant.Payload)
	var snapshot sourceGrantSnapshot
	if json.Unmarshal(payload, &snapshot) != nil || snapshot.DeviceID != station.DeviceID || snapshot.CredentialHandle != APIHandle(station, "a1") || snapshot.ConfigRevision != station.AccountSourceRevision(station.LocalAccounts[0]) || strings.Contains(string(grantRaw), "key-a") {
		t.Fatal("grant did not bind the saved station's current source")
	}
	if config.RemoteIdentity(c.o.Store.Get()) != config.RemoteIdentity(base) {
		t.Fatal("acquiring a grant switched the active station")
	}
	// Feed the verified public contract into the original standby handover.
	env.CommandID = "standby-authorized-handover"
	env.Command = map[string]any{"type": "remote.station.switch", "targetHubUrl": station.EffectiveHubURL(), "targetDeviceId": station.DeviceID, "targetComputerId": base.ComputerID, "operationId": "10000000-0000-4000-8000-000000000001", "agent": "codex", "accountId": snapshot.CredentialHandle}
	env.Source = &protocol.ExecutionSource{Mode: "restricted", PolicyRevision: snapshot.PolicyRevision, Issuer: snapshot.Issuer, SourceType: snapshot.SourceType, CredentialHandle: snapshot.CredentialHandle, Origin: snapshot.Origin, APIBase: snapshot.APIBase, ConfigRevision: snapshot.ConfigRevision, Agent: snapshot.Agent, ControlSurface: snapshot.ControlSurface, Model: snapshot.Model}
	env.Source.Station = &protocol.StationPolicy{Version: 1, APIBases: []string{snapshot.APIBase}}
	c.executeStandby(base, station, env)
	reply = stationSourceRegressionReply(t, replies)
	if !reply.OK || config.RemoteIdentity(c.o.Store.Get()) != config.RemoteIdentity(station) || issuerCalls.Load() != 1 {
		t.Fatalf("issued grant did not complete the original standby handover: %#v", reply)
	}
}

func TestStandbySourceGrantRejectsUnrelatedOriginalCommandAndNonce(t *testing.T) {
	for _, name := range []string{"ordinary-task", "different-station", "different-device", "different-computer", "different-binding", "different-nonce", "different-policy", "different-hash", "expired", "invalid-source-only"} {
		t.Run(name, func(t *testing.T) {
			c, base, station, env, replies, issuerCalls := standbyGrantFixture(t, func(challenge *sourceGrantChallenge) {
				var command map[string]any
				_ = json.Unmarshal(challenge.Command, &command)
				switch name {
				case "ordinary-task":
					command["type"] = "session.send"
				case "different-station":
					command["targetHubUrl"] = "https://other.test/salcara-hub"
				case "different-device":
					command["targetDeviceId"] = "other"
				case "different-computer":
					command["targetComputerId"] = "other"
				case "different-binding":
					challenge.BindingID = strings.Repeat("f", 64)
				case "different-nonce":
					challenge.ChallengeID = "challenge-other"
				case "different-policy":
					challenge.PolicyRevision = "other"
				case "expired":
					challenge.ExpiresAt = time.Now().Unix() - 1
				case "invalid-source-only":
					command["sourceOnly"] = "true"
				}
				challenge.Command, _ = json.Marshal(command)
				challenge.CommandHash, _ = sourceCommandHash(challenge.Command)
				if name == "different-hash" {
					challenge.CommandHash = strings.Repeat("0", 64)
				}
			}, nil)
			c.executeStandby(base, station, env)
			reply := stationSourceRegressionReply(t, replies)
			if reply.OK || reply.Result != nil || issuerCalls.Load() != 0 {
				t.Fatal("unrelated challenge contacted issuer or returned a grant")
			}
		})
	}
}

func TestStandbySourceGrantRejectsChangesDuringIssuance(t *testing.T) {
	for _, name := range []string{"unbind", "station-secret", "station-removed", "active-identity", "computer", "account-key", "account-origin", "source-type"} {
		t.Run(name, func(t *testing.T) {
			c, base, station, env, replies, issuerCalls := standbyGrantFixture(t, nil, func(store *config.Store) {
				if err := store.Update(func(cfg *config.Config) error {
					switch name {
					case "unbind":
						cfg.PhoneHash = ""
					case "station-secret":
						cfg.RemoteConnections[1].DeviceSecret = strings.Repeat("f", 64)
					case "station-removed":
						cfg.RemoteConnections = cfg.RemoteConnections[:1]
					case "active-identity":
						cfg.DeviceSecret = strings.Repeat("f", 64)
					case "computer":
						cfg.ComputerID = "other"
					case "account-key":
						cfg.LocalAccounts[0].Key = "changed-synthetic-key"
					case "account-origin":
						cfg.LocalAccounts[0].BaseURL = "https://other.test/tenant"
					case "source-type":
						cfg.LocalAccounts[0].SourceType = "reverse_proxy"
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
			c.executeStandby(base, station, env)
			reply := stationSourceRegressionReply(t, replies)
			if reply.OK || reply.Result != nil || issuerCalls.Load() != 1 {
				t.Fatalf("stale grant returned after %s: %#v calls=%d", name, reply, issuerCalls.Load())
			}
		})
	}
}

func TestActiveSourceGrantStillRequiresCurrentPhoneGuard(t *testing.T) {
	c, base, station, _, _, issuerCalls := standbyGrantFixture(t, nil, nil)
	ctx := withCommandGuard(context.Background(), commandGuard{identity: config.RemoteIdentity(base), deviceID: base.DeviceID, bindingID: base.PhoneBindingID, phoneHash: strings.Repeat("f", 64)})
	if _, err := c.acquireSourceGrant(ctx, "challenge-standby"); err == nil || issuerCalls.Load() != 0 {
		t.Fatal("active acquisition ignored the phone guard")
	}
	// An active A guard cannot use the target B challenge's device identity.
	ctx = withCommandGuard(context.Background(), commandGuard{identity: config.RemoteIdentity(base), deviceID: base.DeviceID, bindingID: base.PhoneBindingID, phoneHash: base.PhoneHash})
	if _, err := c.acquireSourceGrantFor(ctx, "challenge-standby", station, func(current config.Config) bool {
		return config.RemoteIdentity(current) == config.RemoteIdentity(station)
	}, false); err == nil || issuerCalls.Load() != 0 {
		t.Fatal("caller supplied target config became active acquisition authority")
	}
}

func TestActiveSourceGrantKeepsDevicePhoneNonceAndSourceBinding(t *testing.T) {
	c, _, station, _, _, issuerCalls := standbyGrantFixture(t, func(challenge *sourceGrantChallenge) {
		var original map[string]any
		_ = json.Unmarshal(challenge.Command, &original)
		challenge.Command, _ = json.Marshal(map[string]any{"type": "session.send", "agent": "codex", "sessionKey": "codex:s", "accountId": original["accountId"], "controlSurface": "cli"})
		challenge.CommandHash, _ = sourceCommandHash(challenge.Command)
	}, nil)
	if err := c.o.Store.Update(func(cfg *config.Config) error {
		cfg.ConnectRemote(station.RelayRoot, station.EffectiveHubURL())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	current := c.o.Store.Get()
	c.publishPair(PairStatus{DeviceID: current.DeviceID, Paired: true}, eventIdentity(current))
	ctx := withCommandGuard(context.Background(), commandGuard{identity: config.RemoteIdentity(current), deviceID: current.DeviceID, bindingID: current.PhoneBindingID, phoneHash: current.PhoneHash})
	result, err := c.acquireSourceGrant(ctx, "challenge-standby")
	if err != nil || issuerCalls.Load() != 1 {
		t.Fatalf("active acquisition regressed: %v calls=%d", err, issuerCalls.Load())
	}
	grant, ok := result.(signedSourceGrant)
	if !ok {
		t.Fatal("active acquisition did not return a grant")
	}
	payload, _ := base64.RawURLEncoding.DecodeString(grant.Payload)
	var snapshot sourceGrantSnapshot
	if json.Unmarshal(payload, &snapshot) != nil || snapshot.ChallengeID != "challenge-standby" || snapshot.DeviceID != current.DeviceID || snapshot.BindingID != current.PhoneBindingID || snapshot.CredentialHandle != APIHandle(current, "a1") || snapshot.ConfigRevision != current.AccountSourceRevision(current.LocalAccounts[0]) {
		t.Fatal("active source bindings changed during refactor")
	}
}

func TestStandbySourceGrantRechecksBeforeCredentialDisclosure(t *testing.T) {
	for _, name := range []string{"unbind", "account-key", "station-secret"} {
		t.Run(name, func(t *testing.T) {
			c, base, station, env, replies, issuerCalls := standbyGrantFixture(t, nil, nil, func(store *config.Store) {
				if err := store.Update(func(cfg *config.Config) error {
					switch name {
					case "unbind":
						cfg.PhoneHash = ""
					case "account-key":
						cfg.LocalAccounts[0].Key = "changed-synthetic-key"
					case "station-secret":
						cfg.RemoteConnections[1].DeviceSecret = strings.Repeat("f", 64)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
			c.executeStandby(base, station, env)
			reply := stationSourceRegressionReply(t, replies)
			if reply.OK || reply.Result != nil || issuerCalls.Load() != 0 {
				t.Fatal("provider credential disclosed after challenge changed ownership")
			}
		})
	}
}
