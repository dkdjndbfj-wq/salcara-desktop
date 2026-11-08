package agents

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"salcara/bridge/internal/protocol"
)

func TestCodexExplicitBrowserNavigationMappings(t *testing.T) {
	for _, tc := range []struct {
		name, item, action, page string
	}{
		{"playwright", `{"type":"mcpToolCall","server":"playwright","tool":"browser_navigate","arguments":{"url":"https://example.test/path?q=1#section","tab_id":"tab-1"},"result":{"content":[{"type":"text","text":"Opened page"}]}}`, "navigate", "playwright:tab-1"},
		{"custom browser server", `{"type":"mcpToolCall","server":"browser-two","tool":"browser_navigate","arguments":{"url":"https://example.test/path?q=1#section","pageId":"tab-1"},"result":{"content":[]}}`, "navigate", "browser-two:tab-1"},
		{"puppeteer typed result", `{"type":"mcpToolCall","server":"puppeteer","tool":"puppeteer_navigate","arguments":{"url":"https://example.test/path?q=1#section"},"result":{"structuredContent":{"page_id":2},"content":[]}}`, "navigate", "puppeteer:2"},
		{"chrome new", `{"type":"mcpToolCall","server":"chrome-devtools","tool":"new_page","arguments":{"url":"https://example.test/path?q=1#section"},"result":{"pageId":0}}`, "open", "chrome-devtools:0"},
		{"chrome navigate", `{"type":"mcpToolCall","server":"chrome_devtools","tool":"navigate_page","arguments":{"type":"url","url":"https://example.test/path?q=1#section","page_id":0},"result":{"content":[]}}`, "navigate", "chrome_devtools:0"},
		{"codex app new", `{"type":"mcpToolCall","server":"codex_app","tool":"open_in_codex","arguments":{"target":{"type":"browser","url":"https://example.test/path?q=1#section"}},"result":{"content":[]}}`, "open", ""},
		{"codex app existing", `{"type":"mcpToolCall","server":"codex_app","tool":"open_in_codex","arguments":{"target":{"type":"browser","url":"https://example.test/path?q=1#section","tabId":"iab:7"}},"result":{"tabId":"iab:7"}}`, "navigate", "codex_app:iab:7"},
		{"dynamic qualified", `{"type":"dynamicToolCall","namespace":"functions","tool":"mcp__playwright__browser_navigate","success":true,"arguments":{"url":"https://example.test/path?q=1#section","tabId":"tab-1"},"contentItems":[{"type":"inputText","text":"Opened page"}]}`, "navigate", "playwright:tab-1"},
		{"dynamic native namespace", `{"type":"dynamicToolCall","namespace":"mcp__codex_app","tool":"open_in_codex","success":true,"arguments":{"target":{"type":"browser","url":"https://example.test/path?q=1#section"}},"result":{"tabId":"7"},"contentItems":[]}`, "open", "codex_app:7"},
		{"dynamic dotted namespace", `{"type":"dynamicToolCall","tool":"chrome-devtools.new_page","success":true,"arguments":{"url":"https://example.test/path?q=1#section"},"contentItems":[{"type":"inputText","text":"{\"pageId\":3}"}]}`, "open", "chrome-devtools:3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var item cxItem
			if err := json.Unmarshal([]byte(tc.item), &item); err != nil {
				t.Fatal(err)
			}
			item.ID, item.Status = "navigation-1", "completed"
			started, _ := codexItemEvent("codex:owner", item, false, "")
			if started.Browser != nil {
				t.Fatal("starting event opened a browser")
			}
			event, ok := codexItemEvent("codex:owner", item, true, "")
			want := &protocol.BrowserNavigation{Action: tc.action, URL: "https://example.test/path?q=1#section", PageID: tc.page}
			if !ok || event.Type != "tool" || event.Status != "done" || event.SessionKey != "codex:owner" || event.ID != item.ID || !reflect.DeepEqual(event.Browser, want) {
				t.Fatalf("explicit completed navigation was not retained: %+v browser=%+v", event, event.Browser)
			}
			wire, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			var roundtrip protocol.Event
			if err := json.Unmarshal(wire, &roundtrip); err != nil || !reflect.DeepEqual(roundtrip.Browser, want) {
				t.Fatalf("browser metadata lost during protocol serialization: %s", wire)
			}
		})
	}
}

func TestCodexBrowserNavigationRequiresSuccessfulOwnedResult(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*cxItem)
	}{
		{"in progress", func(it *cxItem) { it.Status = "inProgress" }},
		{"missing status", func(it *cxItem) { it.Status = "" }},
		{"failed", func(it *cxItem) { it.Status = "failed" }},
		{"declined", func(it *cxItem) { it.Status = "declined" }},
		{"native error", func(it *cxItem) { it.Error = json.RawMessage(`{"message":"timeout"}`) }},
		{"mcp error flag", func(it *cxItem) {
			it.Result = json.RawMessage(`{"isError":true,"content":[{"type":"text","text":"failed"}]}`)
		}},
		{"mcp structured error", func(it *cxItem) { it.Result = json.RawMessage(`{"structuredContent":{"isError":true}}`) }},
		{"mcp serialized error", func(it *cxItem) {
			it.Result = json.RawMessage(`{"content":[{"type":"text","text":"{\"isError\":true}"}]}`)
		}},
		{"mcp malformed error flag", func(it *cxItem) { it.Result = json.RawMessage(`{"isError":"false"}`) }},
		{"ambiguous page", func(it *cxItem) { it.Result = json.RawMessage(`{"pageId":"another-tab"}`) }},
		{"invalid arguments", func(it *cxItem) { it.Arguments = json.RawMessage(`"https://example.test"`) }},
		{"dynamic no success", func(it *cxItem) { it.Type = "dynamicToolCall"; ns := "playwright"; it.Namespace = &ns }},
		{"dynamic failure", func(it *cxItem) { it.Type = "dynamicToolCall"; no := false; it.Success = &no }},
		{"dynamic inner error", func(it *cxItem) {
			it.Type = "dynamicToolCall"
			yes := true
			it.Success = &yes
			it.ContentItems = json.RawMessage(`[{"type":"inputText","text":"{\"isError\":true}"}]`)
		}},
		{"dynamic result error", func(it *cxItem) {
			it.Type = "dynamicToolCall"
			yes := true
			it.Success = &yes
			it.Result = json.RawMessage(`{"isError":true}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := cxItem{Type: "mcpToolCall", ID: "open", Server: "playwright", Tool: "browser_navigate", Status: "completed",
				Arguments: json.RawMessage(`{"url":"https://example.test","tabId":"tab-1"}`), Result: json.RawMessage(`{"content":[{"type":"text","text":"original output"}]}`)}
			tc.edit(&item)
			event, ok := codexItemEvent("codex:owner", item, true, "")
			if !ok || event.Type != "tool" || event.Browser != nil {
				t.Fatalf("failed/unowned navigation generated browser metadata: %+v", event)
			}
		})
	}
}

func TestClaudeBrowserNavigationFollowsSuccessfulToolResultOnly(t *testing.T) {
	for _, tc := range []struct {
		name, content   string
		failed, browser bool
	}{
		{"success", `[{"type":"text","text":"Opened page"}]`, false, true},
		{"tool error", `"timeout"`, true, false},
		{"MCP error", `{"isError":true,"content":[]}`, false, false},
		{"serialized MCP error", `[{"type":"text","text":"{\"isError\":true}"}]`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mapper := newClaudeMapper("claude:owner", "")
			start := mapper.assistant(json.RawMessage(`{"id":"assistant","content":[{"type":"tool_use","id":"open-1","name":"mcp__playwright__browser_navigate","input":{"url":"https://example.test","tab_id":"tab-1"}}]}`), "", 1)
			if len(start) != 1 || start[0].Browser != nil || start[0].Status != "running" {
				t.Fatalf("tool start must remain an ordinary activity: %+v", start)
			}
			block := claudeBlock{ToolUseID: "open-1", Content: json.RawMessage(tc.content), IsError: tc.failed}
			event := mapper.toolResult(block, 2)
			if (event.Browser != nil) != tc.browser || event.SessionKey != "claude:owner" || event.ID != "open-1" {
				t.Fatalf("incorrect completed tool metadata: %+v browser=%+v", event, event.Browser)
			}
			if tc.browser && (event.Browser.PageID != "playwright:tab-1" || event.Output != "Opened page") {
				t.Fatalf("lost original result or namespaced identity: %+v", event)
			}
			if mapper.toolResult(block, 3).Browser != nil {
				t.Fatal("unmatched duplicate result invented tool input ownership")
			}
		})
	}
}

func TestBrowserNavigationNeverInfersLinksOrOtherTaskTargets(t *testing.T) {
	input := map[string]any{"url": "https://example.test", "command": "start https://example.test"}
	for _, name := range []string{"WebFetch", "WebSearch", "Bash", "shell", "exec_command", "mcp__search__search", "mcp__unknown__new_page", "mcp__unknown__navigate_page", "mcp__other__open_in_codex", "browser_click", "puppeteer_evaluate", "mcp__cua_repl__js"} {
		if nav := browserNavigation("", name, input, json.RawMessage(`"https://example.test"`)); nav != nil {
			t.Fatalf("unrecognized tool %s opened a browser: %+v", name, nav)
		}
	}
	for _, raw := range []string{
		`{"target":{"type":"file","url":"https://example.test"}}`,
		`{"target":{"type":"browser","url":"https://example.test"},"threadId":"another-thread"}`,
		`{"target":{"type":"browser","url":"https://example.test"},"threadId":null}`,
		`{"target":{"type":"browser","tabId":"existing"}}`,
	} {
		var args map[string]any
		_ = json.Unmarshal([]byte(raw), &args)
		if nav := browserNavigation("codex_app", "open_in_codex", args, nil); nav != nil {
			t.Fatalf("ambiguous/foreign target opened browser: %s", raw)
		}
	}
	for _, kind := range []string{"back", "forward", "reload"} {
		if nav := browserNavigation("chrome-devtools", "navigate_page", map[string]any{"type": kind, "url": "https://example.test"}, nil); nav != nil {
			t.Fatal("non-URL navigation must not reuse a supplied URL", kind)
		}
	}
	for _, item := range []cxItem{
		{Type: "agentMessage", ID: "text", Text: "Opened https://example.test"},
		{Type: "webSearch", ID: "web", Query: "https://example.test"},
		{Type: "commandExecution", ID: "shell", Command: "start https://example.test", Status: "completed"},
	} {
		if event, ok := codexItemEvent("codex:owner", item, true, ""); !ok || event.Browser != nil {
			t.Fatalf("ordinary activity was removed or interpreted as navigation: %+v", event)
		}
	}
}

func TestBrowserNavigationURLBoundaries(t *testing.T) {
	for _, raw := range []string{
		"https://example.test/path?q=one%20two&next=%2F#section", "http://localhost:8083/", "http://127.0.0.1:8080/", "https://[::1]:443/a", "https://例子.测试/页面",
		"https://example.test/" + strings.Repeat("a", 4096-len("https://example.test/")),
	} {
		if !validBrowserURL(raw) {
			t.Errorf("valid URL rejected: %q", raw)
		}
	}
	for _, raw := range []string{
		"", "/relative", "//example.test", "javascript:alert(1)", "file:///tmp/test", "data:text/html,test", "about:blank", "https:example.test", "https:///no-host",
		"https://user:password@example.test/", "https://user@example.test/", "https://example.test:bad/", "https://example.test:/", "https://example.test:0/", "https://example.test:65536/",
		"https://example.test/a b", " https://example.test", "https://example.test/\n", "https://example.test/\u200b", "https://example.test/%0a", "https://example.test/%E2%80%8B",
		"https://example.test/%GG", "https://example.test/?q=%GG", "https://example.test/#%GG", "https://example.test\\evil/", "https://example.test/%5cother",
		"https://-bad.test/", "https://bad..test/", "https://./", "https://999.1.1.1/", "https://[not-an-ip]/",
		"https://example.test/" + strings.Repeat("a", 4097-len("https://example.test/")),
	} {
		if validBrowserURL(raw) {
			t.Errorf("unsafe/malformed URL accepted: %q", raw)
		}
	}
}

func TestBrowserNavigationPageIdentityBoundaries(t *testing.T) {
	for _, raw := range []string{
		`{"tabId":null}`, `{"tabId":true}`, `{"tabId":{}}`, `{"tabId":-1}`, `{"tabId":1.1}`, `{"tabId":9007199254740992}`,
		`{"tabId":""}`, `{"tabId":"tab\n1"}`, `{"tabId":"tab\u200b1"}`, `{"tabId":"tab 1"}`, `{"tabId":"one","page_id":"two"}`,
	} {
		var input map[string]any
		_ = json.Unmarshal([]byte(raw), &input)
		input["url"] = "https://example.test"
		if nav := browserNavigation("playwright", "browser_navigate", input, nil); nav != nil {
			t.Fatalf("malformed identity accepted: %s", raw)
		}
	}
	input := map[string]any{"url": "https://example.test", "tabId": "same-tab"}
	one := browserNavigation("first", "browser_navigate", input, nil)
	two := browserNavigation("second", "browser_navigate", input, nil)
	if one == nil || two == nil || one.PageID == two.PageID {
		t.Fatal("different browser server identities were collapsed")
	}
	input["tabId"] = strings.Repeat("a", 256)
	if browserNavigation("first", "browser_navigate", input, nil) != nil {
		t.Fatal("namespaced pageId exceeded the mobile limit")
	}
}
