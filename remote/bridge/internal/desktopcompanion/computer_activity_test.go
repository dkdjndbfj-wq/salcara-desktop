package desktopcompanion

import (
	"context"
	"encoding/json"
	"testing"
)

func TestNativeComputerActivityAndInlineFramesStayInLeasedThread(t *testing.T) {
	fixture := gatewayFixture(t)
	fixture.result = func(command map[string]any) any {
		if command["type"] != "read" || command["sessionKey"] != "codex:"+targetID {
			t.Fatal("wrong owner", command)
		}
		var items []map[string]json.RawMessage
		if err := json.Unmarshal([]byte(`[
   {"id":"computer-running","type":"mcpToolCall","status":"inProgress","server":"cua_repl","tool":"js","arguments":{"code":"await cua.getState()"}},
   {"id":"computer-frame","type":"dynamicToolCall","status":"completed","namespace":"mcp__cua_repl","tool":"js","success":true,"contentItems":[{"type":"inputImage","imageUrl":"data:image/png;base64,abcdefghijklmnop"}]},
   {"id":"other-frame","type":"dynamicToolCall","status":"completed","namespace":"unrelated","tool":"js","success":true,"contentItems":[{"type":"inputImage","imageUrl":"data:image/png;base64,abcdefghijklmnop"}]}
  ]`), &items); err != nil {
			t.Fatal(err)
		}
		return nativeEnvelope(map[string]any{"schemaVersion": 1, "thread": map[string]any{"id": targetID, "kind": "codex", "hostId": "local"}, "turns": []any{map[string]any{"id": "turn-computer", "status": "inProgress", "items": items}}})
	}
	_, events, _, err := fixture.service.NativeOpenPage(context.Background(), "codex:"+targetID, "")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, event := range events {
		if event.ID == "computer-running" {
			found++
			if event.Computer == nil || event.Status != "running" || event.SessionKey != "codex:"+targetID {
				t.Fatalf("lost running state: %+v", event)
			}
		}
		if event.ID == "computer-frame" {
			found++
			if event.Computer == nil || len(event.ImageData) != 1 || len(event.Attachments) != 0 {
				t.Fatalf("frame bypassed attachment handoff: %+v", event)
			}
		}
		if event.ID == "other-frame" && (event.Computer != nil || len(event.ImageData) > 0) {
			t.Fatal("unrelated image claimed computer activity")
		}
	}
	if found != 2 {
		t.Fatal("missing computer records", found)
	}
	if _, _, _, err = fixture.service.NativeOpenPage(context.Background(), "codex:"+otherID, ""); err == nil {
		t.Fatal("activity expanded thread lease")
	}
}

func TestNativeToolResultFlagsOverrideOnlyCompletedToolStatus(t *testing.T) {
	for _, test := range []struct{ name, raw, status string }{
		{"mcp error", `{"type":"mcpToolCall","status":"completed","server":"cua_repl","tool":"js","result":{"isError":true}}`, "failed"},
		{"mcp error object", `{"type":"mcpToolCall","status":"completed","error":{"message":"failed"}}`, "failed"},
		{"mcp false", `{"type":"mcpToolCall","status":"completed","success":false}`, "failed"},
		{"dynamic false", `{"type":"dynamicToolCall","status":"completed","namespace":"mcp__cua_repl","tool":"js","success":false}`, "failed"},
		{"dynamic success alias", `{"type":"dynamicToolCall","status":"success","success":false}`, "failed"},
		{"dynamic true", `{"type":"dynamicToolCall","status":"completed","success":true}`, "done"},
		{"mcp true", `{"type":"mcpToolCall","status":"completed","result":{"isError":false}}`, "done"},
		{"running", `{"type":"dynamicToolCall","status":"inProgress","success":false}`, "running"},
		{"future status", `{"type":"mcpToolCall","status":"future","result":{"isError":true}}`, "future"},
		{"no status", `{"type":"dynamicToolCall","success":false}`, ""},
		{"prose", `{"type":"mcpToolCall","status":"completed","output":"isError: true"}`, "done"},
		{"ordinary assistant", `{"type":"agentMessage","status":"completed","text":"unchanged","success":false}`, "done"},
		{"function output", `{"type":"functionCallOutput","status":"completed","success":false}`, "done"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var item map[string]json.RawMessage
			if err := json.Unmarshal([]byte(test.raw), &item); err != nil {
				t.Fatal(err)
			}
			item["id"] = json.RawMessage(`"call"`)
			events := nativeEvents("codex:owner", nativeRead{Turns: []nativeTurn{{ID: "turn", Status: "completed", Items: []map[string]json.RawMessage{item}}}})
			if len(events) != 2 || events[0].ID != "call" || events[0].Status != test.status || events[0].SessionKey != "codex:owner" || events[0].TurnID != "turn" {
				t.Fatalf("wrong projected state: %+v", events)
			}
			if test.name == "ordinary assistant" && (events[0].Type != "message" || events[0].Text != "unchanged") {
				t.Fatal("ordinary message semantics changed")
			}
		})
	}
}
