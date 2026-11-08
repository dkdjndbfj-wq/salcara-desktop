package console

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestNormalizeHubStartRequestEnforcesPolicy(t *testing.T) {
	tests := []struct {
		name    string
		request startHubRequest
		wantErr bool
	}{
		{name: "open requires confirmation", request: startHubRequest{Mode: "open"}, wantErr: true},
		{name: "public bind rejected", request: startHubRequest{Addr: ":8765", Mode: "restricted", Bases: []string{"https://api.example.com"}}, wantErr: true},
		{name: "loopback default", request: startHubRequest{Mode: "restricted", Bases: []string{"https://api.example.com"}}},
		{name: "open rejects restricted bases", request: startHubRequest{Mode: "open", ConfirmOpen: true, Bases: []string{"https://api.example.com"}}, wantErr: true},
		{name: "restricted requires a base", request: startHubRequest{Mode: "restricted"}, wantErr: true},
		{name: "restricted rejects invalid base", request: startHubRequest{Mode: "restricted", Bases: []string{"https://user:pass@example.com"}}, wantErr: true},
		{name: "restricted rejects duplicates", request: startHubRequest{Mode: "restricted", Bases: []string{"https://api.example.com", "https://api.example.com/v1"}}, wantErr: true},
		{name: "restricted canonicalizes bases", request: startHubRequest{Mode: "restricted", Bases: []string{"HTTPS://API.EXAMPLE.COM/v1"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := normalizeHubStartRequest(&tc.request)
			if (err != nil) != tc.wantErr {
				t.Fatalf("normalizeHubStartRequest() error = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && tc.name == "restricted canonicalizes bases" && tc.request.Bases[0] != "https://api.example.com" {
				t.Fatalf("canonical base = %q", tc.request.Bases[0])
			}
			if err == nil && tc.name == "loopback default" && tc.request.Addr != "127.0.0.1:8765" {
				t.Fatalf("default address = %q", tc.request.Addr)
			}
		})
	}

	tooMany := startHubRequest{Mode: "restricted", Bases: make([]string, 33)}
	for i := range tooMany.Bases {
		tooMany.Bases[i] = "https://api-" + strings.Repeat("x", i+1) + ".example.com"
	}
	if err := normalizeHubStartRequest(&tooMany); err == nil {
		t.Fatal("expected more than 32 API bases to be rejected")
	}
}

func TestEmbeddedHubStartStopClosesListenerAndReportsPolicy(t *testing.T) {
	s, _ := newTestServer(t)
	t.Cleanup(s.CloseSelfHostServices)

	start := httptest.NewRequest(http.MethodPost, "/api/local/hub/start", strings.NewReader(`{"addr":"127.0.0.1:0","mode":"restricted","revision":"policy-test","bases":["https://api.example.com/v1"]}`))
	started := httptest.NewRecorder()
	s.handleHubStart(started, start)
	if started.Code != http.StatusOK {
		t.Fatalf("start status = %d, body=%s", started.Code, started.Body)
	}
	var startResult struct {
		Addr string `json:"addr"`
	}
	if err := json.Unmarshal(started.Body.Bytes(), &startResult); err != nil {
		t.Fatal(err)
	}
	if startResult.Addr == "" {
		t.Fatal("start response omitted bound address")
	}

	status := httptest.NewRecorder()
	s.handleHubStatus(status, httptest.NewRequest(http.MethodGet, "/api/local/hub/status", nil))
	var statusResult struct {
		Running  bool     `json:"running"`
		Mode     string   `json:"mode"`
		Revision string   `json:"revision"`
		Bases    []string `json:"bases"`
	}
	if err := json.Unmarshal(status.Body.Bytes(), &statusResult); err != nil {
		t.Fatal(err)
	}
	if !statusResult.Running || statusResult.Mode != "restricted" || statusResult.Revision != "policy-test" || len(statusResult.Bases) != 1 || statusResult.Bases[0] != "https://api.example.com" {
		t.Fatalf("unexpected Hub status: %+v", statusResult)
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			s.handleHubStatus(w, httptest.NewRequest(http.MethodGet, "/api/local/hub/status", nil))
		}()
	}
	stop := httptest.NewRecorder()
	s.handleHubStop(stop, httptest.NewRequest(http.MethodPost, "/api/local/hub/stop", strings.NewReader(`{}`)))
	wg.Wait()
	if stop.Code != http.StatusOK {
		t.Fatalf("stop status = %d, body=%s", stop.Code, stop.Body)
	}

	probe, err := net.Listen("tcp", startResult.Addr)
	if err != nil {
		t.Fatalf("listener still bound after stop: %v", err)
	}
	_ = probe.Close()
}

func TestEmbeddedHubOpenModeCannotBypassConfirmation(t *testing.T) {
	s, _ := newTestServer(t)
	t.Cleanup(s.CloseSelfHostServices)
	start := httptest.NewRequest(http.MethodPost, "/api/local/hub/start", strings.NewReader(`{"addr":"127.0.0.1:0","mode":"open"}`))
	w := httptest.NewRecorder()
	s.handleHubStart(w, start)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("start status = %d, want %d; body=%s", w.Code, http.StatusBadRequest, w.Body)
	}
	if s.embeddedHub != nil {
		t.Fatal("Hub started without open-mode confirmation")
	}
}
