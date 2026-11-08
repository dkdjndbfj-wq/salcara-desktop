package console

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"salcara/bridge/internal/config"
)

const (
	runtimeRevokeAccountID = "runtime-revoke-account"
	runtimeRevokeClientID  = "runtime-revoke-client"
	runtimeRevokeClientKey = "synthetic-runtime-revoke-client-key"
)

func seedCodexAPIRuntime(t *testing.T, s *Server) {
	t.Helper()
	s.d.Store.SetRuntimeGatewayKey("fixture-runtime-lease")
	err := s.d.Store.Update(func(c *config.Config) error {
		c.LocalAccounts = append(c.LocalAccounts, config.LocalAccount{ID: runtimeRevokeAccountID, Name: "Fixture", Kind: "api", BaseURL: "https://provider.example", Key: "synthetic-upstream", Model: "gpt-fixture", AuthMode: "bearer"})
		c.CodexAPI.Enabled = true
		c.CodexAPI.RuntimeLease = c.GatewayRuntimeKey
		c.CodexAPI.Clients = []config.NamedAPIClient{{ID: runtimeRevokeClientID, Name: "Fixture", AccountID: runtimeRevokeAccountID, KeyDigest: config.APIClientDigest(runtimeRevokeClientKey)}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func blockCodexAPIConfigSaves(t *testing.T, s *Server) {
	t.Helper()
	path := s.d.Store.Path()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func codexAPIAdmissionStatus(s *Server, key string) int {
	req := httptest.NewRequest(http.MethodGet, "/codex-api/v1/models", nil)
	req.RemoteAddr = "127.0.0.1:49123"
	req.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	s.apiService.ServeHTTP(w, req)
	return w.Code
}

func TestCodexAPIStopSaveFailureStaysInert(t *testing.T) {
	s, h := newTestServer(t)
	seedCodexAPIRuntime(t, s)
	blockCodexAPIConfigSaves(t, s)
	w := localRequest(s, h, http.MethodPost, "/api/local/codex-api/enabled", map[string]bool{"enabled": false})
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "本次进程停止") || strings.Contains(w.Body.String(), runtimeRevokeClientKey) {
		t.Fatalf("unexpected stop failure response: %d %s", w.Code, w.Body)
	}
	current := s.d.Store.Get()
	if current.CodexAPI.Enabled || current.CodexAPI.RuntimeLease != "" || current.CodexAPIActive() {
		t.Fatal("failed persistence revived the stopped service")
	}
	if got := codexAPIAdmissionStatus(s, runtimeRevokeClientKey); got != http.StatusServiceUnavailable {
		t.Fatalf("old client reached stopped service: %d", got)
	}
}

func TestCodexAPIRevokeSaveFailureKeepsOldKeyDisabled(t *testing.T) {
	s, h := newTestServer(t)
	seedCodexAPIRuntime(t, s)
	blockCodexAPIConfigSaves(t, s)
	w := localRequest(s, h, http.MethodPost, "/api/local/codex-api/clients/revoke", map[string]string{"id": runtimeRevokeClientID})
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "本次进程撤销") || strings.Contains(w.Body.String(), runtimeRevokeClientKey) {
		t.Fatalf("unexpected revoke failure response: %d %s", w.Code, w.Body)
	}
	client, ok := s.d.Store.Get().APIClient(runtimeRevokeClientID)
	if !ok || !client.Disabled || client.KeyDigest != config.APIClientDigest(runtimeRevokeClientKey) {
		t.Fatal("failed persistence restored the revoked client")
	}
	if got := codexAPIAdmissionStatus(s, runtimeRevokeClientKey); got != http.StatusUnauthorized {
		t.Fatalf("old client key remained live after failed revoke: %d", got)
	}
}

func TestCodexAPIDisableAndRotateSaveFailureKeepsOldKeyDisabled(t *testing.T) {
	s, h := newTestServer(t)
	seedCodexAPIRuntime(t, s)
	blockCodexAPIConfigSaves(t, s)
	w := localRequest(s, h, http.MethodPost, "/api/local/codex-api/clients", map[string]any{
		"id": runtimeRevokeClientID, "name": "Disabled fixture", "accountId": runtimeRevokeAccountID, "disabled": true, "rotate": true,
	})
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "本次进程停用") || strings.Contains(w.Body.String(), "sk-salcara-client-") {
		t.Fatalf("unexpected disabled edit failure response: %d %s", w.Code, w.Body)
	}
	client, ok := s.d.Store.Get().APIClient(runtimeRevokeClientID)
	if !ok || !client.Disabled || client.KeyDigest != config.APIClientDigest(runtimeRevokeClientKey) {
		t.Fatal("failed disabled rotation revived or replaced the old key")
	}
	if got := codexAPIAdmissionStatus(s, runtimeRevokeClientKey); got != http.StatusUnauthorized {
		t.Fatalf("old client key remained live after failed disabled edit: %d", got)
	}
}

func TestCodexAPIStaleStopCannotRetireNewerStart(t *testing.T) {
	s, h := newTestServer(t)
	seedCodexAPIRuntime(t, s)
	var triggered atomic.Bool
	var callbackErr atomic.Value
	s.d.Store.OnChange(func(old, next config.Config) {
		if old.CodexAPI.Enabled && !next.CodexAPI.Enabled && triggered.CompareAndSwap(false, true) {
			if err := s.d.Store.Update(func(c *config.Config) error {
				c.CodexAPI.Enabled = true
				c.CodexAPI.RuntimeLease = c.GatewayRuntimeKey
				return nil
			}); err != nil {
				callbackErr.Store(err)
			}
		}
	})
	w := localRequest(s, h, http.MethodPost, "/api/local/codex-api/enabled", map[string]bool{"enabled": false})
	if err, _ := callbackErr.Load().(error); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusConflict || !triggered.Load() || !s.d.Store.Get().CodexAPIActive() {
		t.Fatalf("stale stop affected newer start: %d %s", w.Code, w.Body)
	}
}

func TestCodexAPIStaleRevokeCannotRetireNewerRotation(t *testing.T) {
	s, h := newTestServer(t)
	seedCodexAPIRuntime(t, s)
	newKey := "synthetic-newer-client-key"
	var triggered atomic.Bool
	var callbackErr atomic.Value
	s.d.Store.OnChange(func(old, next config.Config) {
		oldClient, oldOK := old.APIClient(runtimeRevokeClientID)
		nextClient, nextOK := next.APIClient(runtimeRevokeClientID)
		if oldOK && nextOK && !oldClient.Disabled && nextClient.Disabled && triggered.CompareAndSwap(false, true) {
			if err := s.d.Store.Update(func(c *config.Config) error {
				for i := range c.CodexAPI.Clients {
					if c.CodexAPI.Clients[i].ID == runtimeRevokeClientID {
						c.CodexAPI.Clients[i].Disabled = false
						c.CodexAPI.Clients[i].KeyDigest = config.APIClientDigest(newKey)
					}
				}
				return nil
			}); err != nil {
				callbackErr.Store(err)
			}
		}
	})
	w := localRequest(s, h, http.MethodPost, "/api/local/codex-api/clients/revoke", map[string]string{"id": runtimeRevokeClientID})
	if err, _ := callbackErr.Load().(error); err != nil {
		t.Fatal(err)
	}
	client, ok := s.d.Store.Get().APIClient(runtimeRevokeClientID)
	if w.Code != http.StatusConflict || !triggered.Load() || !ok || client.Disabled || client.KeyDigest != config.APIClientDigest(newKey) {
		t.Fatalf("stale revoke affected newer rotation: %d %s", w.Code, w.Body)
	}
}
