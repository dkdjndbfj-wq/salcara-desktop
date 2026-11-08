package hubclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

func TestPreviewLogsBoundUTF16WireLength(t *testing.T) {
	p := &workspacePreview{logsSupported: true}
	p.appendLog("error", strings.Repeat("😀", 2000), previewLogLocation{URL: strings.Repeat("😀", 1024), Line: -1, Column: -1})
	if len(p.logs) != 1 {
		t.Fatalf("lost log: %#v", p.logs)
	}
	entry := p.logs[0]
	if len(utf16.Encode([]rune(entry.Message))) != 2000 || len(utf16.Encode([]rune(entry.Source))) != 1024 || !entry.Truncated {
		t.Fatalf("log would exceed mobile wire bounds: message=%d source=%d clipped=%v", len(utf16.Encode([]rune(entry.Message))), len(utf16.Encode([]rune(entry.Source))), entry.Truncated)
	}
	if text, clipped := previewLogText("a😀b", 2); text != "a" || !clipped {
		t.Fatalf("split a supplementary-plane character: %q clipped=%v", text, clipped)
	}
}

func TestPreviewLogsCaptureBoundaries(t *testing.T) {
	p := &workspacePreview{logsSupported: true}
	p.handleEvent("Runtime.consoleAPICalled", json.RawMessage(`{"type":"warning","args":[{"type":"string","value":"slow render"},{"type":"number","value":2}],"stackTrace":{"callFrames":[{"url":"http://localhost/app.js","lineNumber":6,"columnNumber":2}]}}`))
	p.handleEvent("Runtime.exceptionThrown", json.RawMessage(`{"exceptionDetails":{"text":"Uncaught","url":"http://localhost/app.js","lineNumber":8,"columnNumber":1,"exception":{"description":"Error: broken"}}}`))
	if len(p.logs) != 2 || p.logs[0].Level != "warn" || p.logs[0].Message != "slow render 2" || p.logs[0].Line != 7 || p.logs[0].Column != 3 || p.logs[1].Message != "Error: broken" || p.logs[1].Line != 9 {
		t.Fatalf("wrong log observations: %#v", p.logs)
	}
	for range 100 {
		p.appendLog("log", strings.Repeat("x", 3000), previewLogLocation{URL: "http://localhost/app.js"})
	}
	size := 0
	for _, entry := range p.logs {
		size += len(entry.Message) + len(entry.Source)
	}
	if len(p.logs) > 64 || size > 32000 || !p.logs[len(p.logs)-1].Truncated {
		t.Fatalf("unbounded logs: %d %d", len(p.logs), size)
	}
	sequence := p.logSequence
	p.handleEvent("Page.frameNavigated", json.RawMessage(`{"frame":{"parentId":"child-parent"}}`))
	if len(p.logs) == 0 {
		t.Fatal("child navigation cleared main logs")
	}
	p.handleEvent("Page.frameNavigated", json.RawMessage(`{"frame":{"id":"main"}}`))
	if len(p.logs) != 0 || p.logSequence != sequence {
		t.Fatal("navigation must clear entries without reusing their ids")
	}
	p.logsSupported = false
	p.handleEvent("Runtime.consoleAPICalled", json.RawMessage(`{"type":"error","args":[{"value":"unavailable"}]}`))
	if len(p.logs) != 0 {
		t.Fatal("disabled capture invented logs")
	}
}

func TestWorkspacePreviewRealBrowserLogs(t *testing.T) {
	if os.Getenv("SALCARA_PREVIEW_BROWSER_TEST") != "1" {
		t.Skip("opt-in real browser")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!doctype html><script>console.warn("real preview warning");setTimeout(()=>{throw new Error("real preview failure")},30)</script><p>Preview</p>`)
	}))
	defer server.Close()
	c, _, root := workspaceFixture(t)
	t.Cleanup(c.closeWorkspacePreviews)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	base := map[string]any{"type": "workspace.preview.open", "sessionKey": workspaceTestKey, "controlSurface": "cli", "expectedCwd": root, "operationId": "real-browser-log-fixture", "url": server.URL, "width": 640, "height": 480}
	raw, err := c.dispatchWorkspacePreview(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	page := raw.(previewReply).PageID
	for range 30 {
		raw, err = c.dispatchWorkspacePreview(ctx, map[string]any{"type": "workspace.preview.frame", "sessionKey": workspaceTestKey, "controlSurface": "cli", "expectedCwd": root, "pageId": page, "capture": false})
		if err != nil {
			t.Fatal(err)
		}
		frame := raw.(previewReply)
		warn, failed := false, false
		for _, entry := range frame.Logs {
			if strings.Contains(entry.Message, "real preview warning") {
				warn = entry.Level == "warn" && entry.Source == server.URL+"/" && entry.Line > 0
			}
			if strings.Contains(entry.Message, "real preview failure") {
				failed = entry.Level == "error" && entry.Line > 0
			}
		}
		if frame.LogsSupported && warn && failed {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("real runtime observations not received: %#v", raw)
}
