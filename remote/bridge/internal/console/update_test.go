package console

import (
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/hubclient"
	"salcara/bridge/internal/protocol"
)

func TestUpdatePrepareAuthIdentityPendingAndGracefulQuit(t *testing.T) {
	s, h := newTestServer(t)
	m := agents.NewManagerWithOptions(nil, func() agents.Settings { return agents.Settings{} }, agents.Options{StateDir: t.TempDir()})
	defer m.Close()
	s.d.Hub = hubclient.New(hubclient.Options{Store: s.d.Store})
	s.d.Hub.SetManager(m)
	quit := make(chan struct{}, 1)
	s.d.Quit = func() { quit <- struct{}{} }
	body := `{"pid":` + strconv.Itoa(os.Getpid()) + `,"version":"test"}`
	host := "127.0.0.1:47831"
	hdr := map[string]string{"Cookie": CookieName + "=" + s.Token(), "Content-Type": "application/json", "Origin": "http://" + host}
	if w := do(h, "POST", "/api/update/prepare", host, nil, body); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", w.Code)
	}
	if w := do(h, "POST", "/api/update/prepare", host, hdr, `{"pid":1,"version":"test"}`); w.Code != http.StatusConflict {
		t.Fatalf("wrong owner: %d", w.Code)
	}
	s.Broadcast(protocol.Event{Type: "approval.request", ApprovalID: "fixture"})
	if w := do(h, "POST", "/api/update/prepare", host, hdr, body); w.Code != http.StatusConflict {
		t.Fatalf("pending: %d", w.Code)
	}
	s.Broadcast(protocol.Event{Type: "approval.resolved", ApprovalID: "fixture"})
	if w := do(h, "POST", "/api/update/prepare", host, hdr, body); w.Code != http.StatusOK {
		t.Fatalf("idle: %d %s", w.Code, w.Body.String())
	}
	select {
	case <-quit:
		t.Fatal("prepare must not quit")
	default:
	}
	if w := do(h, "POST", "/api/update/cancel", host, hdr, body); w.Code != http.StatusOK {
		t.Fatalf("cancel: %d", w.Code)
	}
	if w := do(h, "POST", "/api/update/commit", host, hdr, body); w.Code != http.StatusConflict {
		t.Fatalf("cancelled commit: %d", w.Code)
	}
	if w := do(h, "POST", "/api/update/prepare", host, hdr, body); w.Code != http.StatusOK {
		t.Fatalf("reprepare: %d", w.Code)
	}
	if w := do(h, "POST", "/api/update/commit", host, hdr, body); w.Code != http.StatusOK {
		t.Fatalf("commit: %d", w.Code)
	}
	select {
	case <-quit:
	case <-time.After(time.Second):
		t.Fatal("core did not receive graceful quit")
	}
}
