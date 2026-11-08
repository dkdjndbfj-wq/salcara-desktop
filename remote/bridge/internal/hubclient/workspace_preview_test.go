package hubclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/jpeg"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"salcara/bridge/internal/config"
)

func TestPreviewURLAndInputAdmission(t *testing.T) {
	for _, raw := range []string{"http://localhost:8083/", "http://127.0.0.1:2345/a?b=2", "http://[::1]:3000/"} {
		if _, err := previewURL(raw); err != nil {
			t.Fatalf("valid loopback rejected: %s", raw)
		}
	}
	for _, raw := range []string{"file:///C:/secret.txt", "http://localhost.example/", "http://127.1/", "http://user:pass@localhost/", "http://192.168.1.2/", "javascript:alert(1)", "http://localhost/\n"} {
		if _, err := previewURL(raw); err == nil {
			t.Fatalf("unsafe preview admitted: %s", raw)
		}
	}
	p := &workspacePreview{width: 800, height: 600}
	for _, input := range []map[string]any{{"action": "click", "x": 800, "y": 0}, {"action": "scroll", "deltaY": math.Inf(1)}, {"action": "text", "text": strings.Repeat("x", 16385)}, {"action": "key", "key": "F12"}, {"action": "click", "x": 1, "y": 1, "script": "ignored"}, {"action": "navigate", "url": "file:///tmp/test"}} {
		if err := p.validateInput(input); err == nil {
			t.Fatalf("invalid input accepted: %#v", input)
		}
	}
}

func TestPreviewProfileCleanupOnlyOwnTempDirectory(t *testing.T) {
	ordinary := t.TempDir()
	if err := os.WriteFile(filepath.Join(ordinary, "keep.txt"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	removePreviewProfile(ordinary)
	if _, err := os.Stat(filepath.Join(ordinary, "keep.txt")); err != nil {
		t.Fatal("ordinary directory removed")
	}
	own, err := os.MkdirTemp("", "salcara-preview-")
	if err != nil {
		t.Fatal(err)
	}
	removePreviewProfile(own)
	if _, err := os.Stat(own); !os.IsNotExist(err) {
		t.Fatalf("owned directory not removed: %v", err)
	}
}

func TestPreviewUnconfirmedInputIsNeverAcknowledgedAsSuccessful(t *testing.T) {
	c, _, root := workspaceFixture(t)
	input := map[string]any{"action": "click", "x": 1, "y": 1}
	cmd := map[string]any{"type": "workspace.preview.input", "sessionKey": workspaceTestKey, "controlSurface": "cli", "expectedCwd": root, "pageId": "test-page", "sequence": 1, "input": input}
	scope, err := c.previewScope(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(input)
	p := &workspacePreview{id: "test-page", scope: scope, sequence: 2, lastInput: sha256.Sum256(raw), width: 800, height: 600}
	c.previews = map[string]*workspacePreview{p.id: p}
	if _, err := c.dispatchWorkspacePreview(context.Background(), cmd); err == nil || !strings.Contains(err.Error(), "preview_input_uncertain") {
		t.Fatalf("unconfirmed click was acknowledged: %v", err)
	}
	if p.sequence != 2 {
		t.Fatal("retry consumed another input sequence")
	}
	p.lastInputConfirmed = true
	if _, err := c.dispatchWorkspacePreview(context.Background(), cmd); err != nil {
		t.Fatalf("confirmed retry rejected: %v", err)
	}
}

func previewTestEval(t *testing.T, ctx context.Context, p *workspacePreview, expression string) json.RawMessage {
	t.Helper()
	var result struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		Exception any `json:"exceptionDetails"`
	}
	p.mu.Lock()
	err := p.call(ctx, "Runtime.evaluate", map[string]any{"expression": expression, "returnByValue": true, "awaitPromise": true}, &result)
	p.mu.Unlock()
	if err != nil || result.Exception != nil {
		t.Fatalf("browser evaluation failed: %v, %v", err, result.Exception)
	}
	return result.Result.Value
}
func previewTestWait(t *testing.T, ctx context.Context, p *workspacePreview, expression string) {
	t.Helper()
	until := time.Now().Add(8 * time.Second)
	for time.Now().Before(until) {
		if string(previewTestEval(t, ctx, p, expression)) == "true" {
			return
		}
		time.Sleep(40 * time.Millisecond)
	}
	t.Fatalf("browser condition did not become true: %s", expression)
}

// This test launches the actual installed headless Edge/Chrome, fetches CSS,
// JavaScript and WebSockets, performs CDP input and decodes its real screenshot.
func TestWorkspacePreviewRealBrowser(t *testing.T) {
	if os.Getenv("SALCARA_PREVIEW_BROWSER_TEST") != "1" {
		t.Skip("set SALCARA_PREVIEW_BROWSER_TEST=1 to exercise installed Edge/Chrome")
	}
	if previewBrowserExecutable() == "" {
		t.Fatal("no installed Edge/Chrome")
	}
	var cssRequests, apiRequests, websocketMessages atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><head><link rel="stylesheet" href="/style.css"></head><body><form id="form"><input id="field" autocomplete="off"><button id="submit">Send</button></form><button id="fetch" type="button">Fetch</button><div id="result"></div><div style="height:2400px"></div><script src="/app.js"></script></body></html>`)
	})
	mux.HandleFunc("/style.css", func(w http.ResponseWriter, r *http.Request) {
		cssRequests.Add(1)
		w.Header().Set("Content-Type", "text/css")
		fmt.Fprint(w, `body{margin:0;background:rgb(234,240,250)} input{width:220px;height:40px} button{height:40px}`)
	})
	mux.HandleFunc("/app.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		fmt.Fprint(w, `window.submitted=0;window.loaded=true;document.getElementById('form').onsubmit=e=>{e.preventDefault();window.submitted++};document.getElementById('fetch').onclick=async()=>{document.getElementById('result').textContent=await(await fetch('/api')).text()};const ws=new WebSocket('ws://'+location.host+'/socket');ws.onopen=()=>ws.send('phone-preview');ws.onmessage=e=>window.wsReply=e.data;`)
	})
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) { apiRequests.Add(1); fmt.Fprint(w, "real API result") })
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux.HandleFunc("/socket", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		typ, data, err := conn.ReadMessage()
		if err == nil {
			websocketMessages.Add(1)
			_ = conn.WriteMessage(typ, append([]byte("echo:"), data...))
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	c, _, root := workspaceFixture(t)
	t.Cleanup(c.closeWorkspacePreviews)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	base := map[string]any{"sessionKey": workspaceTestKey, "controlSurface": "cli", "expectedCwd": root}
	command := func(typ string, fields map[string]any) (any, error) {
		cmd := map[string]any{"type": "workspace.preview." + typ}
		for k, v := range base {
			cmd[k] = v
		}
		for k, v := range fields {
			cmd[k] = v
		}
		return c.dispatchWorkspacePreview(ctx, cmd)
	}
	opened, err := command("open", map[string]any{"operationId": "real-browser-test-1", "url": server.URL, "width": 800, "height": 600})
	if err != nil {
		t.Fatalf("real browser open: %v", err)
	}
	reply := opened.(previewReply)
	c.previewMu.Lock()
	p := c.previews[reply.PageID]
	c.previewMu.Unlock()
	if p == nil {
		t.Fatal("preview not registered")
	}
	profile := p.profile
	previewTestWait(t, ctx, p, `window.loaded === true && window.wsReply === 'echo:phone-preview'`)
	if cssRequests.Load() == 0 || websocketMessages.Load() == 0 {
		t.Fatal("assets or WebSocket were not fetched")
	}
	if got := string(previewTestEval(t, ctx, p, `getComputedStyle(document.body).backgroundColor`)); got != `"rgb(234, 240, 250)"` {
		t.Fatalf("CSS did not apply: %s", got)
	}
	sequence := 1
	input := func(value map[string]any) {
		t.Helper()
		if _, err := command("input", map[string]any{"pageId": p.id, "sequence": sequence, "input": value}); err != nil {
			t.Fatalf("input %#v: %v", value, err)
		}
		sequence++
	}
	click := func(id string) {
		t.Helper()
		raw := previewTestEval(t, ctx, p, fmt.Sprintf(`(()=>{const r=document.getElementById(%q).getBoundingClientRect();return {x:Math.floor(r.x+r.width/2),y:Math.floor(r.y+r.height/2)}})()`, id))
		var position map[string]any
		if err := json.Unmarshal(raw, &position); err != nil {
			t.Fatal(err)
		}
		position["action"] = "click"
		input(position)
	}
	click("field")
	input(map[string]any{"action": "text", "text": "hello远程"})
	input(map[string]any{"action": "key", "key": "Backspace"})
	if got := string(previewTestEval(t, ctx, p, `document.getElementById('field').value === 'hello远'`)); got != `true` {
		t.Fatalf("text/backspace mismatch: %s", got)
	}
	input(map[string]any{"action": "key", "key": "Enter"})
	previewTestWait(t, ctx, p, `window.submitted === 1`)
	click("fetch")
	previewTestWait(t, ctx, p, `document.getElementById('result').textContent === 'real API result'`)
	if apiRequests.Load() != 1 {
		t.Fatal("fetch not executed exactly once")
	}
	// A duplicate transport delivery acknowledges the confirmed action without another click.
	last := map[string]any{"action": "key", "key": "Enter"}
	click("field")
	input(last)
	if _, err = command("input", map[string]any{"pageId": p.id, "sequence": sequence - 1, "input": last}); err != nil {
		t.Fatal(err)
	}
	previewTestWait(t, ctx, p, `window.submitted === 2`)
	if _, err = command("input", map[string]any{"pageId": p.id, "sequence": sequence, "input": map[string]any{"action": "key", "key": "F12"}}); err == nil {
		t.Fatal("invalid key admitted")
	}
	input(map[string]any{"action": "scroll", "deltaY": 450})
	previewTestWait(t, ctx, p, `scrollY > 0`)
	input(map[string]any{"action": "resize", "width": 640, "height": 480})
	reopened, err := command("open", map[string]any{"operationId": "real-browser-test-1", "url": server.URL, "width": 800, "height": 600})
	if err != nil || reopened.(previewReply).PageID != p.id {
		t.Fatalf("open retry after resize created another page: %v", err)
	}
	frame, err := command("frame", map[string]any{"pageId": p.id, "capture": true})
	if err != nil {
		t.Fatal(err)
	}
	shot := frame.(previewReply)
	data, err := base64.StdEncoding.DecodeString(shot.Image)
	if err != nil {
		t.Fatal(err)
	}
	image, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil || image.Width != 640 || image.Height != 480 {
		t.Fatalf("real frame dimensions: %dx%d (%v)", image.Width, image.Height, err)
	}
	if len(shot.Image) > 220<<10 || shot.NextSequence != int64(sequence) {
		t.Fatal("invalid frame bounds or input cursor")
	}
	input(map[string]any{"action": "reload"})
	previewTestWait(t, ctx, p, `window.loaded === true && window.submitted === 0`)
	input(map[string]any{"action": "navigate", "url": server.URL + "/second"})
	previewTestWait(t, ctx, p, `location.pathname === '/second' && window.loaded === true`)
	if _, err = command("close", map[string]any{"pageId": p.id}); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(profile); !os.IsNotExist(err) {
		t.Fatalf("isolated browser profile survived close: %v", err)
	}
	c.previewMu.Lock()
	remaining := len(c.previews)
	c.previewMu.Unlock()
	if remaining != 0 {
		t.Fatal("closed page still consumes capacity")
	}
}

func TestWorkspacePreviewRevocationStopsIdleBrowser(t *testing.T) {
	if os.Getenv("SALCARA_PREVIEW_BROWSER_TEST") != "1" {
		t.Skip("opt-in actual browser")
	}
	c, _, root := workspaceFixture(t)
	t.Cleanup(c.closeWorkspacePreviews)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "preview") }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	opened, err := c.dispatchWorkspacePreview(ctx, map[string]any{"type": "workspace.preview.open", "sessionKey": workspaceTestKey, "controlSurface": "cli", "expectedCwd": root, "operationId": "revoke-browser-test", "url": server.URL, "width": 640, "height": 480})
	if err != nil {
		t.Fatal(err)
	}
	c.previewMu.Lock()
	p := c.previews[opened.(previewReply).PageID]
	c.previewMu.Unlock()
	if err = c.o.Store.Update(func(cfg *config.Config) error { cfg.Projects = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.done:
	case <-time.After(12 * time.Second):
		t.Fatal("revoked preview process remained alive")
	}
}
