package console

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"salcara/bridge/internal/launcher"
)

func TestLaunchProgressReadsRealHTTPPostBodyBeforeFirstFlush(t *testing.T) {
	server := httptest.NewServer(launchProgressEndpoint(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID string `json:"id"`
		}
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true, "id": body.ID})
	}))
	defer server.Close()
	req, err := http.NewRequest("POST", server.URL, strings.NewReader(`{"id":"fixture-account"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/x-ndjson")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"status":200`) || !strings.Contains(string(body), `"id":"fixture-account"`) {
		t.Fatalf("streaming consumed the POST body before the handler: %s", body)
	}
}

func TestLaunchProgressRejectsOversizedBodyBeforeMutation(t *testing.T) {
	called := false
	handler := launchProgressEndpoint(func(w http.ResponseWriter, r *http.Request) { called = true })
	r := httptest.NewRequest("POST", "/", strings.NewReader(strings.Repeat("a", (1<<20)+1)))
	r.Header.Set("Accept", "application/x-ndjson")
	w := httptest.NewRecorder()
	handler(w, r)
	if called || w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), `"phase"`) {
		t.Fatal("oversized launch body reached the mutation or progress stream")
	}
}

func TestLaunchProgressFlushesBeforeBlockingOperationAndPreservesResult(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(launchProgressEndpoint(func(w http.ResponseWriter, r *http.Request) {
		launcher.LaunchPhase(r.Context(), "stop", "等待原工具退出")
		close(entered)
		<-release
		w.WriteHeader(http.StatusConflict)
		writeJSON(w, map[string]any{"error": "fixture save denied", "started": true, "result": map[string]int{"pid": 10}})
	}))
	defer server.Close()
	req, _ := http.NewRequest("POST", server.URL, strings.NewReader("{}"))
	req.Header.Set("Accept", "application/x-ndjson")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	if !scanner.Scan() || !strings.Contains(scanner.Text(), `"step":"source"`) {
		close(release)
		t.Fatal("first phase did not flush")
	}
	if !scanner.Scan() || !strings.Contains(scanner.Text(), `"step":"stop"`) {
		close(release)
		t.Fatal("current phase did not flush")
	}
	<-entered
	close(release)
	if !scanner.Scan() {
		t.Fatal("missing final response")
	}
	var record struct {
		Sequence int
		Event    struct {
			Type   string
			Status int
			Data   struct {
				Error   string
				Started bool
			}
		}
	}
	if json.Unmarshal(scanner.Bytes(), &record) != nil || record.Sequence != 3 || record.Event.Status != 409 || !record.Event.Data.Started || record.Event.Data.Error != "fixture save denied" {
		t.Fatalf("final outcome lost: %s", scanner.Text())
	}
}

func TestLaunchProgressKeepsNormalJSONAndAuthorization(t *testing.T) {
	called := 0
	handler := launchProgressEndpoint(func(w http.ResponseWriter, r *http.Request) { called++; writeErr(w, 409, "fixture denied") })
	w := httptest.NewRecorder()
	handler(w, httptest.NewRequest("POST", "/", nil))
	if w.Code != 409 || !strings.Contains(w.Body.String(), "fixture denied") || called != 1 {
		t.Fatal("legacy endpoint changed")
	}
	_, h := newTestServer(t)
	w = do(h, "POST", "/api/local/switch", "127.0.0.1:47831", map[string]string{"Accept": "application/x-ndjson", "Content-Type": "application/json"}, `{}`)
	if w.Code != 401 || strings.Contains(w.Body.String(), `"phase"`) {
		t.Fatal("progress bypassed console authorization")
	}
}

func TestLaunchProgressKeepsFallbackWarningAndIsolatesRequests(t *testing.T) {
	ready := make(chan struct{}, 2)
	release := make(chan struct{})
	handler := launchProgressEndpoint(func(w http.ResponseWriter, r *http.Request) {
		launcher.LaunchPhase(r.Context(), "catalog", "读取可用模型", r.URL.Path)
		ready <- struct{}{}
		<-release
		launcher.LaunchWarning(r.Context(), "catalog", "使用已保存目录 "+r.URL.Path)
		writeJSON(w, map[string]any{"ok": true})
	})
	var wg sync.WaitGroup
	for _, path := range []string{"/fixture-A", "/fixture-B"} {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			r := httptest.NewRequest("POST", path, nil)
			r.Header.Set("Accept", "application/x-ndjson")
			w := httptest.NewRecorder()
			handler(w, r)
			other := "/fixture-A"
			if path == other {
				other = "/fixture-B"
			}
			if strings.Contains(w.Body.String(), other) || !strings.Contains(w.Body.String(), `"state":"warning"`) {
				t.Error("mixed or lost progress", w.Body.String())
			}
		}(path)
	}
	<-ready
	<-ready
	close(release)
	wg.Wait()
}
