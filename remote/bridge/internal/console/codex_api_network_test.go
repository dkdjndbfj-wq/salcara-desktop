package console

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"salcara/bridge/internal/config"
)

func configureAPINetwork(t *testing.T, s *Server, n config.CodexAPINetwork) {
	t.Helper()
	if err := s.d.Store.Update(func(c *config.Config) error { c.CodexAPI.Network = n; return nil }); err != nil {
		t.Fatal(err)
	}
	// Applying network policy advances the service authorization epoch. Seed
	// the runtime lease afterwards so these requests model an explicitly armed
	// service at the current epoch.
	seedCodexAPIRuntime(t, s)
	s.apiNetwork.mu.Lock()
	s.apiNetwork.settings = n
	s.apiNetwork.mu.Unlock()
	t.Cleanup(s.CloseAPIListeners)
}
func networkRequest(h http.Handler, path, remote, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Host = "127.0.0.1:48123"
	r.RemoteAddr = remote
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestAPIListenerLANRequiresPrivateBindingAndNamedKey(t *testing.T) {
	s, _ := newTestServer(t)
	n := config.CodexAPINetwork{BindScope: "lan", Ports: []int{48123}}
	configureAPINetwork(t, s, n)
	s.apiNetwork.binding = "fixture-binding"
	h := s.apiListenerHandler(48123, n, "fixture-binding")
	for _, tc := range []struct {
		path, remote, key string
		want              int
	}{
		{"/v1/models", "172.20.8.2:1234", runtimeRevokeClientKey, 200},
		{"/v1/models", "172.20.8.2:1234", "", 401},
		{"/v1/models", "8.8.8.8:1234", runtimeRevokeClientKey, 403},
		{"/api/local/accounts", "127.0.0.1:1234", runtimeRevokeClientKey, 404},
		{"/gateway/codex/v1/models", "127.0.0.1:1234", runtimeRevokeClientKey, 404},
	} {
		w := networkRequest(h, tc.path, tc.remote, tc.key)
		if w.Code != tc.want {
			t.Fatalf("%s %s: got %d %s", tc.path, tc.remote, w.Code, w.Body)
		}
	}
	if got := networkRequest(s.apiService, "/codex-api/v1/models", "172.20.8.2:1234", runtimeRevokeClientKey).Code; got != 403 {
		t.Fatalf("direct service forged LAN permit: %d", got)
	}
	for _, header := range []string{"Origin", "Host", "Sec-Fetch-Site"} {
		r := httptest.NewRequest("GET", "/v1/models", nil)
		r.Host = "127.0.0.1:48123"
		r.RemoteAddr = "172.20.8.2:1234"
		r.Header.Set("Authorization", "Bearer "+runtimeRevokeClientKey)
		switch header {
		case "Host":
			r.Host = "foreign.example:48123"
		case "Origin":
			r.Header.Set(header, "https://foreign.example")
		default:
			r.Header.Set(header, "cross-site")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("%s accepted: %d", header, w.Code)
		}
	}
}
func TestAPIListenerLoopbackAndRetiredBinding(t *testing.T) {
	s, _ := newTestServer(t)
	n := config.CodexAPINetwork{Ports: []int{48123}}
	configureAPINetwork(t, s, n)
	s.apiNetwork.binding = "original"
	h := s.apiListenerHandler(48123, n, "original")
	if w := networkRequest(h, "/v1/models", "172.20.8.2:1234", runtimeRevokeClientKey); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if w := networkRequest(h, "/v1/models", "127.0.0.1:1234", runtimeRevokeClientKey); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	s.apiNetwork.binding = "replacement"
	if w := networkRequest(h, "/v1/models", "127.0.0.1:1234", runtimeRevokeClientKey); w.Code != 503 {
		t.Fatalf("retired handler revived: %d", w.Code)
	}
}
func freeAPIPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}
func TestAPINetworkPortConflictStopsServiceWithoutPartialListener(t *testing.T) {
	s, h := newTestServer(t)
	seedCodexAPIRuntime(t, s)
	t.Cleanup(s.CloseAPIListeners)
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	blockedPort := occupied.Addr().(*net.TCPAddr).Port
	other := freeAPIPort(t)
	w := localRequest(s, h, "POST", "/api/local/codex-api/network", map[string]any{"ports": []int{other, blockedPort}, "revision": apiNetworkRevision(s.d.Store.Get().CodexAPI.AuthEpoch)})
	if w.Code != 503 || s.d.Store.Get().CodexAPIActive() {
		t.Fatalf("conflict remained active: %d %s", w.Code, w.Body)
	}
	if len(s.apiNetwork.entries) != 0 {
		t.Fatal("partial listener published")
	}
	probe, err := net.Listen("tcp4", occupied.Addr().String())
	if err == nil {
		probe.Close()
		t.Fatal("occupied port was closed")
	}
	ln, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(other)))
	if err != nil {
		t.Fatal("partial prepared listener leaked")
	}
	ln.Close()
}
func TestAPINetworkSaveFailureAndStaleRevision(t *testing.T) {
	s, h := newTestServer(t)
	seedCodexAPIRuntime(t, s)
	t.Cleanup(s.CloseAPIListeners)
	before := s.d.Store.Get()
	w := localRequest(s, h, "POST", "/api/local/codex-api/network", map[string]any{"ports": []int{}, "revision": "stale"})
	if w.Code != 409 || !s.d.Store.Get().CodexAPIActive() {
		t.Fatalf("stale edit changed runtime: %d", w.Code)
	}
	blockCodexAPIConfigSaves(t, s)
	w = localRequest(s, h, "POST", "/api/local/codex-api/network", map[string]any{"ports": []int{}, "revision": apiNetworkRevision(before.CodexAPI.AuthEpoch)})
	if w.Code != 500 || s.d.Store.Get().CodexAPIActive() {
		t.Fatalf("failed save remained active: %d %s", w.Code, w.Body)
	}
}
