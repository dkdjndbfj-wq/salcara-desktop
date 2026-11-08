package apiservice

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/gateway"
	"salcara/bridge/internal/protocol"
)

func fixedBudgetService(t *testing.T) (*Service, *liveConfig) {
	t.Helper()
	s, live := fixtureService(t)
	s.Pool = gateway.NewAccountPool()
	live.change(func(c *config.Config) {
		c.LocalAccounts[0].MaxConcurrency = 4
		c.LocalAccounts[1].CredentialRef = c.LocalAccounts[0].CredentialRef
		c.LocalAccounts[1].MaxConcurrency = 4
		duplicate := c.LocalAccounts[0]
		duplicate.ID, duplicate.Name, duplicate.PoolGroup = "account-C", "lower shared ceiling", "other-group"
		duplicate.PoolEnabled, duplicate.MaxConcurrency = false, 1
		c.LocalAccounts = append(c.LocalAccounts, duplicate)
	})
	s.ModelCatalog = func(context.Context, config.LocalAccount, bool) (protocol.ModelCatalog, error) {
		return protocol.ModelCatalog{Models: []string{"gpt-fixture"}}, nil
	}
	return s, live
}

func TestNamedFixedCredentialBudgetReservesSocketOnceAndAllowsReadOnlyCatalog(t *testing.T) {
	s, live := fixedBudgetService(t)
	var calls atomic.Int32
	s.Official = func(_ context.Context, _ config.LocalAccount, _ []byte, stream, _ bool, _ func() bool) (*http.Response, error) {
		calls.Add(1)
		if stream {
			return successfulResponse(io.NopCloser(strings.NewReader(namedCompleted("budget-response"))), true), nil
		}
		return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
	}
	c := namedSocket(t, namedSocketServer(t, s), syntheticClientKey)
	sendNamedFrame(t, c, map[string]any{"type": "response.create", "model": "friendly", "generate": false})
	warm := namedSocketEvent(t, c, "response.completed")["response"].(map[string]any)
	sendNamedFrame(t, c, map[string]any{"type": "response.create", "previous_response_id": warm["id"], "input": "synthetic"})
	namedSocketEvent(t, c, "response.completed")
	if w := callAPI(s, syntheticKeyB, `{"model":"gpt-fixture","input":"synthetic"}`); w.Code != 429 || calls.Load() != 1 {
		t.Fatal("duplicate named client bypassed socket shared budget", w.Code, calls.Load())
	}
	r := apiRequest(context.Background(), syntheticKeyB, nil)
	r.Method, r.URL.Path = http.MethodGet, "/codex-api/v1/models"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal("model discovery consumed occupied inference slot", w.Code)
	}
	current := live.get()
	if _, err := s.Pool.AdmitFixed(current.LocalAccounts[1], current.LocalAccounts); !errors.Is(err, gateway.ErrCredentialCapacity) {
		t.Fatal("fixed socket did not share ordinary pool budget", err)
	}
	c.Close()
	waitNamedSocketReleased(t, s)
	if w := callAPI(s, syntheticKeyB, `{"model":"gpt-fixture","input":"synthetic"}`); w.Code != 200 || calls.Load() != 2 {
		t.Fatal("socket disconnect leaked fixed budget", w.Code, calls.Load())
	}
}

func TestNamedFixedCredentialBudgetIncludesAllGroupsAndProtectsWaitingUpload(t *testing.T) {
	s, live := fixedBudgetService(t)
	var calls atomic.Int32
	s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
		calls.Add(1)
		return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
	}
	body := &blockedBody{entered: make(chan struct{}), release: make(chan struct{}), reader: strings.NewReader(`{"model":"friendly","input":"synthetic"}`)}
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); s.ServeHTTP(w, apiRequest(context.Background(), syntheticClientKey, body)) }()
	awaitDone(t, body.entered)
	if other := callAPI(s, syntheticKeyB, `{"model":"gpt-fixture","input":"synthetic"}`); other.Code != 429 || calls.Load() != 0 {
		t.Fatal("slow upload failed to reserve shared credential floor", other.Code, calls.Load())
	}
	current := live.get()
	if _, _, err := s.Pool.Select("codex", current.LocalAccounts, "shared", "gpt-fixture", ""); err == nil {
		t.Fatal("pool route bypassed named slow upload")
	}
	close(body.release)
	awaitDone(t, done)
	if w.Code != 200 || calls.Load() != 1 {
		t.Fatal("fixed upload failed", w.Code, calls.Load())
	}
	if other := callAPI(s, syntheticKeyB, `{"model":"gpt-fixture","input":"synthetic"}`); other.Code != 200 || calls.Load() != 2 {
		t.Fatal("fixed HTTP leaked credential budget", other.Code, calls.Load())
	}
}
