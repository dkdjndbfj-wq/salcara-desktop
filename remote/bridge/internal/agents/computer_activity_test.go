package agents

import (
	"encoding/json"
	"testing"
)

func TestComputerActivityUsesTypedToolsAndPreservesLifecycle(t *testing.T) {
	for _, status := range []string{"inProgress", "completed", "failed"} {
		raw := json.RawMessage(`{"id":"real","type":"mcpToolCall","server":"cua_repl","tool":"js","status":"` + status + `","arguments":{"code":"await cua.getState()"}}`)
		var item cxItem
		if err := json.Unmarshal(raw, &item); err != nil {
			t.Fatal(err)
		}
		event, ok := codexItemEvent("codex:owner", item, status != "inProgress", "/fixture")
		if !ok || event.Computer == nil || event.Computer.Provider != "cua_repl" || event.Computer.Action != "script" {
			t.Fatalf("missing actual activity: %+v", event)
		}
		if status == "inProgress" && event.Status != "running" {
			t.Fatal("running became completed")
		}
		if status == "failed" && event.Status != "failed" {
			t.Fatal("failure became completed")
		}
	}
}
func TestComputerActivityDoesNotInferFromCodeOrProse(t *testing.T) {
	for _, raw := range []string{
		`{"type":"commandExecution","command":"computer screenshot"}`,
		`{"type":"mcpToolCall","server":"unrelated","tool":"click","arguments":{"computer":true}}`,
		`{"type":"dynamicToolCall","namespace":"functions","tool":"exec","arguments":{"code":"tools.computer.click()"}}`,
		`{"type":"mcpToolCall","server":"playwright","tool":"shell","arguments":{"action":"click"}}`,
	} {
		if value := ComputerActivityFromCodexItem(json.RawMessage(raw)); value != nil {
			t.Fatalf("invented activity: %+v", value)
		}
	}
}
func TestComputerActivityProjectsExplicitBrowserAndComputerActions(t *testing.T) {
	for _, test := range []struct{ raw, action, surface string }{
		{`{"type":"dynamicToolCall","namespace":"mcp__playwright","tool":"browser_click"}`, "click", "browser"},
		{`{"type":"mcpToolCall","server":"functions","tool":"mcp__computer__computer","arguments":{"action":"type","text":"private input"}}`, "type", "computer"},
		{`{"type":"mcpToolCall","server":"chrome-devtools","tool":"take_screenshot"}`, "screenshot", "browser"},
	} {
		value := ComputerActivityFromCodexItem(json.RawMessage(test.raw))
		if value == nil || value.Action != test.action || value.Surface != test.surface {
			t.Fatalf("wrong projection: %+v", value)
		}
		encoded, _ := json.Marshal(value)
		var fields map[string]any
		_ = json.Unmarshal(encoded, &fields)
		if len(fields) != 4 {
			t.Fatal("activity leaked inputs")
		}
	}
}

func TestComputerImagesAcceptOnlyInlineImagesFromComputerTools(t *testing.T) {
	for _, test := range []struct {
		raw   string
		count int
	}{
		{`{"type":"mcpToolCall","server":"cua_repl","tool":"js","result":{"content":[{"type":"image","data":"abcdefghijklmnop"}]}}`, 1},
		{`{"type":"dynamicToolCall","namespace":"mcp__cua_repl","tool":"js","contentItems":[{"type":"inputImage","imageUrl":"data:image/png;base64,abcdefghijklmnop"}]}`, 1},
		{`{"type":"dynamicToolCall","namespace":"mcp__cua_repl","tool":"js","contentItems":[{"type":"inputImage","imageUrl":"file:///private/screenshot.png"},{"type":"inputImage","imageUrl":"https://example.test/screenshot.png"}]}`, 0},
		{`{"type":"mcpToolCall","server":"unrelated","tool":"js","result":{"content":[{"type":"image","data":"abcdefghijklmnop"}]}}`, 0},
	} {
		if got := ComputerActivityImages(json.RawMessage(test.raw)); len(got) != test.count {
			t.Fatalf("image admission mismatch: %d", len(got))
		}
	}
}

func TestComputerActivityClaudeKeepsCallIdentityAndFailure(t *testing.T) {
	mapper := newClaudeMapper("claude:owner", "")
	start := mapper.assistant(json.RawMessage(`{"id":"assistant","content":[{"type":"tool_use","id":"real-call","name":"mcp__computer__computer","input":{"action":"scroll"}}]}`), "", 1)
	if len(start) != 1 || start[0].Computer == nil || start[0].Computer.Action != "scroll" || start[0].Status != "running" {
		t.Fatalf("missing actual call: %+v", start)
	}
	end := mapper.toolResult(claudeBlock{ToolUseID: "real-call", IsError: true, Content: json.RawMessage(`"failed"`)}, 2)
	if end.ID != "real-call" || end.Computer == nil || end.Status != "failed" {
		t.Fatalf("lost call result: %+v", end)
	}
}

func TestComputerActivityMCPErrorIsNotReportedCompleted(t *testing.T) {
	var item cxItem
	_ = json.Unmarshal([]byte(`{"id":"failed-call","type":"mcpToolCall","server":"playwright","tool":"browser_click","status":"completed","result":{"isError":true,"content":[]}}`), &item)
	event, ok := codexItemEvent("codex:owner", item, true, "")
	if !ok || event.Computer == nil || event.Status != "failed" {
		t.Fatalf("failure lost: %+v", event)
	}
}

func TestCodexToolResultFailureFlagsAreTypedAndExplicit(t *testing.T) {
	for _, test := range []struct {
		raw    string
		failed bool
	}{
		{`{"type":"mcpToolCall","result":{"isError":true}}`, true},
		{`{"type":"mcpToolCall","error":{"message":"failed"}}`, true},
		{`{"type":"mcpToolCall","success":false}`, true},
		{`{"type":"dynamicToolCall","success":false}`, true},
		{`{"type":"dynamicToolCall","success":true}`, false},
		{`{"type":"mcpToolCall","error":null,"result":{"isError":false}}`, false},
		{`{"type":"mcpToolCall","result":{"content":[{"type":"text","text":"success=false, isError=true"}]}}`, false},
		{`{"type":"agentMessage","success":false,"result":{"isError":true}}`, false},
		{`{"type":"functionCallOutput","success":false}`, false},
		{`{"type":"dynamicToolCall"}`, false},
		{`{"type":"mcpToolCall","success":"false"}`, false},
		{`invalid`, false},
	} {
		if got := CodexToolResultFailed(json.RawMessage(test.raw)); got != test.failed {
			t.Fatalf("wrong result failure for %s: %v", test.raw, got)
		}
	}
}
