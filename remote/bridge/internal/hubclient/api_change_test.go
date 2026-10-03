package hubclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/desktopcompanion"
	"salcara/bridge/internal/protocol"
)

type busyAPIWorker struct {
	*fakeAgent
	busy bool
}

func (a *busyAPIWorker) ActiveRemoteTurns() bool { return a.busy }

type apiDesktopStatus struct {
	*routeDesktop
	err error
}

func (d *apiDesktopStatus) NativeStatus(context.Context) (desktopcompanion.NativeConnection, error) {
	return d.st, d.err
}

func TestPhoneConversationAPIChangeGuardsPreserveOriginalThread(t *testing.T) {
	for _, family := range []string{"codex", "claude", "claude-desktop"} {
		t.Run(family, func(t *testing.T) {
			workerFamily := family
			if family == "claude-desktop" {
				workerFamily = "claude"
			}
			account := config.LocalAccount{ID: "vault", Name: "备用", Kind: "api", Key: "fake-not-real", BaseURL: "https://fixture.invalid/v1", Models: []string{"m1"}}
			c := statusFixture(t, config.Config{DeviceID: "pc", DeviceSecret: "fixture-secret", LocalAccounts: []config.LocalAccount{account}})
			worker := &busyAPIWorker{fakeAgent: &fakeAgent{id: workerFamily}, busy: true}
			c.SetManager(&fakeManager{list: []agents.Agent{worker}})
			cmd := map[string]any{"type": "agents.api.set", "agent": family, "accountId": APIHandle(c.o.Store.Get(), account.ID), "sessionKey": workerFamily + ":original"}
			before := c.o.Store.Get()
			if _, err := c.Dispatch(context.Background(), cmd); err == nil {
				t.Fatal("active task allowed API change")
			}
			if !reflect.DeepEqual(before, c.o.Store.Get()) {
				t.Fatal("rejected mutation changed config")
			}
			worker.busy = false
			if _, err := c.Dispatch(context.Background(), cmd); err != nil {
				t.Fatal(err)
			}
			if c.o.Store.Get().RemoteAPI[workerFamily].AccountID != account.ID {
				t.Fatal("idle change not applied")
			}
			if len(worker.started)+len(worker.sent)+len(worker.stopped)+len(worker.opened) != 0 {
				t.Fatal("changing API started, stopped or replaced original conversation")
			}
		})
	}
}

func TestPhoneConversationAPIDesktopLeaseGuard(t *testing.T) {
	key := "codex:01a0ae56-e9f6-4933-9ad4-5e08cd7874e5"
	for _, test := range []struct {
		name    string
		err     error
		st      desktopcompanion.NativeConnection
		allowed bool
	}{
		{name: "adapter inactive", err: desktopcompanion.ErrActivationRequired, allowed: true},
		{name: "wrapped inactive", err: errors.Join(errors.New("wrapper"), desktopcompanion.ErrActivationRequired), allowed: true},
		{name: "unknown transport error", err: errors.New("connection refused")},
		{name: "selected original native session", st: desktopcompanion.NativeConnection{Active: true, ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), SessionKeys: []string{key}}},
		{name: "other session only", st: desktopcompanion.NativeConnection{Active: true, ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), SessionKeys: []string{"codex:11111111-2222-4333-8444-555555555555"}}, allowed: true},
		{name: "expired lease", st: desktopcompanion.NativeConnection{Active: true, ExpiresAt: time.Now().Add(-time.Hour).UnixMilli(), SessionKeys: []string{key}}, allowed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := statusFixture(t, config.Config{})
			c.o.Desktop = &apiDesktopStatus{routeDesktop: &routeDesktop{st: test.st}, err: test.err}
			err := c.checkRemoteAPIChange(context.Background(), "codex", key)
			if (err == nil) != test.allowed {
				t.Fatalf("allowed=%v error=%v", test.allowed, err)
			}
		})
	}
}

func TestPhoneConversationAPIFamilyAndFallbackBusyGuard(t *testing.T) {
	c := statusFixture(t, config.Config{})
	for _, key := range []string{"claude:original", "codex:", "codex:" + string(make([]byte, 512))} {
		if err := c.checkRemoteAPIChange(context.Background(), "codex", key); err == nil {
			t.Fatalf("bad scope accepted: %q", key)
		}
	}
	if err := c.checkRemoteAPIChange(context.Background(), "unknown", ""); err == nil {
		t.Fatal("unknown family accepted")
	}
	worker := &fakeAgent{id: "codex", sessions: []protocol.SessionInfo{{SessionKey: "codex:other", Status: "waiting_approval"}}}
	c.SetManager(&fakeManager{list: []agents.Agent{worker}})
	if err := c.checkRemoteAPIChange(context.Background(), "codex", "codex:original"); err == nil {
		t.Fatal("changing a global family setting ignored another pending task")
	}
}

func TestPhoneConversationAPIChangedDuringModelCatalogReadRejectsStaleResult(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		close(entered)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"old-catalog-model"}]}`))
	}))
	defer server.Close()
	a := config.LocalAccount{ID: "old", Name: "旧 API", Kind: "api", Key: "fake", BaseURL: server.URL + "/v1", Model: "old-default"}
	b := config.LocalAccount{ID: "new", Name: "新 API", Kind: "api", Key: "fake-other", BaseURL: "https://fixture.invalid/v1", Models: []string{"new-model"}}
	c := statusFixture(t, config.Config{DeviceID: "pc", DeviceSecret: "fixture", LocalAccounts: []config.LocalAccount{a, b}, RemoteAPI: map[string]config.RemoteAPI{"codex": {AccountID: "old"}}})
	c.SetManager(&fakeManager{list: []agents.Agent{&fakeAgent{id: "codex"}}})
	done := make(chan error, 1)
	go func() {
		_, err := c.Dispatch(context.Background(), map[string]any{"type": "models.list", "tool": "codex", "refresh": true})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("catalog request never started")
	}
	_, err := c.Dispatch(context.Background(), map[string]any{"type": "agents.api.set", "agent": "codex", "accountId": APIHandle(c.o.Store.Get(), b.ID), "sessionKey": "codex:original"})
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("old API catalog escaped after selection changed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("catalog request stuck")
	}
	if c.o.Store.Get().RemoteAPI["codex"].AccountID != b.ID {
		t.Fatal("catalog completion replaced new selection")
	}
}
