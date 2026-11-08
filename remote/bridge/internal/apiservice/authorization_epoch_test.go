package apiservice

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

// Exercise the actual configuration commit path, including revocation followed
// by an identical restoration. A snapshot comparison alone misses this case.
func storedService(t *testing.T) (*Service, *config.Store) {
	t.Helper()
	store, err := config.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, live := fixtureService(t)
	fixture := live.get()
	store.SetRuntimeGatewayKey("synthetic-runtime-lease")
	updateAPIConfig(t, store, func(c *config.Config) {
		c.CodexAPI = fixture.CodexAPI
		c.LocalAccounts = fixture.LocalAccounts
		c.CodexAPI.RuntimeLease = c.GatewayRuntimeKey
	})
	return New(store.Get), store
}

func updateAPIConfig(t *testing.T, store *config.Store, fn func(*config.Config)) {
	t.Helper()
	if err := store.Update(func(c *config.Config) error { fn(c); return nil }); err != nil {
		t.Fatal(err)
	}
}

func restoreAPIAuthorization(t *testing.T, store *config.Store, change string) {
	t.Helper()
	original := store.Get()
	if change == "runtime" {
		store.SetRuntimeGatewayKey("synthetic-next-runtime")
		updateAPIConfig(t, store, func(c *config.Config) { c.CodexAPI.RuntimeLease = c.GatewayRuntimeKey })
		return
	}
	updateAPIConfig(t, store, func(c *config.Config) {
		switch change {
		case "service":
			c.CodexAPI.Enabled, c.CodexAPI.RuntimeLease = false, ""
		case "client":
			c.CodexAPI.Clients[0].Disabled = true
		case "policy":
			c.CodexAPI.Clients[0].ModelDenylist = []string{"gpt-fixture"}
		case "prefix":
			c.CodexAPI.Clients[0].ModelDenyPrefixes = []string{"gpt-"}
		case "alias":
			c.CodexAPI.Clients[0].ModelAliases["friendly"] = "different-fixture"
		case "key":
			c.CodexAPI.Clients[0].KeyDigest = config.APIClientDigest("synthetic-rotated-key")
		case "remove":
			c.CodexAPI.Clients = c.CodexAPI.Clients[1:]
		case "account-key":
			c.LocalAccounts[0].Key = "synthetic-changed-account-key"
		case "account-policy":
			c.LocalAccounts[0].ModelDenylist = []string{"gpt-fixture"}
		case "account-remove":
			c.LocalAccounts = c.LocalAccounts[1:]
		}
	})
	updateAPIConfig(t, store, func(c *config.Config) {
		c.CodexAPI = original.CodexAPI
		c.LocalAccounts = original.LocalAccounts
	})
}

func TestRestoredAuthorizationCannotRevivePendingAPIRequests(t *testing.T) {
	for _, phase := range []string{"body", "catalog", "bootstrap"} {
		for _, change := range []string{"service", "client", "policy", "prefix", "alias", "key", "remove", "runtime", "account-key", "account-policy", "account-remove"} {
			t.Run(phase+"/"+change, func(t *testing.T) {
				s, store := storedService(t)
				var calls atomic.Int32
				var prepared atomic.Bool
				entered, release := make(chan struct{}), make(chan struct{})
				s.Official = func(_ context.Context, _ config.LocalAccount, _ []byte, _, _ bool, current func() bool) (*http.Response, error) {
					if phase == "bootstrap" && !prepared.Swap(true) {
						close(entered)
						<-release
					}
					if !current() {
						return nil, errors.New("fixture authorization changed")
					}
					calls.Add(1)
					return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
				}
				s.ModelCatalog = func(context.Context, config.LocalAccount, bool) (protocol.ModelCatalog, error) {
					close(entered)
					<-release
					return protocol.ModelCatalog{Models: []string{"late-fixture-hint"}}, nil
				}
				w, done := httptest.NewRecorder(), make(chan struct{})
				go func() {
					defer close(done)
					r := apiRequest(context.Background(), syntheticClientKey, &blockedBody{entered: entered, release: release, reader: strings.NewReader(`{"model":"friendly","input":"fixture"}`)})
					if phase == "catalog" {
						r.Method, r.URL.Path, r.Body = http.MethodGet, "/codex-api/v1/models", http.NoBody
					} else if phase == "bootstrap" {
						r.Body = io.NopCloser(strings.NewReader(`{"model":"friendly","input":"fixture"}`))
					}
					s.ServeHTTP(w, r)
				}()
				awaitDone(t, entered)
				restoreAPIAuthorization(t, store, change)
				close(release)
				awaitDone(t, done)
				if calls.Load() != 0 || w.Code < 400 || strings.Contains(w.Body.String(), "late-fixture-hint") {
					t.Fatalf("restoration revived stale request: upstream=%d status=%d body=%s", calls.Load(), w.Code, w.Body)
				}
				if fresh := callAPI(s, syntheticClientKey, `{"model":"friendly","input":"new fixture"}`); fresh.Code != 200 || calls.Load() != 1 {
					t.Fatalf("restored authorization must accept a fresh request: calls=%d status=%d", calls.Load(), fresh.Code)
				}
			})
		}
	}
}

func TestUnrelatedConfigurationDoesNotRevokePendingAPIRequest(t *testing.T) {
	s, store := storedService(t)
	s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
		return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
	}
	body := &blockedBody{entered: make(chan struct{}), release: make(chan struct{}), reader: strings.NewReader(`{"model":"friendly","input":"fixture"}`)}
	w, done := httptest.NewRecorder(), make(chan struct{})
	go func() { defer close(done); s.ServeHTTP(w, apiRequest(context.Background(), syntheticClientKey, body)) }()
	awaitDone(t, body.entered)
	updateAPIConfig(t, store, func(c *config.Config) {
		c.DeviceName = "Changed fixture name"
		c.CodexAPI.Clients[1].Disabled = true
		c.LocalAccounts[0].Models = []string{"gpt-fixture", "new-catalog-hint"}
		c.LocalAccounts[0].LastUsedAt++
	})
	close(body.release)
	awaitDone(t, done)
	if w.Code != 200 {
		t.Fatalf("unrelated configuration revoked an unchanged client: %d %s", w.Code, w.Body)
	}
}
