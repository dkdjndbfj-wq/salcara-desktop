package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func TestScopedRemoteCredentialRejectsSelectionChangedAfterAdmission(t *testing.T) {
	var hits atomic.Int32
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"id":"should-not-arrive"}`))
	}))
	defer u.Close()
	a := config.LocalAccount{ID: "a", Kind: "api", SourceType: "api_key", Key: "key-a", BaseURL: u.URL, Model: "chosen-model", Wire: "responses"}
	b := a
	b.ID, b.Key = "b", "key-b"
	c := config.Config{LocalAccounts: []config.LocalAccount{a, b}, RemoteAPI: map[string]config.RemoteAPI{"codex": {AccountID: "a", Model: "chosen-model"}}, GatewayRuntimeKey: "runtime", RemoteExecutionArmed: true}
	c.RemoteStationAuthorization = &config.StationAuthorization{Identity: config.RemoteIdentity(c), Revision: "r1", Station: protocol.StationPolicy{Version: 1, APIBases: []string{u.URL}}}
	key := c.RemoteGatewayCredential("remote-codex")
	h := New(nil, func(string) (config.LocalAccount, bool) {
		c.RemoteAPI["codex"] = config.RemoteAPI{AccountID: "b", Model: "chosen-model"}
		return c.RemoteToolAccount("codex")
	})
	h.KeyForTarget = c.RemoteGatewayCredential
	// Do not bind a method to a value snapshot: production reads the current store.
	h.KeyForTarget = func(target string) string { return c.RemoteGatewayCredential(target) }
	r := httptest.NewRequest(http.MethodPost, "/gateway/remote-codex/v1/responses", strings.NewReader(`{"model":"chosen-model","input":"fixture"}`))
	r.RemoteAddr = "127.0.0.1:10000"
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusConflict || hits.Load() != 0 {
		t.Fatalf("selection changed after admission: status %d, upstream hits %d", w.Code, hits.Load())
	}
}

func targetCredentialGateway(c *config.Config) *Handler {
	h := New(func() string { return c.GatewayKey }, func(target string) (config.LocalAccount, bool) {
		kind := "claude"
		if strings.HasSuffix(target, "codex") {
			kind = "codex"
		}
		return config.LocalAccount{ID: target, Kind: kind, Key: upstreamTestKey, Model: "chosen-model"}, true
	})
	h.KeyForTarget = func(target string) string {
		if strings.HasPrefix(target, "remote-") {
			return c.GatewayCredential()
		}
		return c.GatewayKey
	}
	return h
}

func TestGatewayTargetCredentialsRemainIsolatedAcrossLeaseRotation(t *testing.T) {
	c := config.Config{GatewayKey: localTestKey, GatewayRuntimeKey: "current-remote-lease"}
	h := targetCredentialGateway(&c)
	request := func(target, key, header string) int {
		r := httptest.NewRequest(http.MethodGet, "/gateway/"+target+"/v1/models", nil)
		r.RemoteAddr = "127.0.0.1:10000"
		if header == "Authorization" {
			key = "Bearer " + key
		}
		r.Header.Set(header, key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for _, lease := range []string{"current-remote-lease", "next-remote-lease"} {
		previous := c.GatewayRuntimeKey
		c.GatewayRuntimeKey = lease
		for _, target := range []string{"codex", "claude", "claude-desktop", "remote-codex", "remote-claude"} {
			for _, header := range []string{"Authorization", "x-api-key"} {
				valid, wrong := c.GatewayKey, lease
				if strings.HasPrefix(target, "remote-") {
					valid, wrong = lease, c.GatewayKey
				}
				if got := request(target, valid, header); got != http.StatusOK {
					t.Fatalf("%s rejected its current credential after lease rotation: %d", target, got)
				}
				if got := request(target, wrong, header); got != http.StatusUnauthorized {
					t.Fatalf("%s accepted the other route's credential: %d", target, got)
				}
				if strings.HasPrefix(target, "remote-") && previous != lease {
					if got := request(target, previous, header); got != http.StatusUnauthorized {
						t.Fatalf("%s accepted an old process lease: %d", target, got)
					}
				}
			}
		}
	}
}

func TestGatewayEmptyTargetCredentialDoesNotFallBackToLegacyKey(t *testing.T) {
	c := config.Config{GatewayKey: localTestKey, GatewayRuntimeKey: "remote-lease"}
	h := targetCredentialGateway(&c)
	h.KeyForTarget = func(string) string { return "" }
	r := httptest.NewRequest(http.MethodGet, "/gateway/remote-codex/v1/models", nil)
	r.RemoteAddr = "127.0.0.1:10000"
	r.Header.Set("Authorization", "Bearer "+localTestKey)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("empty target credential fell back to the legacy key: %d", w.Code)
	}
}

func TestGatewayLeaseRotationDuringRequestOnlyInvalidatesRemoteTarget(t *testing.T) {
	for _, target := range []string{"codex", "remote-codex"} {
		t.Run(target, func(t *testing.T) {
			c := config.Config{GatewayKey: localTestKey, GatewayRuntimeKey: "initial-remote-lease"}
			h := targetCredentialGateway(&c)
			source := h.Source
			h.Source = func(target string) (config.LocalAccount, bool) {
				c.GatewayRuntimeKey = "rotated-remote-lease"
				return source(target)
			}
			key, expected := c.GatewayKey, http.StatusOK
			if strings.HasPrefix(target, "remote-") {
				key, expected = c.GatewayRuntimeKey, http.StatusConflict
			}
			r := httptest.NewRequest(http.MethodGet, "/gateway/"+target+"/v1/models", nil)
			r.RemoteAddr = "127.0.0.1:10000"
			r.Header.Set("Authorization", "Bearer "+key)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != expected {
				t.Fatalf("%s used the wrong credential for in-flight validation: %d", target, w.Code)
			}
		})
	}
}

func TestWebSocketTargetCredentialsAndLeaseRotation(t *testing.T) {
	for _, target := range []string{"codex", "remote-codex"} {
		t.Run(target, func(t *testing.T) {
			c := config.Config{GatewayKey: localTestKey, GatewayRuntimeKey: "initial-remote-lease"}
			h := targetCredentialGateway(&c)
			var credentialMu sync.RWMutex
			credential := h.KeyForTarget
			h.KeyForTarget = func(target string) string {
				credentialMu.RLock()
				defer credentialMu.RUnlock()
				return credential(target)
			}
			s := httptest.NewServer(h)
			defer s.Close()
			url := "ws" + strings.TrimPrefix(s.URL, "http") + "/gateway/" + target + "/v1/responses"
			valid, wrong := c.GatewayKey, c.GatewayRuntimeKey
			if strings.HasPrefix(target, "remote-") {
				valid, wrong = c.GatewayRuntimeKey, c.GatewayKey
			}
			conn, response, err := websocket.DefaultDialer.Dial(url, http.Header{"Authorization": []string{"Bearer " + wrong}})
			if conn != nil {
				_ = conn.Close()
			}
			if response != nil {
				_ = response.Body.Close()
			}
			if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
				t.Fatal("socket accepted the other route's credential")
			}
			conn, _, err = websocket.DefaultDialer.Dial(url, http.Header{"Authorization": []string{"Bearer " + valid}})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			warmup := map[string]any{"type": "response.create", "model": "chosen-model", "generate": false, "input": "original context"}
			wsSend(t, conn, warmup)
			wsUntil(t, conn, "response.completed")
			credentialMu.Lock()
			c.GatewayRuntimeKey = "rotated-remote-lease"
			credentialMu.Unlock()
			wsSend(t, conn, warmup)
			if strings.HasPrefix(target, "remote-") {
				event := wsUntil(t, conn, "error")
				if event["error"].(map[string]any)["code"] != "configuration_changed" {
					t.Fatal("socket retained a rotated remote lease")
				}
			} else {
				wsUntil(t, conn, "response.completed")
			}
		})
	}
}

func TestRemoteWebSocketPinsItsOwnTargetForForwarding(t *testing.T) {
	h := fixtureGateway(t, "codex", "responses", wsCompletedFixture, "text/event-stream", nil)
	source := h.Source
	h.Source = func(target string) (config.LocalAccount, bool) {
		if target != "remote-codex" {
			t.Errorf("remote socket consulted the local tool route %q", target)
			return config.LocalAccount{}, false
		}
		return source(target)
	}
	h.KeyForTarget = func(target string) string {
		if target == "remote-codex" {
			return "remote-process-lease"
		}
		return localTestKey
	}
	s := httptest.NewServer(h)
	defer s.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(s.URL, "http")+"/gateway/remote-codex/v1/responses", http.Header{"Authorization": []string{"Bearer remote-process-lease"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	wsSend(t, conn, map[string]any{"type": "response.create", "model": "chosen-model", "input": "original context"})
	wsUntil(t, conn, "response.completed")
}
