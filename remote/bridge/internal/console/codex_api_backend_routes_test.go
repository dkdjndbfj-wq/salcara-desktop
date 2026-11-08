package console

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"salcara/bridge/internal/config"
)

func TestBackendAPIMountRequiresCurrentNamedServiceAndLocalHost(t *testing.T) {
	s, h := newTestServer(t)
	seedCodexAPIRuntime(t, s)
	request := func(path, key, origin, host string) int {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Host, r.RemoteAddr = host, "127.0.0.1:49123"
		r.Header.Set("Authorization", "Bearer "+key)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for _, path := range []string{"/backend-api/codex/models", "/codex-api/backend-api/codex/models"} {
		if status := request(path, runtimeRevokeClientKey, "", "127.0.0.1:47831"); status != 200 {
			t.Fatalf("backend alias mount: %d", status)
		}
		if status := request(path, "synthetic-invalid-key", "", "127.0.0.1:47831"); status != 401 {
			t.Fatalf("backend alias bypassed named key: %d", status)
		}
		if status := request(path, runtimeRevokeClientKey, "https://foreign.example", "127.0.0.1:47831"); status != 403 {
			t.Fatalf("backend alias accepted browser origin: %d", status)
		}
		if status := request(path, runtimeRevokeClientKey, "", "foreign.example:47831"); status != 403 {
			t.Fatalf("backend alias bypassed host guard: %d", status)
		}
	}
	if err := s.d.Store.Update(func(c *config.Config) error { c.CodexAPI.Enabled = false; return nil }); err != nil {
		t.Fatal(err)
	}
	if status := request("/backend-api/codex/models", runtimeRevokeClientKey, "", "127.0.0.1:47831"); status != 503 {
		t.Fatalf("backend alias bypassed stopped service: %d", status)
	}
}
