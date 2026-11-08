package console

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"salcara/bridge/internal/hubserver"
)

func startLifecycleTestHub(t *testing.T, s *Server, ctx context.Context) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/local/hub/start", strings.NewReader(`{"addr":"127.0.0.1:0","mode":"restricted","bases":["https://api.example.test"]}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	s.handleHubStart(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("start status = %d, body = %s", w.Code, w.Body.String())
	}
	var result struct {
		Addr string `json:"addr"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Addr == "" {
		t.Fatalf("invalid start result: %s, error = %v", w.Body.String(), err)
	}
	return result.Addr
}

func lifecycleHTTPClient(t *testing.T) *http.Client {
	t.Helper()
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

func TestEmbeddedHubStopCancelsActiveBridgeStream(t *testing.T) {
	s, _ := newTestServer(t)
	t.Cleanup(s.CloseSelfHostServices)
	addr := startLifecycleTestHub(t, s, context.Background())
	client := lifecycleHTTPClient(t)
	registered, err := client.Post("http://"+addr+"/salcara-hub/v1/bridge/register", "application/json", strings.NewReader(`{"deviceId":"shutdown-device","deviceSecret":"shutdown-secret"}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, registered.Body)
	_ = registered.Body.Close()
	if registered.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d", registered.StatusCode)
	}
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/salcara-hub/v1/bridge/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Device-Id", "shutdown-device")
	req.Header.Set("X-Device-Secret", "shutdown-secret")
	stream, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stream.Body.Close() })
	if stream.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d", stream.StatusCode)
	}
	streamEnded := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, stream.Body)
		close(streamEnded)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.stopEmbeddedHub(ctx); err != nil {
		t.Errorf("stop waited for the SSE client instead of cancelling it: %v", err)
	}
	select {
	case <-streamEnded:
	case <-time.After(time.Second):
		t.Error("Bridge stream remained open after Hub stop")
	}
	probe, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listener remains bound after stopping an active stream: %v", err)
	}
	_ = probe.Close()
}

func TestEmbeddedHubRetainsStoppingManagerUntilRequestsDrain(t *testing.T) {
	s, _ := newTestServer(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseRequest) }) }
	hubCtx, cancel := context.WithCancel(context.Background())
	srv := &http.Server{
		BaseContext: func(net.Listener) context.Context { return hubCtx },
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			close(requestStarted)
			<-releaseRequest
		}),
	}
	t.Cleanup(func() {
		release()
		cancel()
		_ = srv.Close()
		_ = ln.Close()
	})
	mgr := &HubManager{
		hub: hubserver.New(hubserver.Options{}), server: srv, listener: ln,
		cancel: cancel, addr: ln.Addr().String(), started: time.Now(),
		serveReturned: make(chan struct{}),
	}
	s.hubMu.Lock()
	s.embeddedHub, s.embeddedHubServer = mgr, srv
	s.hubMu.Unlock()
	ready := &readyListener{Listener: ln, ready: make(chan struct{})}
	serveDone := make(chan struct{})
	go func() {
		s.serveEmbeddedHub(mgr, ready)
		close(serveDone)
	}()
	select {
	case <-ready.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("test Hub did not begin accepting requests")
	}
	response, err := lifecycleHTTPClient(t).Get("http://" + mgr.addr + "/hold")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("test request did not enter handler")
	}
	ctx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopCancel()
	stopped := make(chan error, 1)
	go func() { stopped <- s.stopEmbeddedHub(ctx) }()
	select {
	case <-serveDone:
	case <-time.After(time.Second):
		t.Fatal("Serve did not return after shutdown closed its listener")
	}
	s.hubMu.RLock()
	retained := s.embeddedHub == mgr && mgr.stopping
	s.hubMu.RUnlock()
	if !retained {
		t.Error("manager disappeared while its active request was still draining")
	}
	status := httptest.NewRecorder()
	s.handleHubStatus(status, httptest.NewRequest(http.MethodGet, "/api/local/hub/status", nil))
	var result struct {
		Running  bool `json:"running"`
		Stopping bool `json:"stopping"`
	}
	if err := json.Unmarshal(status.Body.Bytes(), &result); err != nil || result.Running || !result.Stopping {
		t.Errorf("status hid the in-progress shutdown: %s, error = %v", status.Body.String(), err)
	}
	release()
	select {
	case err := <-stopped:
		if err != nil {
			t.Errorf("shutdown after request drained: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not complete after request drained")
	}
}

func TestEmbeddedHubOutlivesSuccessfulStartRequest(t *testing.T) {
	s, _ := newTestServer(t)
	t.Cleanup(s.CloseSelfHostServices)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := startLifecycleTestHub(t, s, ctx)
	cancel()
	response, err := lifecycleHTTPClient(t).Get("http://" + addr + "/salcara-hub/v1/ping")
	if err != nil {
		t.Fatalf("cancelling the completed start request stopped Hub: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("ping status after start request cancellation = %d", response.StatusCode)
	}
}

func TestEmbeddedHubBindFailureDoesNotPublishManager(t *testing.T) {
	s, _ := newTestServer(t)
	t.Cleanup(s.CloseSelfHostServices)
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	body, err := json.Marshal(startHubRequest{Addr: occupied.Addr().String(), Mode: "restricted", Bases: []string{"https://api.example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.handleHubStart(w, httptest.NewRequest(http.MethodPost, "/api/local/hub/start", strings.NewReader(string(body))))
	if w.Code != http.StatusConflict {
		t.Fatalf("bind failure status = %d, body = %s", w.Code, w.Body.String())
	}
	s.hubMu.RLock()
	published := s.embeddedHub != nil || s.embeddedHubServer != nil
	s.hubMu.RUnlock()
	if published {
		t.Fatal("failed listener was published as a running Hub")
	}
}

func TestEmbeddedHubExitCleanupIsIdempotentAndReleasesPort(t *testing.T) {
	s, _ := newTestServer(t)
	t.Cleanup(s.CloseSelfHostServices)
	addr := startLifecycleTestHub(t, s, context.Background())
	s.CloseSelfHostServices()
	s.CloseSelfHostServices()
	s.hubMu.RLock()
	published := s.embeddedHub != nil || s.embeddedHubServer != nil
	s.hubMu.RUnlock()
	if published {
		t.Fatal("exit cleanup left a published Hub manager")
	}
	probe, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("exit cleanup left a listening socket: %v", err)
	}
	_ = probe.Close()
}
