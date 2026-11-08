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
)

func TestPreviewDOMAdmission(t *testing.T) {
	p := &workspacePreview{width: 640, height: 480}
	for _, input := range []map[string]any{
		{"action": "element-pick", "documentId": "document_123", "x": 20, "y": 10},
		{"action": "style-preview", "documentId": "document_123", "elementId": "element_123", "revision": 1, "property": "color", "value": "red", "important": false},
		{"action": "style-undo", "documentId": "document_123", "elementId": "element_123", "revision": 1},
	} {
		if err := p.validateInput(input); err != nil {
			t.Fatalf("valid DOM input rejected: %v", err)
		}
	}
	for _, value := range []string{`url(https://evil.test)`, `u\72l(foo)`, `var(--image)`, `expression(alert(1))`, `red;display:none`, `red/*x*/`, `@import 'x'`, `<script>`} {
		if err := p.validateInput(map[string]any{"action": "style-preview", "documentId": "document_123", "elementId": "element_123", "revision": 1, "property": "color", "value": value, "important": false}); err == nil {
			t.Fatalf("unsafe style admitted: %q", value)
		}
	}
	for _, input := range []map[string]any{
		{"action": "element-pick", "documentId": "document_123", "x": 640, "y": 10},
		{"action": "element-pick", "documentId": "document_123", "x": 20, "y": 10, "script": "evil()"},
		{"action": "style-preview", "documentId": "document_123", "elementId": "element_123", "revision": 1, "property": "background-image", "value": "none", "important": false},
		{"action": "style-undo", "documentId": "document_123", "elementId": "element_123", "revision": 0},
	} {
		if err := p.validateInput(input); err == nil {
			t.Fatalf("invalid DOM input admitted: %#v", input)
		}
	}
}

func TestWorkspacePreviewRealBrowserDOM(t *testing.T) {
	if os.Getenv("SALCARA_PREVIEW_BROWSER_TEST") != "1" {
		t.Skip("opt-in real browser")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/style.css":
			w.Header().Set("Content-Type", "text/css")
			fmt.Fprint(w, `#target{position:absolute;left:40px;top:40px;width:200px;height:80px;color:rgb(0,0,255)} iframe{position:absolute;left:300px;top:40px;width:200px;height:100px}`)
		case "/child":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<button style="width:170px;height:70px">Child button</button>`)
		default:
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<link rel="stylesheet" href="/style.css"><button id="target" style="padding:4px">Target element</button><iframe src="/child"></iframe>`)
		}
	}))
	defer server.Close()
	c, _, root := workspaceFixture(t)
	t.Cleanup(c.closeWorkspacePreviews)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	command := func(action string, fields map[string]any) (any, error) {
		cmd := map[string]any{"type": "workspace.preview." + action, "sessionKey": workspaceTestKey, "controlSurface": "cli", "expectedCwd": root}
		for k, v := range fields {
			cmd[k] = v
		}
		return c.dispatchWorkspacePreview(ctx, cmd)
	}
	raw, err := command("open", map[string]any{"operationId": "real-browser-dom-test", "url": server.URL, "width": 800, "height": 600})
	if err != nil {
		t.Fatal(err)
	}
	page := raw.(previewReply).PageID
	c.previewMu.Lock()
	p := c.previews[page]
	c.previewMu.Unlock()
	previewTestWait(t, ctx, p, `!!document.getElementById('target')&&document.querySelector('iframe').contentDocument.readyState==='complete'`)
	sequence := 1
	input := func(value map[string]any) {
		t.Helper()
		if _, err := command("input", map[string]any{"pageId": page, "sequence": sequence, "input": value}); err != nil {
			t.Fatalf("input %v: %v", value, err)
		}
		sequence++
	}
	frame := func() previewReply {
		t.Helper()
		raw, err := command("frame", map[string]any{"pageId": page, "capture": false})
		if err != nil {
			t.Fatal(err)
		}
		return raw.(previewReply)
	}
	shot := frame()
	if !shot.DOMSupported || shot.DocumentID == "" {
		t.Fatal("DOM capability missing")
	}
	pick := func(x, y int) {
		t.Helper()
		input(map[string]any{"action": "element-pick", "documentId": shot.DocumentID, "x": x, "y": y})
		shot = frame()
	}
	style := func(action, property, value string) {
		t.Helper()
		e := shot.Element
		if e == nil {
			t.Fatal("no element")
		}
		v := map[string]any{"action": action, "documentId": shot.DocumentID, "elementId": e.ID, "revision": e.Revision}
		if action == "style-preview" {
			v["property"] = property
			v["value"] = value
			v["important"] = true
		}
		input(v)
		shot = frame()
	}
	pick(60, 60)
	if shot.Element == nil || shot.Element.Selector != "#target" || shot.Element.Tag != "button" || shot.Element.Text != "Target element" {
		t.Fatalf("incorrect selected node: %#v", shot.Element)
	}
	if len(shot.Element.Sources) == 0 || !strings.HasSuffix(shot.Element.Sources[0].URL, "/style.css") {
		t.Fatalf("missing real stylesheet hint: %#v", shot.Element.Sources)
	}
	encoded, _ := json.Marshal(shot.Element)
	if strings.Contains(string(encoded), "objectId") || strings.Contains(string(encoded), "backendNodeId") {
		t.Fatal("private CDP IDs leaked")
	}
	style("style-preview", "color", "red")
	if shot.DOMError != "" || shot.Element.Computed["color"] != "rgb(255, 0, 0)" || len(shot.Element.Changes) != 1 {
		t.Fatalf("style not applied: %#v %s", shot.Element, shot.DOMError)
	}
	input(map[string]any{"action": "style-preview", "documentId": shot.DocumentID, "elementId": shot.Element.ID, "revision": 1, "property": "color", "value": "green", "important": true})
	shot = frame()
	if shot.DOMError == "" || shot.Element.Computed["color"] != "rgb(255, 0, 0)" {
		t.Fatal("stale revision changed styles")
	}
	pick(330, 60)
	if shot.DOMError == "" || shot.Element.Selector != "#target" {
		t.Fatal("modified node abandoned")
	}
	style("style-undo", "", "")
	if shot.DOMError != "" || len(shot.Element.Changes) != 0 || shot.Element.Computed["color"] != "rgb(0, 0, 255)" {
		t.Fatal("undo failed")
	}
	if got := string(previewTestEval(t, ctx, p, `document.getElementById('target').style.cssText`)); got != `"padding: 4px;"` {
		t.Fatalf("original inline style not restored: %s", got)
	}
	pick(330, 60)
	if shot.DOMError == "" {
		t.Fatal("iframe node accepted")
	}
	pick(60, 60)
	style("style-preview", "padding-top", "20px")
	style("style-preview", "border-radius", "12px")
	previewTestEval(t, ctx, p, `history.pushState({},'', '/changed');true`)
	style("style-preview", "color", "green")
	if shot.DOMError == "" {
		t.Fatal("same-document URL change admitted stale edit")
	}
	style("style-undo", "", "")
	if shot.DOMError != "" {
		t.Fatalf("same document undo blocked: %s", shot.DOMError)
	}
	pick(60, 60)
	style("style-preview", "color", "red")
	previewTestEval(t, ctx, p, `document.getElementById('target').style.color='green';true`)
	style("style-undo", "", "")
	if shot.DOMError == "" || shot.Element.Computed["color"] != "rgb(0, 128, 0)" {
		t.Fatal("undo overwrote site mutation")
	}
	oldDocument := shot.DocumentID
	input(map[string]any{"action": "reload"})
	previewTestWait(t, ctx, p, `document.getElementById('target').style.color===''`)
	shot = frame()
	if shot.DocumentID == oldDocument || shot.Element != nil {
		t.Fatal("reload did not invalidate selection")
	}
	input(map[string]any{"action": "element-pick", "documentId": oldDocument, "x": 60, "y": 60})
	shot = frame()
	if shot.DOMError == "" || shot.Element != nil {
		t.Fatal("old document admitted")
	}
	pick(60, 60)
	previewTestEval(t, ctx, p, `document.getElementById('target').outerHTML='<button id="target">Replacement</button>';true`)
	style("style-preview", "color", "red")
	if shot.DOMError == "" || shot.Element != nil {
		t.Fatal("detached node was edited or re-resolved")
	}
	if got := string(previewTestEval(t, ctx, p, `document.getElementById('target').style.color`)); got != `""` {
		t.Fatal("replacement node was mutated")
	}
}
