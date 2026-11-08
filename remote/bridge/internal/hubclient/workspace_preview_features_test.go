package hubclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPreviewFeatureAdmission(t *testing.T) {
	p := &workspacePreview{width: 640, height: 480, dialog: &previewDialog{ID: "1"}, chooser: &previewChooser{ID: "2", NodeID: 1}}
	for _, input := range []map[string]any{
		{"action": "mouse", "phase": "down", "x": 5, "y": 6}, {"action": "scrollTo", "x": 0, "y": 120}, {"action": "dialog", "dialogId": "1", "accept": false}, {"action": "key", "key": "a", "modifiers": 2}, {"action": "inspect"},
		{"action": "upload", "chooserId": "2", "uploadId": "upload_id_1", "name": "file.txt", "total": 1, "offset": 0, "dataBase64": "YQ==", "final": true},
	} {
		if err := p.validateInput(input); err != nil {
			t.Fatalf("valid feature rejected: %#v %v", input, err)
		}
	}
	for _, input := range []map[string]any{
		{"action": "mouse", "phase": "wrong", "x": 1, "y": 1}, {"action": "scrollTo", "x": -1, "y": 0}, {"action": "dialog", "dialogId": "stale", "accept": true}, {"action": "key", "key": "a", "modifiers": 16}, {"action": "inspect", "script": "alert(1)"},
		{"action": "upload", "chooserId": "2", "uploadId": "upload_id_1", "name": "file.txt", "total": 2, "offset": 0, "dataBase64": "YQ==", "final": true},
	} {
		if err := p.validateInput(input); err == nil {
			t.Fatalf("invalid feature accepted: %#v", input)
		}
	}
}

func TestWorkspacePreviewRealBrowserFeatures(t *testing.T) {
	if os.Getenv("SALCARA_PREVIEW_BROWSER_TEST") != "1" {
		t.Skip("opt-in real browser")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/download" {
			w.Header().Set("Content-Disposition", `attachment; filename="result.txt"`)
			fmt.Fprint(w, "downloaded content")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><body><input id="text" value="selected text"><button id="dialog" onclick="window.answer=prompt('Enter value','initial')">Prompt</button><input type="file" id="upload"><a id="download" href="/download">Download</a><div style="height:3000px">Body</div></body></html>`)
	}))
	defer server.Close()
	c, _, root := workspaceFixture(t)
	t.Cleanup(c.closeWorkspacePreviews)
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	command := func(action string, fields map[string]any) (any, error) {
		cmd := map[string]any{"type": "workspace.preview." + action, "sessionKey": workspaceTestKey, "controlSurface": "cli", "expectedCwd": root}
		for k, v := range fields {
			cmd[k] = v
		}
		return c.dispatchWorkspacePreview(ctx, cmd)
	}
	raw, err := command("open", map[string]any{"operationId": "real-browser-features", "url": server.URL, "width": 800, "height": 600})
	if err != nil {
		t.Fatal(err)
	}
	page := raw.(previewReply).PageID
	c.previewMu.Lock()
	p := c.previews[page]
	c.previewMu.Unlock()
	previewTestWait(t, ctx, p, `!!document.getElementById('text')`)
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
		raw, err := command("frame", map[string]any{"pageId": page, "capture": true})
		if err != nil {
			t.Fatal(err)
		}
		return raw.(previewReply)
	}
	click := func(id string) {
		t.Helper()
		raw := previewTestEval(t, ctx, p, fmt.Sprintf(`(()=>{const r=document.getElementById(%q).getBoundingClientRect();return {x:Math.floor(r.x+r.width/2),y:Math.floor(r.y+r.height/2)}})()`, id))
		var point map[string]any
		if err := json.Unmarshal(raw, &point); err != nil {
			t.Fatal(err)
		}
		point["action"] = "click"
		input(point)
	}
	click("text")
	input(map[string]any{"action": "key", "key": "a", "modifiers": 2})
	if text := frame().SelectionText; text != "selected text" {
		t.Fatalf("selection not captured: %q", text)
	}
	input(map[string]any{"action": "inspect"})
	if !strings.Contains(frame().Inspection, "Prompt") {
		t.Fatal("inspection missing actual DOM")
	}
	click("dialog")
	dialog := frame().Dialog
	if dialog == nil || dialog.Type != "prompt" {
		t.Fatal("prompt not surfaced")
	}
	input(map[string]any{"action": "dialog", "dialogId": dialog.ID, "accept": true, "prompt": "phone answer"})
	previewTestWait(t, ctx, p, `window.answer==='phone answer'`)
	click("upload")
	chooser := frame().Chooser
	if chooser == nil {
		t.Fatal("chooser not surfaced")
	}
	input(map[string]any{"action": "upload", "chooserId": chooser.ID, "uploadId": "test_upload_123", "name": "hello.txt", "total": 5, "offset": 0, "dataBase64": base64.StdEncoding.EncodeToString([]byte("hello")), "final": true})
	previewTestWait(t, ctx, p, `document.getElementById('upload').files.length===1`)
	if got := string(previewTestEval(t, ctx, p, `document.getElementById('upload').files[0].text()`)); got != `"hello"` {
		t.Fatalf("uploaded bytes mismatch: %s", got)
	}
	click("download")
	var downloaded *previewDownload
	for i := 0; i < 40; i++ {
		shot := frame()
		for _, d := range shot.Downloads {
			if d.State == "completed" {
				copy := d
				downloaded = &copy
			}
		}
		if downloaded != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if downloaded == nil {
		t.Fatal("download not completed")
	}
	raw, err = command("file", map[string]any{"pageId": page, "downloadId": downloaded.ID, "offset": 0})
	if err != nil {
		t.Fatal(err)
	}
	chunk := raw.(map[string]any)
	data, err := base64.StdEncoding.DecodeString(chunk["dataBase64"].(string))
	if err != nil || string(data) != "downloaded content" || chunk["done"] != true {
		t.Fatal("download bytes mismatch")
	}
	input(map[string]any{"action": "scrollTo", "x": 0, "y": 500})
	previewTestWait(t, ctx, p, `scrollY===500`)
	if frame().ScrollY != 500 {
		t.Fatal("scroll not reported")
	}
	previewTestEval(t, ctx, p, `setTimeout(()=>alert('Async alert'),50);true`)
	time.Sleep(100 * time.Millisecond)
	asyncDialog := frame().Dialog
	if asyncDialog == nil || asyncDialog.Message != "Async alert" {
		t.Fatal("asynchronous dialog blocked frame polling")
	}
	input(map[string]any{"action": "dialog", "dialogId": asyncDialog.ID, "accept": true})
}
