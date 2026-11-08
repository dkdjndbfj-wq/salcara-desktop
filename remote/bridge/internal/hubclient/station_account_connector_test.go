package hubclient

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/gateway"
	"salcara/bridge/internal/protocol"
)

// The fixture models an authenticated Hub challenge and a same-origin station
// verifier. All credentials are synthetic; no native process or provider is used.
type stationAccountConnectorFixture struct {
	client        *Client
	store         *config.Store
	account       config.LocalAccount
	challenge     sourceGrantChallenge
	policy        sourceGrantPolicy
	command       map[string]any
	issuerCalls   atomic.Int32
	challengeHook func()
	issuerHook    func(*sourceGrantSnapshot)
	denyIssuer    bool
}

func newStationAccountConnectorFixture(t *testing.T, source, agent string) *stationAccountConnectorFixture {
	t.Helper()
	c, store, account, _ := sourceGuardFixture(t, source)
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	f := &stationAccountConnectorFixture{client: c, store: store}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/salcara-hub/v1/bridge/source/challenge":
			cfg := store.Get()
			if r.Header.Get("X-Salcara-Device-Id") != cfg.DeviceID || r.Header.Get("X-Salcara-Device-Secret") != cfg.DeviceSecret || r.URL.Query().Get("challengeId") != f.challenge.ChallengeID {
				t.Error("challenge was not bound to the authenticated station")
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if f.challengeHook != nil {
				f.challengeHook()
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"challenge": f.challenge, "policy": f.policy})
		case "/grants":
			f.issuerCalls.Add(1)
			if r.Header.Get("X-Salcara-Device-Secret") != "" || r.Header.Get("Authorization") != "" || r.Header.Get("X-Salcara-Pair-Token") != "" {
				t.Error("Hub or phone secret crossed the issuer boundary")
			}
			var input struct {
				Version    int                 `json:"version"`
				Snapshot   sourceGrantSnapshot `json:"snapshot"`
				Credential struct {
					Type, Value string
				} `json:"credential"`
			}
			if json.NewDecoder(r.Body).Decode(&input) != nil || input.Version != 1 || input.Credential.Type != "api_key" || input.Credential.Value != "synthetic-station-key" || input.Snapshot.SourceType != "verified_proxy" {
				t.Error("issuer received the wrong source or credential contract")
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if f.denyIssuer {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if f.issuerHook != nil {
				f.issuerHook(&input.Snapshot)
			}
			input.Snapshot.IssuedAt = time.Now().Unix()
			input.Snapshot.ExpiresAt = input.Snapshot.IssuedAt + 60
			payload, _ := json.Marshal(input.Snapshot)
			_ = json.NewEncoder(w).Encode(signedSourceGrant{Payload: base64.RawURLEncoding.EncodeToString(payload), Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, payload))})
		default:
			t.Errorf("unexpected external request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	previousTransport := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = previousTransport })
	c.o.HTTP = server.Client()
	account.BaseURL, account.Key, account.Model, account.Wire = server.URL+"/tenant", "synthetic-station-key", "gpt-fixture", "responses"
	if source == "official_account" {
		account.ProviderID, account.CredentialRef, account.CredentialEpoch = "openai", "synthetic-vault-account", 17
		account.BaseURL, account.Key = "https://chatgpt.com/backend-api/codex", ""
		account.ModelDenylist = []string{"forbidden-model"}
	}
	if err := store.Update(func(cfg *config.Config) error {
		cfg.LocalAccounts[0] = account
		cfg.ConnectRemote(server.URL, server.URL+"/salcara-hub")
		cfg.PhoneBindingID, cfg.PhoneHash = strings.Repeat("a", 64), strings.Repeat("b", 64)
		cfg.RemoteAPI = map[string]config.RemoteAPI{agent: {AccountID: account.ID, Model: account.Model}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cfg := store.Get()
	c.publishPair(PairStatus{DeviceID: cfg.DeviceID, Paired: true}, eventIdentity(cfg))
	f.account = account
	f.command = map[string]any{"type": "session.send", "agent": agent, "tool": agent, "sessionKey": agent + ":fixture", "accountId": APIHandle(cfg, account.ID), "model": account.Model, "controlSurface": "cli"}
	command, _ := json.Marshal(f.command)
	hash, _ := sourceCommandHash(command)
	f.challenge = sourceGrantChallenge{ChallengeID: "challenge-account-connector", Audience: "synthetic-station", PolicyRevision: "connector-r1", DeviceID: cfg.DeviceID, BindingID: cfg.PhoneBindingID, RequestID: "10000000-0000-4000-8000-000000000001", CommandHash: hash, Command: command, ExpiresAt: time.Now().Unix() + 120}
	f.policy = sourceGrantPolicy{Mode: "restricted", Revision: f.challenge.PolicyRevision, Audience: f.challenge.Audience,
		Station: &protocol.StationPolicy{Version: 1, APIBases: []string{server.URL + "/tenant"}, AllowOfficialAccounts: source == "official_account"},
		Issuers: []sourceIssuer{{ID: "synthetic-station-verifier", PublicKey: base64.RawURLEncoding.EncodeToString(public), SourceTypes: []string{"verified_proxy"}, AgentIDs: []string{"codex", "claude"}, Origins: []string{server.URL}, APIBases: []string{server.URL + "/tenant"}, AuthorizationURL: server.URL + "/grants"}}}
	return f
}

func (f *stationAccountConnectorFixture) acquire() (any, error) {
	cfg := f.store.Get()
	ctx := withCommandGuard(context.Background(), commandGuard{identity: config.RemoteIdentity(cfg), deviceID: cfg.DeviceID, bindingID: cfg.PhoneBindingID, phoneHash: cfg.PhoneHash})
	return f.client.acquireSourceGrant(ctx, f.challenge.ChallengeID)
}

func (f *stationAccountConnectorFixture) sourceEnvelope(s sourceGrantSnapshot) protocol.CommandEnvelope {
	return protocol.CommandEnvelope{Command: f.command, Source: &protocol.ExecutionSource{Mode: "restricted", PolicyRevision: s.PolicyRevision,
		Issuer: s.Issuer, SourceType: s.SourceType, CredentialHandle: s.CredentialHandle, Origin: s.Origin, APIBase: s.APIBase,
		ConfigRevision: s.ConfigRevision, Agent: s.Agent, ControlSurface: s.ControlSurface, Model: s.Model, Station: config.CloneStationPolicy(f.policy.Station)}}
}

func TestStationReverseProxyRequiresRegisteredMatchingVerifier(t *testing.T) {
	for _, name := range []string{"valid", "site-api-only", "different-origin", "different-tenant", "ambiguous", "verifier-denied", "signed-wrong-source", "signed-wrong-base", "oauth-label", "fake-official", "credential-ref"} {
		t.Run(name, func(t *testing.T) {
			f := newStationAccountConnectorFixture(t, "reverse_proxy", "codex")
			calls := int32(0)
			switch name {
			case "valid":
				calls = 1
			case "site-api-only":
				f.policy.Issuers[0].SourceTypes = []string{"site_api"}
			case "different-origin":
				f.policy.Issuers[0].AuthorizationURL = "https://unrelated.invalid/grants"
			case "different-tenant":
				f.policy.Issuers[0].APIBases[0] += "-other"
			case "ambiguous":
				f.policy.Issuers = append(f.policy.Issuers, f.policy.Issuers[0])
			case "verifier-denied":
				calls, f.denyIssuer = 1, true
			case "signed-wrong-source":
				calls = 1
				f.issuerHook = func(s *sourceGrantSnapshot) { s.SourceType = "site_api" }
			case "signed-wrong-base":
				calls = 1
				f.issuerHook = func(s *sourceGrantSnapshot) { s.APIBase += "/other" }
			default:
				if err := f.store.Update(func(cfg *config.Config) error {
					switch name {
					case "oauth-label":
						cfg.LocalAccounts[0].SourceType = "oauth"
					case "fake-official":
						cfg.LocalAccounts[0].SourceType = "official_account"
					case "credential-ref":
						cfg.LocalAccounts[0].CredentialRef = "must-never-leave-computer"
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			result, err := f.acquire()
			if name != "valid" {
				if err == nil || result != nil || f.issuerCalls.Load() != calls {
					t.Fatalf("unverified proxy accepted or unexpected disclosure: err=%v calls=%d", err, f.issuerCalls.Load())
				}
				return
			}
			grant, ok := result.(signedSourceGrant)
			if err != nil || !ok || f.issuerCalls.Load() != 1 {
				t.Fatalf("registered proxy failed: %v", err)
			}
			payload, _ := base64.RawURLEncoding.DecodeString(grant.Payload)
			var snapshot sourceGrantSnapshot
			if json.Unmarshal(payload, &snapshot) != nil || snapshot.SourceType != "verified_proxy" || snapshot.ConfigRevision != f.store.Get().AccountSourceRevision(f.account) || strings.Contains(string(payload), f.account.Key) {
				t.Fatal("proxy grant did not bind the private current source")
			}
			if err := f.client.validateExecutionSource(f.sourceEnvelope(snapshot)); err != nil {
				t.Fatalf("verified proxy failed final Bridge validation: %v", err)
			}
		})
	}
}

func TestStationAccountConnectorDiscardsChangedSourceDuringAcquisition(t *testing.T) {
	for _, source := range []string{"official_account", "reverse_proxy"} {
		for _, change := range []string{"identity", "account", "binding"} {
			t.Run(source+"/"+change, func(t *testing.T) {
				f := newStationAccountConnectorFixture(t, source, "claude")
				mutate := func() {
					if err := f.store.Update(func(cfg *config.Config) error {
						switch change {
						case "identity":
							cfg.ConnectRemote("http://127.0.0.1:1", "http://127.0.0.1:1/salcara-hub")
						case "account":
							if source == "official_account" {
								cfg.LocalAccounts[0].CredentialEpoch++
							} else {
								cfg.LocalAccounts[0].Key = "synthetic-replacement-key"
							}
						case "binding":
							cfg.PhoneHash = strings.Repeat("c", 64)
						}
						return nil
					}); err != nil {
						t.Error(err)
					}
				}
				if source == "official_account" {
					f.challengeHook = mutate
				} else {
					f.issuerHook = func(*sourceGrantSnapshot) { mutate() }
				}
				if result, err := f.acquire(); err == nil || result != nil {
					t.Fatal("late connector result revived a changed account, phone or station")
				}
			})
		}
	}
}

func TestStationOfficialCodexAndClaudeExecuteOnlyBoundAccount(t *testing.T) {
	for _, agent := range []string{"codex", "claude"} {
		t.Run(agent, func(t *testing.T) {
			f := newStationAccountConnectorFixture(t, "official_account", agent)
			result, err := f.acquire()
			if err != nil {
				t.Fatal(err)
			}
			snapshot := result.(map[string]any)["officialAccount"].(sourceGrantSnapshot)
			env := f.sourceEnvelope(snapshot)
			if err := f.client.bindExecutionStation(context.Background(), env); err != nil {
				t.Fatal(err)
			}
			target := "remote-" + agent
			key := f.store.Get().RemoteGatewayCredential(target)
			if key == "" || f.issuerCalls.Load() != 0 {
				t.Fatal("official connector lacked a managed route or disclosed to external issuer")
			}
			h := gateway.New(nil, func(target string) (config.LocalAccount, bool) { return f.store.Get().GatewaySourceAccount(target) })
			h.KeyForTarget = func(target string) string { return f.store.Get().RemoteGatewayCredential(target) }
			h.AuthorizationRevision = func(target string) string { return f.store.Get().GatewayAuthorizationRevision(target) }
			h.Client = &http.Client{Transport: sourceGrantRoundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Error("official account escaped its protected executor")
				return nil, errors.New("unexpected request")
			})}
			var executes atomic.Int32
			h.Official = func(_ context.Context, selected config.LocalAccount, body []byte, stream, compact bool, current func() bool) (*http.Response, error) {
				executes.Add(1)
				var converted map[string]json.RawMessage
				if selected.ID != f.account.ID || selected.CredentialRef != f.account.CredentialRef || selected.CredentialEpoch != f.account.CredentialEpoch || !current() || compact || json.Unmarshal(body, &converted) != nil || string(converted["model"]) != `"gpt-fixture"` || len(converted["input"]) == 0 || len(converted["messages"]) != 0 {
					t.Error("official execution lost exact account/model or Responses translation")
				}
				payload := `{"id":"synthetic-response","object":"response","model":"gpt-fixture","status":"completed","output":[{"type":"message","id":"synthetic-message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"synthetic answer"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`
				contentType := "application/json"
				if stream {
					payload = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + payload + "}\n\n"
					contentType = "text/event-stream"
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(payload))}, nil
			}
			call := func(model string) *httptest.ResponseRecorder {
				path, body := "responses", `{"model":"`+model+`","input":"synthetic question"}`
				if agent == "claude" {
					path, body = "messages", `{"model":"`+model+`","max_tokens":32,"messages":[{"role":"user","content":"synthetic question"}]}`
				}
				req := httptest.NewRequest(http.MethodPost, "/gateway/"+target+"/v1/"+path, strings.NewReader(body))
				req.RemoteAddr = "127.0.0.1:15223"
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+key)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				return w
			}
			w := call("gpt-fixture")
			if w.Code != http.StatusOK || executes.Load() != 1 || !strings.Contains(w.Body.String(), "synthetic answer") {
				t.Fatalf("official %s did not complete: %d %s", agent, w.Code, w.Body.String())
			}
			w = call("forbidden-model")
			if w.Code != http.StatusForbidden || executes.Load() != 1 {
				t.Fatal("official route bypassed the account's model policy")
			}
			// An operator's updated observation retires the account route and
			// queued commands; the previously issued worker key cannot use it.
			cfg := f.store.Get()
			f.store.ObserveRemoteStation(cfg, &config.StationObservation{Mode: "restricted", Revision: "connector-r2", Station: &protocol.StationPolicy{Version: 1, APIBases: []string{}}, CheckedAt: time.Now().UnixMilli()}, f.store.BeginRemoteStationObservation(cfg))
			if err := f.client.bindExecutionStation(context.Background(), env); err == nil {
				t.Fatal("old official assertion restored withdrawn station permission")
			}
			w = call("gpt-fixture")
			if w.Code != http.StatusUnauthorized || executes.Load() != 1 {
				t.Fatal("revoked official station route reached executor")
			}
		})
	}
}

func TestStationOfficialConnectorDoesNotAcceptUnsupportedRoutesOrOAuthLabels(t *testing.T) {
	for _, name := range []string{"hermes", "native", "disabled", "oauth-label", "wrong-provider", "unprotected", "wrong-upstream"} {
		t.Run(name, func(t *testing.T) {
			agent := "codex"
			if name == "hermes" {
				agent = "hermes"
			}
			f := newStationAccountConnectorFixture(t, "official_account", agent)
			switch name {
			case "native":
				f.command["controlSurface"] = "desktop"
				f.command["type"] = "desktop.session.send"
				f.challenge.Command, _ = json.Marshal(f.command)
				f.challenge.CommandHash, _ = sourceCommandHash(f.challenge.Command)
			case "disabled":
				f.policy.Station.AllowOfficialAccounts = false
			default:
				if err := f.store.Update(func(cfg *config.Config) error {
					switch name {
					case "oauth-label":
						cfg.LocalAccounts[0].SourceType = "oauth"
					case "wrong-provider":
						cfg.LocalAccounts[0].ProviderID = "unregistered"
					case "unprotected":
						cfg.LocalAccounts[0].CredentialRef = ""
					case "wrong-upstream":
						cfg.LocalAccounts[0].BaseURL += "/other"
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if result, err := f.acquire(); err == nil || result != nil || f.issuerCalls.Load() != 0 {
				t.Fatal("unsupported account route became an official connector proof")
			}
		})
	}
}
