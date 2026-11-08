package desktopcompanion

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"salcara/bridge/internal/protocol"
)

func TestNativeDesktopReadProjectsOnlyExplicitSuccessfulBrowserTools(t *testing.T) {
	fixture := gatewayFixture(t)
	var items []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`[
		{"id":"mcp-open","type":"mcpToolCall","status":"completed","server":"playwright","tool":"browser_navigate","arguments":{"url":"https://example.test/docs?q=1#setup","tab_id":"page-1"},"result":{"isError":false,"content":[{"type":"text","text":"navigated"}]},"output":"original result"},
		{"id":"dynamic-open","type":"dynamicToolCall","status":"completed","namespace":"mcp__codex_app","tool":"open_in_codex","success":true,"arguments":{"target":{"type":"browser","url":"https://example.test/other"}},"contentItems":[{"type":"inputText","text":"{\"tabId\":\"browser-2\"}"}]},
		{"id":"mcp-running","type":"mcpToolCall","status":"inProgress","server":"playwright","tool":"browser_navigate","arguments":{"url":"https://example.test/running"}},
		{"id":"mcp-failed","type":"mcpToolCall","status":"completed","server":"playwright","tool":"browser_navigate","arguments":{"url":"https://example.test/failed"},"result":{"isError":true}},
		{"id":"mcp-error","type":"mcpToolCall","status":"completed","server":"playwright","tool":"browser_navigate","arguments":{"url":"https://example.test/error"},"error":{"message":"timeout"}},
		{"id":"dynamic-failed","type":"dynamicToolCall","status":"completed","namespace":"mcp__codex_app","tool":"open_in_codex","success":false,"arguments":{"target":{"type":"browser","url":"https://example.test/failed"}}},
		{"id":"another-chat","type":"dynamicToolCall","status":"completed","namespace":"mcp__codex_app","tool":"open_in_codex","success":true,"arguments":{"threadId":"another-thread","target":{"type":"browser","url":"https://example.test/other-chat"}}},
		{"id":"shell-link","type":"commandExecution","status":"completed","command":"start https://example.test","browser":{"action":"open","url":"https://example.test"}},
		{"id":"search-link","type":"webSearch","status":"completed","query":"https://example.test"},
		{"id":"ordinary-link","type":"agentMessage","text":"Opened https://example.test"},
		{"id":"unknown-output","type":"functionCallOutput","status":"completed","tool":"browser_navigate","arguments":{"url":"https://example.test"}}
	]`), &items); err != nil {
		t.Fatal(err)
	}
	fixture.result = func(command map[string]any) any {
		if command["type"] != "read" || command["sessionKey"] != "codex:"+targetID {
			t.Error("browser projection requested an unrelated desktop action", command)
		}
		return nativeEnvelope(map[string]any{
			"schemaVersion": 1, "thread": map[string]any{"id": targetID, "kind": "codex", "hostId": "local"},
			"turns": []any{map[string]any{"id": "browser-turn", "status": "completed", "items": items}},
		})
	}
	_, events, next, err := fixture.service.NativeOpenPage(context.Background(), "codex:"+targetID, "")
	if err != nil || next != "" || len(events) != len(items)+1 {
		t.Fatalf("desktop read failed: %v events=%d next=%q", err, len(events), next)
	}
	want := map[string]*protocol.BrowserNavigation{
		"mcp-open":     {Action: "navigate", URL: "https://example.test/docs?q=1#setup", PageID: "playwright:page-1"},
		"dynamic-open": {Action: "open", URL: "https://example.test/other", PageID: "codex_app:browser-2"},
	}
	for _, event := range events {
		if !reflect.DeepEqual(event.Browser, want[event.ID]) || event.SessionKey != "codex:"+targetID || event.TurnID != "browser-turn" {
			t.Fatalf("desktop browser metadata lost or invented: %+v browser=%+v", event, event.Browser)
		}
		if event.Browser != nil && event.Status != "done" {
			t.Fatalf("browser activity was not terminal: %+v", event)
		}
		if event.ID == "mcp-open" && event.Output != "original result" {
			t.Fatal("browser metadata replaced the original activity output")
		}
	}
	// The phone's message-limited history path must preserve the same identities.
	_, messages, cursor, err := fixture.service.NativeOpenMessagesPage(context.Background(), "codex:"+targetID, "", 10)
	if err != nil || cursor != "" || len(messages) != len(events) {
		t.Fatalf("message-page projection dropped tool metadata: %v events=%d", err, len(messages))
	}
	for _, event := range messages {
		if !reflect.DeepEqual(event.Browser, want[event.ID]) {
			t.Fatalf("message-page browser changed: %+v", event)
		}
	}
	if _, _, _, err := fixture.service.NativeOpenPage(context.Background(), "codex:"+otherID, ""); err == nil {
		t.Fatal("browser projection expanded the desktop lease")
	}
}
