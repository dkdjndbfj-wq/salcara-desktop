package desktopcompanion

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNativeDelegationCategoryComesOnlyFromStructuredPayload(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		delegated     bool
		want          string
	}{
		{"ordinary-text", `[{"type":"text","text":"sent from ChatGPT or another task"}]`, false, "sent from ChatGPT or another task"},
		{"structured-delegation", `[{"type":"text","text":"native wrapper","codexDelegation":{"input":"delegated input","sourceThreadId":"must-not-be-inferred"}}]`, true, "delegated input"},
		{"null-extension", `[{"type":"text","text":"ordinary","codexDelegation":null}]`, false, "ordinary"},
		{"missing-input", `[{"type":"text","text":"ordinary","codexDelegation":{}}]`, false, "ordinary"},
		{"wrong-input-type", `[{"type":"text","text":"ordinary","codexDelegation":{"input":4}}]`, false, "ordinary"},
		{"case-confusable-input", `[{"type":"text","text":"ordinary","codexDelegation":{"Input":"not schema input"}}]`, false, "ordinary"},
		{"mixed-content", `[{"type":"text","text":"intro"},{"type":"text","codexDelegation":{"input":"delegated"}}]`, true, "intro\ndelegated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := map[string]json.RawMessage{"type": json.RawMessage(`"userMessage"`), "id": json.RawMessage(`"fixture-message"`), "clientId": json.RawMessage(`"ChatGPT"`), "content": json.RawMessage(tc.content)}
			events := nativeEvents("codex:fixture", nativeRead{Turns: []nativeTurn{{ID: "fixture-turn", Items: []map[string]json.RawMessage{item}}}})
			if len(events) < 1 || events[0].Text != tc.want || (events[0].MessageOrigin != nil) != tc.delegated {
				t.Fatalf("incorrect origin projection: %+v", events)
			}
			if tc.delegated && events[0].MessageOrigin.Kind != "delegation" {
				t.Fatal("extension invented a sender type")
			}
			encoded, _ := json.Marshal(events[0])
			if strings.Contains(string(encoded), "sourceThreadId") || strings.Contains(string(encoded), "must-not-be-inferred") {
				t.Fatal("unknown extension fields were forwarded")
			}
		})
	}
}

func TestNativeReportedAgentMetadataDoesNotExpandLease(t *testing.T) {
	info := sessionFromNative(nativeThread{ID: targetID, AgentNickname: strings.Repeat("x", 200), AgentRole: " explorer ", ForkedFromID: otherID}, 123)
	if len(info.AgentNickname) != 160 || info.AgentRole != "explorer" || info.ForkedFromSessionKey != "codex:"+otherID || info.ControlExpiresAt != 123 {
		t.Fatalf("incorrect reported metadata: %+v", info)
	}
	info = sessionFromNative(nativeThread{ID: targetID, ForkedFromID: "../../outside"}, 123)
	if info.ForkedFromSessionKey != "" {
		t.Fatal("invalid related task handle was projected")
	}
}

func TestNativeHistoryTerminalErrorsRemainExecutionConditions(t *testing.T) {
	for _, tc := range []struct{ raw, text, code string }{
		{`{"message":"provider busy","codexErrorInfo":"serverOverloaded"}`, "provider busy", "model_capacity"},
		{`"Selected model is at capacity. Please try a different model."`, "Selected model is at capacity. Please try a different model.", "model_capacity"},
		{`{"message":"rate limit reached","codexErrorInfo":"usageLimitExceeded"}`, "rate limit reached", "usage_limit"},
		{`{"message":"Selected model is at capacity, invalid Key","codexErrorInfo":"contextWindowExceeded"}`, "Selected model is at capacity, invalid Key", "context_limit"},
		{`{"message":"stream broke","codexErrorInfo":{"responseStreamDisconnected":{"httpStatusCode":null}}}`, "stream broke", "response_stream_interrupted"},
		{`{"message":"rate limit reached","codexErrorInfo":{"httpConnectionFailed":{"httpStatusCode":429}}}`, "rate limit reached", "upstream_rate_limit"},
		{`{"message":"Selected model is at capacity, invalid Key","codexErrorInfo":"futureCondition"}`, "Selected model is at capacity, invalid Key", "unknown_execution_error"},
		{`null`, "", ""},
		{`{"message":4}`, "", ""},
	} {
		events := nativeEvents("codex:fixture", nativeRead{Turns: []nativeTurn{{ID: "fixture-turn", Status: "failed", Error: json.RawMessage(tc.raw)}}})
		if len(events) != 1 || events[0].Error != tc.text || events[0].ErrorCode != tc.code || events[0].Status != "failed" {
			t.Fatalf("terminal error lost/misclassified: %+v", events)
		}
	}
}

func TestNativeHistoryRetainsReportedPhaseAndAuthoritativePlan(t *testing.T) {
	events := nativeEvents("codex:fixture", nativeRead{Turns: []nativeTurn{{ID: "turn", Status: "completed", Items: []map[string]json.RawMessage{
		{"type": json.RawMessage(`"agentMessage"`), "id": json.RawMessage(`"comment"`), "text": json.RawMessage(`"working"`), "phase": json.RawMessage(`"commentary"`)},
		{"type": json.RawMessage(`"plan"`), "id": json.RawMessage(`"plan"`), "text": json.RawMessage(`""`)},
	}}}})
	if len(events) != 3 || events[0].MessagePhase != "commentary" || !events[0].Final || !events[1].OutputFinal || events[1].Output != "" {
		t.Fatalf("native finality lost: %+v", events)
	}
}
