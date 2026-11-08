package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"salcara/bridge/internal/protocol"
)

func TestCodexTypedErrorsRetainConditionAcrossLiveHistoryAndRPC(t *testing.T) {
	for _, tc := range []struct{ raw, code string }{
		{`"contextWindowExceeded"`, "context_limit"},
		{`"sessionBudgetExceeded"`, "session_budget"},
		{`"usageLimitExceeded"`, "usage_limit"},
		{`{"responseStreamDisconnected":{"httpStatusCode":null}}`, "response_stream_interrupted"},
		{`{"responseTooManyFailedAttempts":{"httpStatusCode":null}}`, "response_retry_exhausted"},
		{`{"httpConnectionFailed":{"httpStatusCode":429}}`, "upstream_rate_limit"},
		{`"unauthorized"`, "upstream_unauthorized"},
		{`"futureCondition"`, "unknown_execution_error"},
		{`{"httpConnectionFailed":null}`, "unknown_execution_error"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			var events []protocol.Event
			a := newCodexAgent(func(e protocol.Event) { events = append(events, e) }, func() Settings { return Settings{} }, "", "")
			defer a.thr.Close()
			errData := `{"message":"Selected model is at capacity, invalid Key","codexErrorInfo":` + tc.raw + `}`
			a.onNotify("error", json.RawMessage(`{"threadId":"target","turnId":"turn","willRetry":true,"error":`+errData+`}`))
			if len(events) != 1 || !events[0].Retrying || events[0].ErrorCode != tc.code || events[0].SessionKey != "codex:target" || events[0].TurnID != "turn" {
				t.Fatalf("live error lost provenance/retry/condition: %+v", events)
			}
			var turnError cxTurnError
			if err := json.Unmarshal([]byte(errData), &turnError); err != nil {
				t.Fatal(err)
			}
			history := codexHistory("codex:target", cxThread{Turns: []cxTurn{{ID: "turn", Status: "failed", Error: &turnError}}})
			if len(history) != 1 || history[0].ErrorCode != tc.code || history[0].Retrying || history[0].Status != "failed" {
				t.Fatalf("terminal history is not live retry state: %+v", history)
			}
			if code := protocol.ExecutionErrorCode(fmt.Errorf("request: %w", &rpcError{Message: "Selected model is at capacity", Data: json.RawMessage(errData)})); code != tc.code {
				t.Fatalf("wrapped RPC condition lost: %q", code)
			}
		})
	}
}

func TestCodexUnknownItemsAndStatesDoNotInventCompletionOrExposePayload(t *testing.T) {
	secret := "fixture-private-value"
	for _, item := range []cxItem{
		{Type: "futureTool", ID: "future", Arguments: json.RawMessage(`{"private":"` + secret + `"}`), Result: json.RawMessage(`"` + secret + `"`), Text: secret, Path: secret},
		{Type: "imageGeneration", ID: "future-image", Status: "unknownFutureStatus", SavedPath: secret, Result: json.RawMessage(`"` + secret + `"`)},
	} {
		event, ok := codexItemEvent("codex:target", item, true, "")
		encoded, _ := json.Marshal(event)
		if !ok || event.Type != "notice" || event.ErrorCode != "unsupported_item" || event.Status != "" || strings.Contains(string(encoded), secret) {
			t.Fatalf("unknown item was silently dropped or exposed: %+v", event)
		}
	}
	for _, status := range []string{"futureTerminal", "", "inProgress", "completed"} {
		duration := int64(100)
		history := codexHistory("codex:target", cxThread{Turns: []cxTurn{{ID: "turn", Status: status, DurationMS: &duration}}})
		switch status {
		case "futureTerminal":
			if len(history) != 1 || history[0].Type != "notice" || history[0].ErrorCode != "unsupported_turn_state" {
				t.Fatalf("unknown terminal invented completion: %+v", history)
			}
		case "":
			if len(history) != 0 {
				t.Fatalf("missing legacy status invented terminal: %+v", history)
			}
		case "inProgress":
			if len(history) != 1 || history[0].Status != "started" {
				t.Fatalf("duration invented completion: %+v", history)
			}
		case "completed":
			if len(history) != 1 || history[0].Status != "completed" {
				t.Fatalf("known completed history lost: %+v", history)
			}
		}
	}
	if history := codexHistory("codex:target", cxThread{Turns: []cxTurn{{ID: "turn", Status: "completed"}}}); len(history) != 1 || history[0].Status != "completed" {
		t.Fatalf("explicit completion incorrectly required duration: %+v", history)
	}
}

type conversationInterruptTransport struct {
	conn *rpcConn
	call map[string]string
}

func (*conversationInterruptTransport) Close() error { return nil }
func (s *conversationInterruptTransport) Write(data []byte) (int, error) {
	var req struct {
		ID     int64             `json:"id"`
		Method string            `json:"method"`
		Params map[string]string `json:"params"`
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return 0, err
	}
	if req.Method != "turn/interrupt" {
		return 0, fmt.Errorf("unexpected synthetic request %s", req.Method)
	}
	s.call = req.Params
	reply, _ := json.Marshal(map[string]any{"id": req.ID, "result": map[string]any{}})
	s.conn.handleLine(reply)
	return len(data), nil
}

func TestCodexUnknownTerminalKeepsActualTurnInterruptible(t *testing.T) {
	a, _, rec := newResumeSafetyAgent(t, cxThread{}, false)
	target := a.threadLocked("target")
	target.info.Status, target.turnID, target.loaded = "running", "active-turn", true
	version := target.turnVersion
	a.onNotify("turn/completed", json.RawMessage(`{"threadId":"target","turn":{"id":"active-turn","status":"futureTerminal"}}`))
	if target.turnID != "active-turn" || target.info.Status != "running" || target.turnVersion != version || rec.count(func(e protocol.Event) bool { return e.Type == "turn" }) != 0 {
		t.Fatalf("unrecognized completion cleared real work: %+v", target)
	}
	transport := &conversationInterruptTransport{conn: a.conn}
	a.conn.stdin = transport
	if _, err := a.InterruptTurn(context.Background(), "target", "active-turn", "10000000-0000-4000-8000-000000000004"); err != nil || transport.call["threadId"] != "target" || transport.call["turnId"] != "active-turn" {
		t.Fatalf("stop lost actual turn identity: %v %+v", err, transport.call)
	}
}

func TestCodexMalformedReasoningIndexIsBounded(t *testing.T) {
	for _, method := range []string{"item/reasoning/summaryTextDelta", "item/reasoning/textDelta", "item/reasoning/summaryPartAdded"} {
		for _, index := range []string{"-1", "256", "1073741824", "9223372036854775808", `"invalid"`, "1.5"} {
			var events []protocol.Event
			a := newCodexAgent(func(e protocol.Event) { events = append(events, e) }, func() Settings { return Settings{} }, "", "")
			params := fmt.Sprintf(`{"threadId":"target","turnId":"turn","itemId":"reason","summaryIndex":%s,"contentIndex":%s,"delta":"private should not project"}`, index, index)
			a.onNotify(method, json.RawMessage(params))
			a.thr.Close()
			if len(a.threads) != 0 || len(events) != 1 || events[0].ErrorCode != "unsupported_item" || strings.Contains(events[0].Text, "private") {
				t.Fatalf("index allocated state or leaked malformed delta: %+v %+v", a.threads, events)
			}
		}
	}
}

func TestCodexGlobalErrorDoesNotInventConversation(t *testing.T) {
	var events []protocol.Event
	a := newCodexAgent(func(e protocol.Event) { events = append(events, e) }, func() Settings { return Settings{} }, "", "")
	defer a.thr.Close()
	for _, raw := range []string{`{"error":{"message":"global"}}`, `{"threadId":"../other","error":{"message":"bad target"}}`} {
		a.onNotify("error", json.RawMessage(raw))
	}
	if len(events) != 0 || len(a.threads) != 0 {
		t.Fatalf("global error attributed to invented conversation: %+v", events)
	}
}
