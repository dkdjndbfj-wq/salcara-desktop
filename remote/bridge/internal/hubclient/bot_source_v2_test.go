package hubclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func botSourceClientFixture(t *testing.T, root string) (*Client, *config.Store, config.LocalAccount, string) {
	t.Helper()
	c, store, account, _ := sourceGuardFixture(t, "official_account")
	account.ProviderID, account.CredentialRef, account.BaseURL, account.Wire = "openai", "protected-fixture-ref", "https://chatgpt.com/backend-api/codex", "responses"
	if err := store.Update(func(cfg *config.Config) error {
		cfg.LocalAccounts[0] = account
		cfg.ConnectRemoteWithBotBinding(root, root+"/salcara-hub", "test-bot-group", "invite", "signature")
		// A QR belonging to another saved Hub must remain independent.
		cfg.PhoneBindingID, cfg.PhoneHash = strings.Repeat("a", 64), strings.Repeat("b", 64)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	scope := strings.Repeat("c", 64)
	return c, store, account, scope
}

func TestBotSourceV2CommandRequiresConnectedAuthenticatedScope(t *testing.T) {
	c, store, _, scope := botSourceClientFixture(t, "https://bot.test")
	cfg := store.Get()
	guard := commandGuard{identity: config.RemoteIdentity(cfg), deviceID: cfg.DeviceID, eventScope: scope}
	ctx := withCommandGuard(context.Background(), guard)
	c.setState(StateConnected, "")
	if c.canUpload(cfg) || c.validateCommandGuard(ctx, "session.send") == nil {
		t.Fatal("Bot mode without handshake authorized execution")
	}
	if err := c.acceptEventScope(cfg, scope, true); err != nil {
		t.Fatal(err)
	}
	if !c.canUpload(cfg) || c.validateCommandGuard(ctx, "session.send") != nil {
		t.Fatal("authenticated Bot generation incorrectly required QR")
	}
	for _, bad := range []string{"", strings.Repeat("d", 64)} {
		guard.eventScope = bad
		if c.validateCommandGuard(withCommandGuard(context.Background(), guard), "session.send") == nil {
			t.Fatal("missing or wrong generation accepted")
		}
	}
	before := store.Get()
	c.acceptPairState(cfg, `{"deviceId":"`+cfg.DeviceID+`","paired":false,"revision":1,"bindingId":"`+cfg.PhoneBindingID+`","phoneHash":"`+cfg.PhoneHash+`"}`)
	if got := store.Get(); got.PhoneBindingID != before.PhoneBindingID || got.PhoneHash != before.PhoneHash {
		t.Fatal("Bot QR snapshot revoked unrelated saved Hub pair")
	}
	c.setState(StateConnecting, "disconnected")
	if c.canUpload(cfg) || c.eventScopeFor(cfg) != "" {
		t.Fatal("disconnect retained Bot handshake authority")
	}
	c.setState(StateConnected, "")
	if c.canUpload(cfg) || c.validateCommandGuard(ctx, "session.send") == nil {
		t.Fatal("reconnect status revived old scope before handshake")
	}
	if err := c.acceptEventScope(cfg, strings.Repeat("d", 64), true); err != nil {
		t.Fatal(err)
	}
	if c.validateCommandGuard(ctx, "session.send") == nil {
		t.Fatal("old queued command followed replacement phone")
	}
	retired, cancel := context.WithCancel(context.Background())
	cancel()
	c.handleSSEFor(retired, "heartbeat", `{"eventScope":"`+scope+`","eventScopeRequired":true}`, cfg)
	if c.eventScopeFor(cfg) != strings.Repeat("d", 64) || c.validateCommandGuard(ctx, "session.send") == nil {
		t.Fatal("buffered heartbeat from a cancelled stream restored old phone authority")
	}
}

func TestBotSourceV2OfficialAcquisitionBindsGroupAndGeneration(t *testing.T) {
	for _, mode := range []string{"current", "wrong-group", "old-generation", "revoked-during-fetch"} {
		t.Run(mode, func(t *testing.T) {
			var c *Client
			var cfg config.Config
			var challenge sourceGrantChallenge
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Salcara-Device-Id") != cfg.DeviceID || r.Header.Get("X-Salcara-Device-Secret") != cfg.DeviceSecret {
					t.Error("challenge lacked device proof")
					w.WriteHeader(403)
					return
				}
				if mode == "revoked-during-fetch" {
					_ = c.acceptEventScope(cfg, strings.Repeat("e", 64), true)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"challenge": challenge, "policy": sourceGrantPolicy{Mode: "restricted", Revision: "bot-v2", Audience: "station", Issuers: []sourceIssuer{}, Station: &protocol.StationPolicy{Version: 2, APIBases: []string{}, AllowOfficialAccounts: true}}})
			}))
			defer server.Close()
			var store *config.Store
			var account config.LocalAccount
			var scope string
			c, store, account, scope = botSourceClientFixture(t, server.URL)
			cfg = store.Get()
			c.setState(StateConnected, "")
			if err := c.acceptEventScope(cfg, scope, true); err != nil {
				t.Fatal(err)
			}
			group, _, _ := cfg.BotEnrollment()
			bindingScope := scope
			if mode == "wrong-group" {
				group = "other-group"
			}
			if mode == "old-generation" {
				bindingScope = strings.Repeat("f", 64)
			}
			binding := sha256.Sum256([]byte("salcara-bot-source-v1\x00" + group + "\x00" + bindingScope))
			command, _ := json.Marshal(map[string]any{"type": "session.send", "tool": "codex", "sessionKey": "codex:fixture", "accountId": APIHandle(cfg, account.ID), "controlSurface": "cli"})
			hash, _ := sourceCommandHash(command)
			challenge = sourceGrantChallenge{ChallengeID: "challenge-bot", Audience: "station", PolicyRevision: "bot-v2", DeviceID: cfg.DeviceID, BindingID: hex.EncodeToString(binding[:]), RequestID: "10000000-0000-4000-8000-000000000001", CommandHash: hash, Command: command, ExpiresAt: time.Now().Unix() + 120}
			ctx := withCommandGuard(context.Background(), commandGuard{identity: config.RemoteIdentity(cfg), deviceID: cfg.DeviceID, eventScope: scope})
			result, err := c.acquireSourceGrant(ctx, challenge.ChallengeID)
			if mode != "current" {
				if err == nil {
					t.Fatal("stale/cross-group Bot source acquired")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			assertion := result.(map[string]any)["officialAccount"].(sourceGrantSnapshot)
			if assertion.BindingID != challenge.BindingID || assertion.BindingID == cfg.PhoneBindingID || assertion.ChallengeID != challenge.ChallengeID {
				t.Fatal("Bot assertion inherited QR or unrelated source binding")
			}
		})
	}
}
