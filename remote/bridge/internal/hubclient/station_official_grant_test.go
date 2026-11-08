package hubclient

import (
	"context"
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

func TestStationOfficialAcquisitionUsesAuthenticatedChallengeWithoutTokens(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "explicitly-enabled"}[allow], func(t *testing.T) {
			c, store, account, _ := sourceGuardFixture(t, "official_account")
			account.ProviderID, account.CredentialRef, account.BaseURL, account.Wire = "openai", "protected-fixture-ref", "https://chatgpt.com/backend-api/codex", "responses"
			var cfg config.Config
			var challenge sourceGrantChallenge
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/salcara-hub/v1/bridge/source/challenge" || r.Header.Get("X-Salcara-Device-Id") != cfg.DeviceID || r.Header.Get("X-Salcara-Device-Secret") != cfg.DeviceSecret {
					t.Error("grant bypassed authenticated challenge")
					w.WriteHeader(403)
					return
				}
				policy := sourceGrantPolicy{Mode: "restricted", Revision: "official-policy", Audience: "station", Issuers: []sourceIssuer{}, Station: &protocol.StationPolicy{Version: 1, APIBases: []string{}, AllowOfficialAccounts: allow}}
				_ = json.NewEncoder(w).Encode(map[string]any{"challenge": challenge, "policy": policy})
			}))
			defer server.Close()
			if err := store.Update(func(s *config.Config) error {
				s.LocalAccounts[0] = account
				s.ConnectRemote(server.URL, server.URL+"/salcara-hub")
				s.PhoneBindingID, s.PhoneHash = strings.Repeat("a", 64), strings.Repeat("b", 64)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			cfg = store.Get()
			c.publishPair(PairStatus{DeviceID: cfg.DeviceID, Paired: true}, eventIdentity(cfg))
			cmd, _ := json.Marshal(map[string]any{"type": "session.send", "tool": "codex", "sessionKey": "codex:fixture", "accountId": APIHandle(cfg, account.ID), "controlSurface": "cli"})
			hash, _ := sourceCommandHash(cmd)
			challenge = sourceGrantChallenge{ChallengeID: "challenge-official", Audience: "station", PolicyRevision: "official-policy", DeviceID: cfg.DeviceID, BindingID: cfg.PhoneBindingID, RequestID: "10000000-0000-4000-8000-000000000001", CommandHash: hash, Command: cmd, ExpiresAt: time.Now().Unix() + 120}
			ctx := withCommandGuard(context.Background(), commandGuard{identity: config.RemoteIdentity(cfg), deviceID: cfg.DeviceID, bindingID: cfg.PhoneBindingID, phoneHash: cfg.PhoneHash})
			result, err := c.acquireSourceGrant(ctx, challenge.ChallengeID)
			if !allow {
				if err == nil {
					t.Fatal("unapproved official grant acquired")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(result)
			if strings.Contains(string(body), account.Key) || strings.Contains(string(body), account.CredentialRef) || strings.Contains(string(body), cfg.DeviceSecret) {
				t.Fatal("credential leaked in account attestation")
			}
			attestation := result.(map[string]any)["officialAccount"].(sourceGrantSnapshot)
			if attestation.Issuer != "station-official" || attestation.SourceType != "official_account_proxy" || attestation.APIBase != account.BaseURL || attestation.ConfigRevision != cfg.AccountSourceRevision(account) || attestation.CommandHash != hash || calls.Load() != 1 {
				t.Fatalf("unbound official attestation: %+v", attestation)
			}
		})
	}
}
