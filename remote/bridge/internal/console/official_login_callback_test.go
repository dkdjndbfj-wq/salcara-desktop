package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/codexaccounts"
)

func manualLoginConsole(t *testing.T) (*Server, http.Handler, *atomic.Int32) {
	t.Helper()
	calls := new(atomic.Int32)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" || r.Method != http.MethodPost {
			t.Error("unexpected synthetic provider request")
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(officialFixture))
	}))
	t.Cleanup(provider.Close)
	connector, err := codexaccounts.NewBrowserConnector(codexaccounts.BrowserOptions{
		FixtureEndpoints: &codexaccounts.BrowserFixtureEndpoints{AuthURL: provider.URL + "/authorize", TokenURL: provider.URL + "/token", RedirectURI: "http://127.0.0.1:0/auth/callback"},
	})
	if err != nil {
		t.Fatal(err)
	}
	vault, err := codexaccounts.New(codexaccounts.Options{Path: filepath.Join(t.TempDir(), "synthetic.vault"), Protector: consoleProtector{}, Login: connector})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(vault.Close)
	s, _ := newTestServer(t)
	s.d.CodexAccounts = vault
	return s, s.Handler(), calls
}

func waitLoginSession(t *testing.T, s *Server, id string, ready func(codexaccounts.LoginSession) bool) codexaccounts.LoginSession {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		row, err := s.d.CodexAccounts.LoginStatus(id)
		if err != nil {
			t.Fatal(err)
		}
		if ready(row) {
			return row
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("synthetic login did not reach expected state")
	return codexaccounts.LoginSession{}
}

func TestOfficialManualCallbackUsesCurrentLoginAndSanitizedHTTPContract(t *testing.T) {
	s, h, calls := manualLoginConsole(t)
	w := localRequest(s, h, "POST", "/api/local/codex/login", map[string]string{})
	var login codexaccounts.LoginSession
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &login) != nil {
		t.Fatal("synthetic login did not start")
	}
	progress := waitLoginSession(t, s, login.ID, func(row codexaccounts.LoginSession) bool { return row.AuthorizationURL != "" })
	auth, _ := url.Parse(progress.AuthorizationURL)
	callback, _ := url.Parse(auth.Query().Get("redirect_uri"))
	callback.RawQuery = url.Values{"state": {auth.Query().Get("state")}, "code": {"synthetic-code-private"}}.Encode()
	second, err := s.d.CodexAccounts.StartLogin(context.Background(), codexaccounts.LoginRequest{})
	if err != nil {
		t.Fatal(err)
	}
	w = localRequest(s, h, "POST", "/api/local/codex/login/callback", map[string]string{"id": second.ID, "callbackURL": callback.String()})
	if w.Code != http.StatusBadRequest || calls.Load() != 0 {
		t.Fatal("cross-session callback was accepted")
	}
	_ = s.d.CodexAccounts.CancelLogin(second.ID)
	for _, raw := range []string{
		`{"id":"` + login.ID + `","callbackURL":"first","callbackURL":"synthetic-code-private"}`,
		`{"id":"` + login.ID + `","CallbackURL":"synthetic-code-private"}`,
		`{"id":"` + login.ID + `","callbackURL":null}`,
		`{"id":"` + login.ID + `","callbackURL":"synthetic-code-private"} {}`,
	} {
		w = do(h, "POST", "/api/local/codex/login/callback", "127.0.0.1:47831", map[string]string{"Cookie": CookieName + "=" + s.Token(), "Content-Type": "application/json"}, raw)
		if w.Code != http.StatusBadRequest || calls.Load() != 0 || strings.Contains(w.Body.String(), "synthetic-code-private") {
			t.Fatal("ambiguous or bare callback accepted or echoed")
		}
	}
	w = localRequest(s, h, "POST", "/api/local/codex/login/callback", map[string]string{"id": login.ID, "callbackURL": callback.String()})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ok":true`) || strings.Contains(w.Body.String(), "synthetic-code-private") {
		t.Fatalf("valid callback submission: %d %s", w.Code, w.Body)
	}
	done := waitLoginSession(t, s, login.ID, func(row codexaccounts.LoginSession) bool { return row.Status != "pending" })
	if done.Status != "complete" || calls.Load() != 1 || done.AuthorizationURL != "" || done.ExpiresAt != nil {
		t.Fatal("manual login did not complete once or retained its challenge")
	}
	w = localRequest(s, h, "POST", "/api/local/codex/login/callback", map[string]string{"id": login.ID, "callbackURL": callback.String()})
	if w.Code != http.StatusConflict || calls.Load() != 1 || strings.Contains(w.Body.String(), "synthetic-code-private") {
		t.Fatal("completed callback was reusable or exposed code")
	}
	w = localRequest(s, h, "POST", "/api/local/codex/login/status", map[string]string{"id": login.ID})
	if w.Code != http.StatusOK {
		t.Fatal("completed account could not attach")
	}
	assertNoOfficialSecrets(t, w.Body.String())
}

func TestOfficialManualCallbackAfterCancelOrWithoutCookieCannotExchange(t *testing.T) {
	s, h, calls := manualLoginConsole(t)
	login, err := s.d.CodexAccounts.StartLogin(context.Background(), codexaccounts.LoginRequest{})
	if err != nil {
		t.Fatal(err)
	}
	progress := waitLoginSession(t, s, login.ID, func(row codexaccounts.LoginSession) bool { return row.AuthorizationURL != "" })
	auth, _ := url.Parse(progress.AuthorizationURL)
	callback, _ := url.Parse(auth.Query().Get("redirect_uri"))
	callback.RawQuery = url.Values{"state": {auth.Query().Get("state")}, "code": {"synthetic-code-private"}}.Encode()
	raw, _ := json.Marshal(map[string]string{"id": login.ID, "callbackURL": callback.String()})
	w := do(h, "POST", "/api/local/codex/login/callback", "127.0.0.1:47831", map[string]string{"Content-Type": "application/json"}, string(raw))
	if w.Code == http.StatusOK || calls.Load() != 0 {
		t.Fatal("unauthenticated callback exchanged credentials")
	}
	if err := s.d.CodexAccounts.CancelLogin(login.ID); err != nil {
		t.Fatal(err)
	}
	w = localRequest(s, h, "POST", "/api/local/codex/login/callback", map[string]string{"id": login.ID, "callbackURL": callback.String()})
	if w.Code != http.StatusConflict || calls.Load() != 0 {
		t.Fatal("cancelled callback exchanged credentials")
	}
}
