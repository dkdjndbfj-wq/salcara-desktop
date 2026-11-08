package apiservice

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

const serviceProfileRoute = "/codex-api/profile/v1/responses"

func profileFixtureService(t *testing.T) (*Service, *liveConfig, string) {
	t.Helper()
	s, live := fixtureService(t)
	live.change(func(c *config.Config) {
		c.GatewayRuntimeKey = "synthetic-service-profile-runtime"
		c.CodexAPI.RuntimeLease, c.CodexAPI.AuthEpoch = c.GatewayRuntimeKey, "synthetic-service-epoch"
		c.CodexAPI.Clients[0].AuthEpoch = "synthetic-client-epoch"
		client := c.CodexAPI.Clients[0]
		c.CodexServiceProfile = config.ServiceProfileBinding{ClientID: client.ID, ClientEpoch: client.AuthEpoch,
			ServiceEpoch: c.CodexAPI.AuthEpoch, Lease: "profile-lease", Generation: "disk-generation", Model: "friendly"}
	})
	return s, live, live.get().CodexServiceProfileCredential()
}

func profileRequest(s *Service, route, key, remote string) *httptest.ResponseRecorder {
	r := apiRequest(context.Background(), key, strings.NewReader(`{"model":"friendly","input":"synthetic"}`))
	r.URL.Path = route
	if remote != "" {
		r.RemoteAddr = remote
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestCodexServiceProfileRouteAndCredentialScope(t *testing.T) {
	s, live, key := profileFixtureService(t)
	var calls atomic.Int32
	s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
		calls.Add(1)
		return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
	}
	if key == "" || profileRequest(s, serviceProfileRoute, key, "").Code != 200 {
		t.Fatal("committed profile could not use Responses")
	}
	for _, tc := range []struct {
		route, credential string
		status            int
	}{
		{"/codex-api/v1/responses", key, 401},
		{"/codex-api/backend-api/codex/responses", key, 401},
		{serviceProfileRoute, syntheticClientKey, 401},
		{"/codex-api/managed/codex/v1/responses", key, 401},
		{"/codex-api/managed/claude/v1/messages", key, 401},
		{"/codex-api/managed/codex-profile/v1/responses", key, 404},
		{"/codex-api/profile/v1/messages", key, 404},
		{"/codex-api/profile/v1/messages/count_tokens", key, 404},
		{"/codex-api/profile/v1/images/generations", key, 404},
		{"/codex-api/profile/v1/chat/completions", key, 404},
		{"/codex-api/profile/backend-api/codex/responses", key, 404},
	} {
		if got := profileRequest(s, tc.route, tc.credential, "").Code; got != tc.status {
			t.Errorf("route %s: got %d, want %d", tc.route, got, tc.status)
		}
	}
	s.AllowRemote = func(*http.Request, config.Config) bool { return true }
	if got := profileRequest(s, serviceProfileRoute, key, "192.168.1.20:15000").Code; got != 403 {
		t.Errorf("remote listener admitted a profile key: %d", got)
	}
	if calls.Load() != 1 {
		t.Fatal("rejected profile route reached provider")
	}
	live.change(func(c *config.Config) { c.CodexServiceProfile = config.ServiceProfileBinding{} })
	if profileRequest(s, serviceProfileRoute, key, "").Code != 401 {
		t.Fatal("revoked profile credential remained live")
	}
	if profileRequest(s, "/codex-api/v1/responses", syntheticClientKey, "").Code != 200 {
		t.Fatal("profile revocation changed manual API key")
	}
}

func TestCodexServiceProfileProspectiveKeyAndApplyAdmission(t *testing.T) {
	s, live, key := profileFixtureService(t)
	armed := live.get().CodexServiceProfile
	live.change(func(c *config.Config) { c.CodexServiceProfile = config.ServiceProfileBinding{} })
	if profileRequest(s, serviceProfileRoute, key, "").Code != 401 {
		t.Fatal("uncommitted prospective key authenticated")
	}
	live.change(func(c *config.Config) { c.CodexServiceProfile = armed })
	release, err := s.BeginManagedLaunch()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if profileRequest(s, serviceProfileRoute, key, "").Code != 503 || profileRequest(s, "/codex-api/v1/responses", syntheticClientKey, "").Code != 503 {
		t.Fatal("request entered while global files were changing")
	}
	s.ModelCatalog = func(context.Context, config.LocalAccount, bool) (protocol.ModelCatalog, error) {
		return protocol.ModelCatalog{Models: []string{"gpt-fixture"}}, nil
	}
	release()
	r := apiRequest(context.Background(), key, nil)
	r.Method, r.URL.Path = http.MethodGet, "/codex-api/profile/v1/models"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "friendly") {
		t.Fatalf("committed profile discovery failed: %d", w.Code)
	}
}

func TestCodexServiceProfileRevocationDuringBodyReadStopsDispatch(t *testing.T) {
	for _, variant := range []string{"revoke", "replace", "client", "service", "stop", "runtime"} {
		t.Run(variant, func(t *testing.T) {
			s, live, key := profileFixtureService(t)
			var calls atomic.Int32
			s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
				calls.Add(1)
				return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
			}
			body := &blockedBody{entered: make(chan struct{}), release: make(chan struct{}), reader: strings.NewReader(`{"model":"friendly","input":"synthetic"}`)}
			r := apiRequest(context.Background(), key, body)
			r.URL.Path = serviceProfileRoute
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { defer close(done); s.ServeHTTP(w, r) }()
			awaitDone(t, body.entered)
			live.change(func(c *config.Config) {
				switch variant {
				case "revoke":
					c.CodexServiceProfile = config.ServiceProfileBinding{}
				case "replace":
					c.CodexServiceProfile.Lease, c.CodexServiceProfile.Generation = "next-lease", "next-generation"
				case "client":
					c.CodexAPI.Clients[0].AuthEpoch = "next-client-epoch"
				case "service":
					c.CodexAPI.AuthEpoch = "next-service-epoch"
				case "stop":
					c.CodexAPI.Enabled = false
				case "runtime":
					c.GatewayRuntimeKey = "next-runtime-key"
				}
			})
			close(body.release)
			awaitDone(t, done)
			if calls.Load() != 0 || w.Code < 400 || w.Code >= 500 {
				t.Fatalf("stale profile dispatched: calls=%d status=%d", calls.Load(), w.Code)
			}
		})
	}
}

func TestCodexServiceProfileRevocationDuringCatalogReadWithholdsDiscovery(t *testing.T) {
	s, live, key := profileFixtureService(t)
	entered, release := make(chan struct{}), make(chan struct{})
	s.ModelCatalog = func(context.Context, config.LocalAccount, bool) (protocol.ModelCatalog, error) {
		close(entered)
		<-release
		return protocol.ModelCatalog{Models: []string{"gpt-fixture"}}, nil
	}
	r := apiRequest(context.Background(), key, nil)
	r.Method, r.URL.Path = http.MethodGet, "/codex-api/profile/v1/models"
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); s.ServeHTTP(w, r) }()
	awaitDone(t, entered)
	live.change(func(c *config.Config) { c.CodexServiceProfile = config.ServiceProfileBinding{} })
	close(release)
	awaitDone(t, done)
	if w.Code != 409 || strings.Contains(w.Body.String(), "gpt-fixture") {
		t.Fatal("revoked profile received a completed model catalog")
	}
}
