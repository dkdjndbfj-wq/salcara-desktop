package agents

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCodexDelegationCategoryRequiresObservedStructuredInput(t *testing.T) {
	for _, tc := range []struct {
		name, content, want string
		delegated           bool
	}{
		{"ordinary-text", `[{"type":"text","text":"sent from ChatGPT or another task"}]`, "sent from ChatGPT or another task", false},
		{"structured-delegation", `[{"type":"text","text":"native wrapper","codexDelegation":{"input":"delegated input","sourceThreadId":"private-source-thread"}}]`, "delegated input", true},
		{"null-extension", `[{"type":"text","text":"ordinary","codexDelegation":null}]`, "ordinary", false},
		{"missing-input", `[{"type":"text","text":"ordinary","codexDelegation":{}}]`, "ordinary", false},
		{"null-input", `[{"type":"text","text":"ordinary","codexDelegation":{"input":null}}]`, "ordinary", false},
		{"wrong-input-type", `[{"type":"text","text":"ordinary","codexDelegation":{"input":4}}]`, "ordinary", false},
		{"case-confusable-input", `[{"type":"text","text":"ordinary","codexDelegation":{"Input":"not schema input"}}]`, "ordinary", false},
		{"not-an-object", `[{"type":"text","text":"ordinary","codexDelegation":[{"input":"not an object"}]}]`, "ordinary", false},
		{"source-only", `[{"type":"text","text":"ordinary","codexDelegation":{"sourceThreadId":"private-source-thread"}}]`, "ordinary", false},
		{"mixed-content", `[{"type":"text","text":"intro"},{"type":"text","text":"native wrapper","codexDelegation":{"input":"delegated"}},{"type":"image"},{"type":"skill","name":"fixture","path":"/fixture/SKILL.md"}]`, "intro\ndelegated\n[图片]", true},
		{"empty-input", `[{"type":"text","text":"intro"},{"type":"text","text":"native wrapper","codexDelegation":{"input":""}}]`, "intro\n", true},
		{"nontext-extension", `[{"type":"image","codexDelegation":{"input":"ignored"}}]`, "[图片]", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clientID := "ChatGPT"
			it := cxItem{Type: "userMessage", ID: "fixture-message", ClientID: &clientID, Content: json.RawMessage(tc.content)}
			itemJSON, err := json.Marshal(it)
			if err != nil {
				t.Fatal(err)
			}
			history := codexHistory("codex:fixture", cxThread{ID: "fixture", Turns: []cxTurn{{ID: "fixture-turn", Status: "completed", Items: []json.RawMessage{itemJSON}}}})
			if len(history) != 2 || history[0].Type != "message" {
				t.Fatalf("history lost the user message: %+v", history)
			}
			for _, completed := range []bool{false, true} {
				event, ok := codexItemEvent("codex:fixture", it, completed, "")
				if !ok || event.Text != tc.want || (event.MessageOrigin != nil) != tc.delegated || event.ClientID != clientID || event.Role != "user" || !event.Final {
					t.Fatalf("incorrect projection (completed=%v): %+v", completed, event)
				}
				if history[0].Text != event.Text || history[0].ClientID != event.ClientID || (history[0].MessageOrigin != nil) != tc.delegated {
					t.Fatalf("live/history projections disagree: live=%+v history=%+v", event, history[0])
				}
				if event.MessageOrigin != nil && event.MessageOrigin.Kind != "delegation" {
					t.Fatal("structured category invented a sender type")
				}
				encoded, err := json.Marshal(event)
				if err != nil || strings.Contains(string(encoded), "sourceThreadId") || strings.Contains(string(encoded), "private-source-thread") {
					t.Fatal("unverified source field escaped the adapter")
				}
			}
		})
	}
}

func TestCodexDelegationInputAndCombinedMessageAreBounded(t *testing.T) {
	for _, tc := range []struct {
		name, prefix string
		input        string
		want         string
	}{
		{"exact-boundary", "", strings.Repeat("x", 20000), strings.Repeat("x", 20000)},
		{"oversized-input", "", strings.Repeat("x", 20001), strings.Repeat("x", 20000)},
		{"combined-input", "intro", strings.Repeat("x", 20000), "intro\n" + strings.Repeat("x", 19994)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parts := []map[string]any{}
			if tc.prefix != "" {
				parts = append(parts, map[string]any{"type": "text", "text": tc.prefix})
			}
			parts = append(parts, map[string]any{"type": "text", "text": "native wrapper", "codexDelegation": map[string]any{"input": tc.input, "sourceThreadId": "private-source-thread"}})
			content, err := json.Marshal(parts)
			if err != nil {
				t.Fatal(err)
			}
			text, origin := codexUserInputProjection(content)
			if text != tc.want || len(text) != 20000 || origin == nil || origin.Kind != "delegation" {
				t.Fatalf("unbounded or incorrect delegation projection: len=%d origin=%+v", len(text), origin)
			}
		})
	}
	ordinary, _ := json.Marshal([]map[string]string{{"type": "text", "text": strings.Repeat("x", 20001)}})
	text, origin := codexUserInputProjection(ordinary)
	if len(text) != 20001 || origin != nil {
		t.Fatal("ordinary user text behavior changed while mapping delegation")
	}
}

func TestCodexDelegationParserRejectsMalformedExtension(t *testing.T) {
	for _, raw := range []string{"", "null", `"input"`, `{`, `{"input":[]}`, `{"input":{}}`, `{"input":"ok"} true`} {
		if input, ok := codexDelegationInput(json.RawMessage(raw)); ok || input != "" {
			t.Fatalf("malformed extension accepted: %q", raw)
		}
	}
}
