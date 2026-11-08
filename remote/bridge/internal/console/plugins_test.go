package console

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/protocol"
)

func pluginConsoleRequest(h http.Handler, s *Server, path, body, remote, origin string, authorized bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Host = "127.0.0.1:47831"
	r.RemoteAddr = remote
	r.Header.Set("Content-Type", "application/json")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if authorized {
		r.AddCookie(&http.Cookie{Name: CookieName, Value: s.Token()})
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestDesktopPluginLeaseRequiresLocalConsoleAndPreservesReplacement(t *testing.T) {
	s, h := newTestServer(t)
	body := `{"port":41234,"token":"synthetic-plugin-console-token"}`
	for _, c := range []struct {
		remote, origin string
		auth           bool
		status         int
	}{{"127.0.0.1:12", "", false, 401}, {"127.0.0.1:12", "https://evil.test", true, 403}, {"192.168.1.2:12", "", true, 403}, {"127.0.0.1:12", "", true, 200}} {
		w := pluginConsoleRequest(h, s, "/api/local/plugins/connect", body, c.remote, c.origin, c.auth)
		if w.Code != c.status {
			t.Fatalf("lease auth %v: %d %s", c, w.Code, w.Body)
		}
	}
	if !s.pluginHooks.Enabled() || s.apiService.Plugins != s.pluginHooks {
		t.Fatal("lease not shared")
	}
	w := pluginConsoleRequest(h, s, "/api/local/plugins/connect", `{"port":41235,"token":"synthetic-new-desktop-plugin-token"}`, "127.0.0.1:12", "", true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"leaseSeconds":90`) {
		t.Fatalf("replacement %s", w.Body)
	}
	w = pluginConsoleRequest(h, s, "/api/local/plugins/disconnect", `{"token":"synthetic-plugin-console-token"}`, "127.0.0.1:12", "", true)
	if w.Code != 200 || !s.pluginHooks.Enabled() {
		t.Fatal("old desktop detached replacement")
	}
	w = pluginConsoleRequest(h, s, "/api/local/plugins/disconnect", `{"token":"synthetic-new-desktop-plugin-token"}`, "127.0.0.1:12", "", true)
	if w.Code != 200 || s.pluginHooks.Enabled() {
		t.Fatal("disconnect failed")
	}
}
func TestDesktopPluginEventsContainLifecycleMetadataOnly(t *testing.T) {
	s, _ := newTestServer(t)
	events := make(chan map[string]any, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var event map[string]any
		_ = json.Unmarshal(raw, &event)
		events <- event
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(u.Port())
	_ = s.pluginHooks.Configure(port, "synthetic-plugin-event-token")
	defer s.pluginHooks.Disconnect("synthetic-plugin-event-token")
	s.Broadcast(protocol.Event{Type: "turn", Status: "started", Tool: "codex", SessionKey: "codex:test", Text: "private-content", Output: "private-output", ApprovalID: "private-approval"})
	s.Broadcast(protocol.Event{Type: "session.updated", Session: &protocol.SessionInfo{SessionKey: "codex:test", Tool: "codex", Status: "running", Title: "Example"}})
	s.Broadcast(protocol.Event{Type: "turn", Status: "completed", Tool: "codex", SessionKey: "codex:test"})
	s.publishPluginTools([]setupItem{{ID: "codex", Name: "Codex", Installed: true, Path: "private-path", Version: "1.2"}}, false)
	want := []string{"session:start", "session:update", "session:end", "tool:detect"}
	for _, name := range want {
		select {
		case event := <-events:
			raw, _ := json.Marshal(event)
			if event["event"] != name || strings.Contains(string(raw), "private-") {
				t.Fatalf("event: %s", raw)
			}
		case <-time.After(time.Second):
			t.Fatalf("missing %s", name)
		}
	}
}
