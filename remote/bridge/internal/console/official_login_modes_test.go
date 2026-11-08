package console

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"salcara/bridge/internal/codexaccounts"
)

func TestOfficialDeviceModeHTTPChallengeCancelAndCallbackIsolation(t *testing.T) {
	var browserCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/code" {
			io.WriteString(w, `{"device_auth_id":"synthetic-private-device-id","user_code":"TEST-CODE","interval":1}`)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, "pending")
	}))
	defer provider.Close()
	device, err := codexaccounts.NewDeviceConnector(codexaccounts.DeviceOptions{ClientID: "synthetic-client", UserCodeURL: provider.URL + "/code", PollURL: provider.URL + "/poll"})
	if err != nil {
		t.Fatal(err)
	}
	vault, err := codexaccounts.New(codexaccounts.Options{
		Path: filepath.Join(t.TempDir(), "synthetic.vault"), Protector: consoleProtector{}, DeviceLogin: device,
		Login: codexaccounts.LoginFunc(func(context.Context) (*coreauth.Auth, error) {
			browserCalls.Add(1)
			return nil, codexaccounts.ErrLoginFailed
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer vault.Close()
	s, _ := newTestServer(t)
	s.d.CodexAccounts = vault
	h := s.Handler()
	w := localRequest(s, h, "POST", "/api/local/codex/login", map[string]string{"mode": "device"})
	var login codexaccounts.LoginSession
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &login) != nil || login.Mode != "device" {
		t.Fatalf("device start failed: %d %s", w.Code, w.Body.String())
	}
	waitLoginSession(t, s, login.ID, func(row codexaccounts.LoginSession) bool { return row.UserCode != "" })
	w = localRequest(s, h, "POST", "/api/local/codex/login/status", map[string]string{"id": login.ID})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "TEST-CODE") || !strings.Contains(w.Body.String(), "https://auth.openai.com/codex/device") || strings.Contains(w.Body.String(), "synthetic-private-device-id") {
		t.Fatalf("unsafe/incomplete device challenge: %d %s", w.Code, w.Body.String())
	}
	w = localRequest(s, h, "POST", "/api/local/codex/login/callback", map[string]string{"id": login.ID, "callbackURL": "http://localhost:1455/auth/callback?state=fixture&code=fixture"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("device accepted browser callback: %d", w.Code)
	}
	w = localRequest(s, h, "POST", "/api/local/codex/login/cancel", map[string]string{"id": login.ID})
	if w.Code != http.StatusOK {
		t.Fatalf("cancel failed: %d %s", w.Code, w.Body.String())
	}
	w = localRequest(s, h, "POST", "/api/local/codex/login/status", map[string]string{"id": login.ID})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"cancelled"`) || strings.Contains(w.Body.String(), "TEST-CODE") || strings.Contains(w.Body.String(), "verificationURL") {
		t.Fatalf("cancel retained challenge: %d %s", w.Code, w.Body.String())
	}
	if browserCalls.Load() != 0 || len(vault.List()) != 0 {
		t.Fatal("device action invoked browser or saved cancelled credentials")
	}
	for _, mode := range []any{"unknown", "Device", 1, nil} {
		w = localRequest(s, h, "POST", "/api/local/codex/login", map[string]any{"mode": mode})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("invalid mode %#v accepted: %d", mode, w.Code)
		}
	}
	if browserCalls.Load() != 0 {
		t.Fatal("invalid mode silently fell back to browser")
	}
}
