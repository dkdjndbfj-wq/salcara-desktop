package console

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"salcara/bridge/internal/desktopcompanion"
	"salcara/bridge/internal/launcher"
)

type blockedNativeControl struct {
	fakeCompanionControl
	release chan struct{}
}

func (f *blockedNativeControl) NativeStatus(ctx context.Context) (desktopcompanion.NativeConnection, error) {
	select {
	case <-f.release:
		return f.connection, nil
	case <-ctx.Done():
		return desktopcompanion.NativeConnection{}, ctx.Err()
	}
}

func TestLocalPagesReturnBeforeToolAndNativeDiscoveryCompletes(t *testing.T) {
	s, h := newTestServer(t)
	release := make(chan struct{})
	f := &blockedNativeControl{release: release}
	f.connection = desktopcompanion.NativeConnection{Active: true, ExpiresAt: time.Now().Add(time.Minute).UnixMilli(), SessionKeys: []string{"codex:11111111-2222-4333-8444-555555555555"}}
	s.d.Companion = f
	s.d.Local.FindTools = func(ctx context.Context, _ map[string]string) []launcher.Tool {
		select {
		case <-release:
			return []launcher.Tool{{ID: "codex", Kind: "codex"}}
		case <-ctx.Done():
			return nil
		}
	}
	defer func() {
		close(release)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		s.d.Local.Inventory(ctx, nil)
		s.nativeConnection(ctx)
	}()
	for _, path := range []string{"/api/local/accounts", "/api/setup", "/api/state"} {
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- localRequest(s, h, "GET", path, nil) }()
		select {
		case response := <-done:
			var value map[string]any
			if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &value) != nil || path != "/api/state" && value["discoveryPending"] != true {
				t.Fatal("local rendering is still gated by discovery")
			}
		case <-time.After(time.Second):
			t.Fatal("local page waited for a blocked probe")
		}
	}
}
