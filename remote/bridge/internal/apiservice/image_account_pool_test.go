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
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/gateway"
	"salcara/bridge/internal/usageledger"
)

func TestExplicitImageAccountsChooseLeastJobsAndRecordActualMember(t *testing.T) {
	s, live := imageService(t)
	live.change(func(c *config.Config) {
		c.CodexAPI.Clients[0].MaxConcurrency = 4
		c.CodexAPI.Clients[0].ImageAccountIDs = []string{"account-A", "account-B"}
	})
	enteredA, releaseA := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var observedMu sync.Mutex
	var observed []usageledger.Observation
	s.Observe = func(o usageledger.Observation) {
		observedMu.Lock()
		observed = append(observed, o)
		observedMu.Unlock()
	}
	s.OfficialImages = func(_ context.Context, a config.LocalAccount, _ []byte, _ bool, _ string, _ func() bool) (*http.Response, error) {
		if a.ID == "account-A" {
			once.Do(func() { close(enteredA) })
			<-releaseA
		}
		return successfulResponse(io.NopCloser(strings.NewReader(syntheticImageResult)), false), nil
	}
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		first <- callImage(s, syntheticClientKey, "v1/images/generations", `{"model":"gpt-image-2","prompt":"first"}`, "application/json")
	}()
	<-enteredA
	second := callImage(s, syntheticClientKey, "v1/images/generations", `{"model":"gpt-image-2","prompt":"second"}`, "application/json")
	if second.Code != http.StatusOK {
		t.Fatalf("idle image member was not selected: %d %s", second.Code, second.Body.String())
	}
	close(releaseA)
	if got := <-first; got.Code != http.StatusOK {
		t.Fatalf("first image failed: %d", got.Code)
	}
	observedMu.Lock()
	defer observedMu.Unlock()
	seen := map[string]bool{}
	for _, o := range observed {
		seen[o.AccountID] = true
	}
	if !seen["account-A"] || !seen["account-B"] {
		t.Fatalf("usage did not record selected image members: %+v", observed)
	}
}

func TestResponsesImageToolUsesExplicitLeastJobsMemberWhileTextStaysPinned(t *testing.T) {
	s, live := imageService(t)
	live.change(func(c *config.Config) {
		c.CodexAPI.Clients[0].MaxConcurrency = 4
		c.CodexAPI.Clients[0].ImageAccountIDs = []string{"account-A", "account-B"}
	})
	enteredA, releaseA := make(chan struct{}), make(chan struct{})
	s.OfficialImages = func(_ context.Context, a config.LocalAccount, _ []byte, _ bool, _ string, _ func() bool) (*http.Response, error) {
		if a.ID != "account-A" {
			t.Errorf("stable tie did not choose first explicit member: %s", a.ID)
		}
		close(enteredA)
		<-releaseA
		return successfulResponse(io.NopCloser(strings.NewReader(syntheticImageResult)), false), nil
	}
	selected := make(chan string, 2)
	s.Official = func(_ context.Context, a config.LocalAccount, _ []byte, _ bool, _ bool, _ func() bool) (*http.Response, error) {
		selected <- a.ID
		return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
	}
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		first <- callImage(s, syntheticClientKey, "v1/images/generations", `{"model":"gpt-image-2","prompt":"hold"}`, "application/json")
	}()
	<-enteredA
	tool := callAPI(s, syntheticClientKey, `{"model":"gpt-fixture","input":"fixture","tools":[{"type":"image_generation","model":"gpt-image-2"}]}`)
	if tool.Code != http.StatusOK || <-selected != "account-B" {
		t.Fatalf("Responses image tool did not share least-jobs pool: %d %s", tool.Code, tool.Body.String())
	}
	plain := callAPI(s, syntheticClientKey, `{"model":"gpt-fixture","input":"fixture"}`)
	if plain.Code != http.StatusOK || <-selected != "account-A" {
		t.Fatalf("ordinary Responses silently inherited image pool: %d", plain.Code)
	}
	close(releaseA)
	if got := <-first; got.Code != http.StatusOK {
		t.Fatal(got.Code)
	}
}

func TestImageAccountSelectionSkipsUnknownQuotaReserve(t *testing.T) {
	s, live := imageService(t)
	live.change(func(c *config.Config) {
		c.CodexAPI.Clients[0].ImageAccountIDs = []string{"account-A", "account-B"}
		c.LocalAccounts[0].QuotaReservePercent = 10
		c.LocalAccounts[1].QuotaReservePercent = 10
	})
	s.Pool = gateway.NewAccountPool()
	s.Pool.SetRoutingProbe(func(ref string, _ uint64, model string) gateway.PoolRoutingObservation {
		if ref == "credential-B" && model == "gpt-image-2" {
			return gateway.PoolRoutingObservation{Current: true, QuotaFresh: true, QuotaAllowed: true, RemainingPercent: 50}
		}
		return gateway.PoolRoutingObservation{}
	})
	selected := ""
	s.OfficialImages = func(_ context.Context, a config.LocalAccount, _ []byte, _ bool, _ string, _ func() bool) (*http.Response, error) {
		selected = a.ID
		return successfulResponse(io.NopCloser(strings.NewReader(syntheticImageResult)), false), nil
	}
	w := callImage(s, syntheticClientKey, "v1/images/generations", `{"model":"gpt-image-2","prompt":"fixture"}`, "application/json")
	if w.Code != http.StatusOK || selected != "account-B" {
		t.Fatalf("unknown quota member was executed: status=%d account=%q", w.Code, selected)
	}
}

func TestImageAccountCredentialAliasesDoNotMultiplySlots(t *testing.T) {
	s, live := imageService(t)
	live.change(func(c *config.Config) {
		c.CodexAPI.Clients[0].MaxConcurrency = 4
		c.CodexAPI.Clients[0].ImageAccountIDs = []string{"account-A", "account-B"}
		c.LocalAccounts[1].CredentialRef = c.LocalAccounts[0].CredentialRef
		c.LocalAccounts[1].CredentialEpoch = c.LocalAccounts[0].CredentialEpoch
	})
	entered, unblock := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s.OfficialImages = func(_ context.Context, _ config.LocalAccount, _ []byte, _ bool, _ string, _ func() bool) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-unblock
		}
		return successfulResponse(io.NopCloser(strings.NewReader(syntheticImageResult)), false), nil
	}
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		first <- callImage(s, syntheticClientKey, "v1/images/generations", `{"model":"gpt-image-2","prompt":"hold"}`, "application/json")
	}()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	r := apiRequest(ctx, syntheticClientKey, strings.NewReader(`{"model":"gpt-image-2","prompt":"queued"}`))
	r.URL.Path = "/codex-api/v1/images/generations"
	r.Header.Set("Content-Type", "application/json")
	second := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); s.ServeHTTP(second, r) }()
	waitImageQueue(t, &s.images, 1, 1)
	if calls.Load() != 1 {
		t.Fatal("credential alias multiplied the upstream image slot")
	}
	cancel()
	<-done
	if second.Code != http.StatusRequestTimeout || calls.Load() != 1 {
		t.Fatalf("cancelled alias waiter dispatched: status=%d calls=%d", second.Code, calls.Load())
	}
	close(unblock)
	if got := <-first; got.Code != http.StatusOK {
		t.Fatal(got.Code)
	}
}

func TestResponsesWebSocketImageTurnUsesSameExplicitPoolAndLedgerMember(t *testing.T) {
	s, live := imageService(t)
	live.change(func(c *config.Config) {
		c.CodexAPI.Clients[0].MaxConcurrency = 4
		c.CodexAPI.Clients[0].ImageAccountIDs = []string{"account-A", "account-B"}
	})
	enteredA, releaseA := make(chan struct{}), make(chan struct{})
	s.OfficialImages = func(_ context.Context, a config.LocalAccount, _ []byte, _ bool, _ string, _ func() bool) (*http.Response, error) {
		if a.ID != "account-A" {
			t.Errorf("first image did not use stable first member: %s", a.ID)
		}
		close(enteredA)
		<-releaseA
		return successfulResponse(io.NopCloser(strings.NewReader(syntheticImageResult)), false), nil
	}
	selected := make(chan string, 1)
	s.Official = func(_ context.Context, a config.LocalAccount, _ []byte, _ bool, _ bool, current func() bool) (*http.Response, error) {
		if !current() {
			t.Error("selected socket member was not current")
		}
		selected <- a.ID
		return successfulResponse(io.NopCloser(strings.NewReader(namedCompleted("image-ws-response"))), true), nil
	}
	observed := make(chan usageledger.Observation, 4)
	s.Observe = func(o usageledger.Observation) { observed <- o }
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		first <- callImage(s, syntheticClientKey, "v1/images/generations", `{"model":"gpt-image-2","prompt":"hold"}`, "application/json")
	}()
	<-enteredA
	c := namedSocket(t, namedSocketServer(t, s), syntheticClientKey)
	sendNamedFrame(t, c, map[string]any{"type": "response.create", "model": "gpt-fixture", "input": "fixture", "tools": []any{map[string]any{"type": "image_generation", "model": "gpt-image-2"}}})
	namedSocketEvent(t, c, "response.completed")
	if got := <-selected; got != "account-B" {
		t.Fatalf("WebSocket image turn did not use least-jobs member: %q", got)
	}
	select {
	case o := <-observed:
		if o.AccountID != "account-B" || o.ClientID != "client-A" {
			t.Fatalf("WebSocket ledger lost actual image member: %+v", o)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("WebSocket image turn was not observed")
	}
	c.Close()
	close(releaseA)
	if got := <-first; got.Code != http.StatusOK {
		t.Fatal(got.Code)
	}
}

func TestAPIServiceRemoteCallbackCannotBypassOriginOrNamedAuthorization(t *testing.T) {
	s, _ := fixtureService(t)
	s.Official = func(_ context.Context, _ config.LocalAccount, _ []byte, _ bool, _ bool, _ func() bool) (*http.Response, error) {
		return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
	}
	allowed := false
	calls := 0
	s.AllowRemote = func(*http.Request, config.Config) bool { calls++; return allowed }
	request := func(key string) *http.Request {
		r := apiRequest(context.Background(), key, strings.NewReader(`{"model":"gpt-fixture","input":"fixture"}`))
		r.RemoteAddr = "192.168.50.25:44000"
		return r
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, request(syntheticClientKey))
	if w.Code != http.StatusForbidden || calls != 1 {
		t.Fatalf("remote callback denial ignored: %d calls=%d", w.Code, calls)
	}
	allowed = true
	w = httptest.NewRecorder()
	s.ServeHTTP(w, request(syntheticClientKey))
	if w.Code != http.StatusOK {
		t.Fatalf("explicit API-only remote listener was rejected downstream: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, request("wrong-key"))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("remote callback bypassed named key: %d", w.Code)
	}
	r := request(syntheticClientKey)
	r.Header.Set("Origin", "https://attacker.invalid")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("remote callback bypassed Origin rejection: %d", w.Code)
	}
}
