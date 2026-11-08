package agents

import (
	"encoding/json"
	"testing"

	"salcara/bridge/internal/protocol"
)

func TestCodexRetryAtSurvivesLiveCompletionAndHistory(t *testing.T) {
	detail := `{"error":{"resets_at":1794016860}}`
	errValue := cxTurnError{Message: "quota exhausted", AdditionalDetails: &detail, CodexErrorInfo: json.RawMessage(`"usageLimitExceeded"`)}
	var events []protocol.Event
	a := newCodexAgent(func(e protocol.Event) { events = append(events, e) }, func() Settings { return Settings{} }, "", "")
	defer a.thr.Close()
	params, _ := json.Marshal(map[string]any{"threadId": "target", "turnId": "turn", "willRetry": true, "error": errValue})
	a.onNotify("error", params)
	if len(events) != 1 || events[0].RetryAt != 1794016860000 || !events[0].Retrying {
		t.Fatalf("live error lost explicit timestamp: %+v", events)
	}
	events = nil
	params, _ = json.Marshal(map[string]any{"threadId": "target", "turn": cxTurn{ID: "turn", Status: "failed", Error: &errValue}})
	a.onNotify("turn/completed", params)
	if len(events) != 2 || events[0].RetryAt != 1794016860000 || events[0].Status != "failed" {
		t.Fatalf("completion lost explicit timestamp: %+v", events)
	}
	history := codexHistory("codex:target", cxThread{Turns: []cxTurn{{ID: "turn", Status: "failed", Error: &errValue}}})
	if len(history) != 1 || history[0].RetryAt != 1794016860000 {
		t.Fatalf("history lost absolute timestamp: %+v", history)
	}
	detail = `{"retry_after":60}`
	completedAt, startedAt := int64(1794016800), int64(1794016700)
	history = codexHistory("codex:target", cxThread{Turns: []cxTurn{{ID: "turn", Status: "failed", Error: &errValue, StartedAt: &startedAt, CompletedAt: &completedAt}}})
	if history[0].RetryAt != 1794016860000 {
		t.Fatalf("relative metadata used the wrong event anchor: %+v", history)
	}
	history = codexHistory("codex:target", cxThread{UpdatedAt: completedAt, Turns: []cxTurn{{ID: "turn", Status: "failed", Error: &errValue, StartedAt: &startedAt}}})
	if history[0].RetryAt != 0 {
		t.Fatalf("missing failure time was guessed: %+v", history)
	}
}
