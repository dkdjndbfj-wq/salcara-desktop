package agents

import (
	"encoding/json"
	"strings"
	"testing"

	"salcara/bridge/internal/protocol"
)

func TestCodexDynamicOutputKeepsTextAndPrivateImages(t *testing.T) {
	var it cxItem
	if err := json.Unmarshal([]byte(`{"type":"dynamicToolCall","id":"call","tool":"render","status":"completed","success":true,"contentItems":[{"type":"inputText","text":"rendered result"},{"type":"inputImage","imageUrl":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAAB"},{"type":"inputImage","imageUrl":"https://example.invalid/private.png"},{"type":"inputAudio","audioUrl":"file:///private.wav"}]}`), &it); err != nil {
		t.Fatal(err)
	}
	e, ok := codexItemEvent("codex:target", it, true, "")
	if !ok || e.Output != "rendered result\n[图片]\n[图片]\n[音频]" || len(e.ImageData) != 1 || e.Status != "done" {
		t.Fatalf("lost tool content: %+v", e)
	}
	wire, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "base64") || strings.Contains(string(wire), "private.") {
		t.Fatalf("raw media escaped private handoff: %s", wire)
	}
	no := false
	it.Success = &no
	e, _ = codexItemEvent("codex:target", it, true, "")
	if e.Status != "failed" || !strings.Contains(e.Output, "rendered result") {
		t.Fatalf("lost failed output: %+v", e)
	}
}

func TestCodexHookAndSleepAreNotUserMessages(t *testing.T) {
	for _, raw := range []string{
		`{"type":"hookPrompt","id":"hook","fragments":[{"text":"hook instructions","hookRunId":"run"}]}`,
		`{"type":"sleep","id":"sleep","durationMs":1500}`,
	} {
		var it cxItem
		_ = json.Unmarshal([]byte(raw), &it)
		e, ok := codexItemEvent("codex:target", it, true, "")
		if !ok || e.Type == "message" {
			t.Fatalf("misclassified native item: %+v", e)
		}
		if it.Type == "sleep" && (e.Status != "done" || e.DurationMS != 1500 || e.Detail != "1.5 秒") {
			t.Fatalf("sleep: %+v", e)
		}
	}
}

func TestCodexMCPKeepsStructuredOutputAndPrivateImage(t *testing.T) {
	var it cxItem
	_ = json.Unmarshal([]byte(`{"type":"mcpToolCall","id":"mcp","server":"fixture","tool":"render","status":"completed","result":{"content":[],"structuredContent":{"result":"ready"},"_meta":{"private":"hidden"}}}`), &it)
	e, ok := codexItemEvent("codex:target", it, true, "")
	if !ok || e.Output != `{"result":"ready"}` || strings.Contains(e.Output, "hidden") {
		t.Fatalf("structured result: %+v", e)
	}
	it.Result = json.RawMessage(`{"content":[{"type":"image","data":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAAB","mimeType":"image/png"}]}`)
	e, _ = codexItemEvent("codex:target", it, true, "")
	if e.Output != "[image]" || len(e.ImageData) != 1 {
		t.Fatalf("image result: %+v", e)
	}
}

func TestCodexCompletedPlanIsAuthoritativeEvenWhenEmpty(t *testing.T) {
	e, ok := codexItemEvent("codex:target", cxItem{Type: "plan", ID: "plan", Text: ""}, true, "")
	if !ok || !e.OutputFinal || e.Status != "done" || e.Output != "" {
		t.Fatalf("empty plan must replace draft: %+v", e)
	}
}

func TestCodexSupplementalNotificationsKeepTaskBoundary(t *testing.T) {
	var events []protocol.Event
	a := newCodexAgent(func(e protocol.Event) { events = append(events, e) }, func() Settings { return Settings{} }, "", "")
	defer a.thr.Close()
	a.threads["target"] = &codexThread{info: protocol.SessionInfo{SessionKey: "codex:target", Title: "old", Model: "old-model"}}
	for _, raw := range []string{`{"threadId":null,"message":"global"}`, `{"threadId":"other","message":"other task"}`} {
		a.onSupplementalNotify("warning", json.RawMessage(raw))
	}
	if len(events) != 0 || len(a.threads) != 1 {
		t.Fatal("warning invented task attribution")
	}
	a.onSupplementalNotify("thread/name/updated", json.RawMessage(`{"threadId":"target","threadName":"renamed"}`))
	a.onSupplementalNotify("model/rerouted", json.RawMessage(`{"threadId":"target","turnId":"turn","fromModel":"old-model","toModel":"new-model","reason":"highRisk"}`))
	a.onSupplementalNotify("guardianWarning", json.RawMessage(`{"threadId":"target","message":"request rejected"}`))
	if len(events) != 4 || a.threads["target"].info.Title != "renamed" || a.threads["target"].info.Model != "new-model" {
		t.Fatalf("supplemental events: %+v", events)
	}
	for _, e := range events {
		if e.SessionKey != "codex:target" {
			t.Fatalf("cross-task event: %+v", e)
		}
	}
}

func TestCodexSupplementalProgressUsesExistingTool(t *testing.T) {
	a := newCodexAgent(func(protocol.Event) {}, func() Settings { return Settings{} }, "", "")
	defer a.thr.Close()
	a.threads["target"] = &codexThread{info: protocol.SessionInfo{SessionKey: "codex:target"}}
	a.onSupplementalNotify("item/mcpToolCall/progress", json.RawMessage(`{"threadId":"target","turnId":"turn","itemId":"call","message":"first"}`))
	a.onSupplementalNotify("item/mcpToolCall/progress", json.RawMessage(`{"threadId":"target","turnId":"turn","itemId":"call","message":"second"}`))
	if e := a.threads["target"].items["call"].lastEvent; e.Output != "first\nsecond" || e.Status != "running" || e.TurnID != "turn" {
		t.Fatalf("lost progress: %+v", e)
	}
	a.onSupplementalNotify("item/plan/delta", json.RawMessage(`{"threadId":"target","turnId":"turn","itemId":"plan","delta":"draft plan"}`))
	if e := a.threads["target"].items["plan"].lastEvent; e.Kind != "plan" || e.Output != "draft plan" {
		t.Fatalf("lost plan draft: %+v", e)
	}
}
