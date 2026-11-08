package apiservice

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func closeAdmissionModels(s *Service, route, key string) *httptest.ResponseRecorder {
	r := apiRequest(context.Background(), key, nil)
	r.Method, r.URL.Path = http.MethodGet, route
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestCloseAdmissionRejectsManualModelsAndLateRuntimeRearm(t *testing.T) {
	s, live, profileKey := profileFixtureService(t)
	var catalogs, dispatches atomic.Int32
	s.ModelCatalog = func(context.Context, config.LocalAccount, bool) (protocol.ModelCatalog, error) {
		catalogs.Add(1)
		return protocol.ModelCatalog{Models: []string{"gpt-fixture"}}, nil
	}
	s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
		dispatches.Add(1)
		return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
	}
	if got := closeAdmissionModels(s, "/codex-api/v1/models", syntheticClientKey); got.Code != http.StatusOK {
		t.Fatalf("fixture discovery failed before closure: %d", got.Code)
	}
	s.CloseAdmission()
	s.CloseAdmission()
	live.change(func(c *config.Config) { c.CodexAPI.Enabled = false; c.CodexAPI.RuntimeLease = "" })
	live.change(func(c *config.Config) {
		// Deliberately revive even the old snapshot: configuration epochs must
		// not be the only reason a closed service refuses these credentials.
		c.CodexAPI.Enabled, c.CodexAPI.RuntimeLease = true, c.GatewayRuntimeKey
	})
	if !live.get().CodexAPIActive() || live.get().CodexServiceProfileCredential() != profileKey {
		t.Fatal("fixture did not republish an otherwise valid runtime")
	}
	for _, request := range []struct{ route, key string }{
		{"/codex-api/v1/models", syntheticClientKey},
		{"/backend-api/codex/models", syntheticClientKey},
		{"/codex-api/profile/v1/models", profileKey},
	} {
		if got := closeAdmissionModels(s, request.route, request.key); got.Code != http.StatusServiceUnavailable {
			t.Errorf("closed discovery route %s returned %d", request.route, got.Code)
		}
	}
	if got := callAPI(s, syntheticClientKey, `{"model":"friendly","input":"synthetic"}`); got.Code != http.StatusServiceUnavailable {
		t.Fatalf("late active state reopened dispatch: %d", got.Code)
	}
	if catalogs.Load() != 1 || dispatches.Load() != 0 {
		t.Fatal("closed service invoked discovery or an upstream executor")
	}
}

func TestCloseAdmissionFencesRequestWaitingForInitialSnapshot(t *testing.T) {
	s, live := fixtureService(t)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var snapshot atomic.Bool
	var catalogs atomic.Int32
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	s.Config = func() config.Config {
		if snapshot.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		return live.get()
	}
	s.ModelCatalog = func(context.Context, config.LocalAccount, bool) (protocol.ModelCatalog, error) {
		catalogs.Add(1)
		return protocol.ModelCatalog{Models: []string{"gpt-fixture"}}, nil
	}
	var response *httptest.ResponseRecorder
	go func() {
		defer close(done)
		response = closeAdmissionModels(s, "/codex-api/v1/models", syntheticClientKey)
	}()
	defer func() { unblock(); awaitDone(t, done) }()
	awaitDone(t, entered)
	s.CloseAdmission()
	unblock()
	awaitDone(t, done)
	if response.Code != http.StatusServiceUnavailable || catalogs.Load() != 0 {
		t.Fatalf("request passed closure while obtaining its first snapshot: %d", response.Code)
	}
}

func TestCloseAdmissionFencesQueuedBodyDespiteUnchangedRuntime(t *testing.T) {
	s, live := fixtureService(t)
	captured := live.get()
	var dispatches atomic.Int32
	s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
		dispatches.Add(1)
		return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
	}
	body := &blockedBody{entered: make(chan struct{}), release: make(chan struct{}), reader: strings.NewReader(`{"model":"friendly","input":"synthetic"}`)}
	response := httptest.NewRecorder()
	done := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(body.release) }) }
	go func() {
		defer close(done)
		s.ServeHTTP(response, apiRequest(context.Background(), syntheticClientKey, body))
	}()
	defer func() { unblock(); awaitDone(t, done) }()
	awaitDone(t, body.entered)
	s.CloseAdmission()
	live.change(func(c *config.Config) { c.CodexAPI.Enabled = false })
	live.change(func(c *config.Config) { *c = captured.Clone() })
	unblock()
	awaitDone(t, done)
	if response.Code < 400 || dispatches.Load() != 0 {
		t.Fatalf("closed queued body reached upstream: status=%d calls=%d", response.Code, dispatches.Load())
	}
}

func TestCloseAdmissionWithholdsCatalogCompletedAfterClosure(t *testing.T) {
	s, _ := fixtureService(t)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	s.ModelCatalog = func(context.Context, config.LocalAccount, bool) (protocol.ModelCatalog, error) {
		close(entered)
		<-release
		return protocol.ModelCatalog{Models: []string{"gpt-fixture"}}, nil
	}
	var response *httptest.ResponseRecorder
	go func() {
		defer close(done)
		response = closeAdmissionModels(s, "/codex-api/v1/models", syntheticClientKey)
	}()
	defer func() { unblock(); awaitDone(t, done) }()
	awaitDone(t, entered)
	s.CloseAdmission()
	unblock()
	awaitDone(t, done)
	if response.Code != http.StatusConflict || strings.Contains(response.Body.String(), "gpt-fixture") {
		t.Fatalf("closed service published a late catalog: %d", response.Code)
	}
}

func TestCloseAdmissionCannotBeReopenedByManagedLaunch(t *testing.T) {
	s, _, key := profileFixtureService(t)
	s.ModelCatalog = func(context.Context, config.LocalAccount, bool) (protocol.ModelCatalog, error) {
		return protocol.ModelCatalog{Models: []string{"gpt-fixture"}}, nil
	}
	release, err := s.BeginManagedLaunch()
	if err != nil {
		t.Fatal(err)
	}
	s.CloseAdmission()
	s.AdmitManagedLaunch(key)
	if got := closeAdmissionModels(s, "/codex-api/profile/v1/models", key); got.Code != http.StatusServiceUnavailable {
		t.Fatal("managed admission reopened a closed service")
	}
	release()
	release()
	if nextRelease, err := s.BeginManagedLaunch(); err == nil {
		nextRelease()
		t.Fatal("closed service admitted a new managed launch")
	}
	if got := closeAdmissionModels(s, "/codex-api/v1/models", syntheticClientKey); got.Code != http.StatusServiceUnavailable {
		t.Fatal("managed launch cleanup reset permanent closure")
	}
}
