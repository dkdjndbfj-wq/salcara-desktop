package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"salcara/bridge/internal/protocol"
)

func TestCodexCapacityKeepsTypedFailureAndRuntimeRetry(t *testing.T) {
	var events []protocol.Event
	a := newCodexAgent(func(e protocol.Event) { events = append(events, e) }, func() Settings { return Settings{} }, "", "")
	defer a.thr.Close()
	a.onNotify("error", json.RawMessage(`{"threadId":"target","willRetry":true,"error":{"message":"本轮暂时无法服务","codexErrorInfo":"serverOverloaded"}}`))
	a.onNotify("turn/completed", json.RawMessage(`{"threadId":"target","turn":{"id":"turn","status":"failed","error":{"message":"本轮暂时无法服务","codexErrorInfo":"serverOverloaded"}}}`))
	if len(events) != 3 || events[0].Level != "warn" || !events[0].Retrying || events[0].ErrorCode != "model_capacity" || events[1].Status != "failed" || events[1].ErrorCode != "model_capacity" {
		t.Fatalf("capacity mislabeled or lost: %+v", events)
	}
	for _, e := range events {
		if e.SessionKey != "codex:target" || e.Type == "turn" && e.Status == "completed" {
			t.Fatalf("failed capacity invented completion/task: %+v", e)
		}
	}
	var thread cxThread
	_ = json.Unmarshal([]byte(`{"turns":[{"id":"turn","status":"failed","error":{"message":"本轮暂时无法服务","codexErrorInfo":"serverOverloaded"}}]}`), &thread)
	if history := codexHistory("codex:target", thread); len(history) != 1 || history[0].ErrorCode != "model_capacity" || history[0].Status != "failed" {
		t.Fatalf("history lost failure: %+v", history)
	}
}

func TestCodexTurnStartKeepsEchoIdentityAndWrappedErrorCode(t *testing.T) {
	a := newCodexAgent(func(protocol.Event) {}, func() Settings { return Settings{} }, "", "")
	defer a.thr.Close()
	input := &rpcFixtureWriter{calls: make(chan []byte, 1)}
	c := &rpcConn{stdin: input, pending: map[int64]chan rpcResult{}, done: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- a.startTurn(ctx, c, "target", "continue", TurnOptions{ClientUserMessageID: "10000000-0000-4000-8000-000000000001"})
	}()
	var request struct {
		ID     int64          `json:"id"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(<-input.calls, &request); err != nil {
		t.Fatal(err)
	}
	if request.Params["threadId"] != "target" || request.Params["clientUserMessageId"] != "10000000-0000-4000-8000-000000000001" {
		t.Fatalf("turn correlation lost: %+v", request)
	}
	c.mu.Lock()
	pending := c.pending[request.ID]
	c.mu.Unlock()
	pending <- rpcResult{err: &rpcError{Message: "temporarily unavailable", Data: json.RawMessage(`{"codexErrorInfo":"serverOverloaded"}`)}}
	if err := <-result; protocol.ExecutionErrorCode(err) != "model_capacity" {
		t.Fatalf("wrapped RPC condition lost: %v", err)
	}
	if protocol.ExecutionErrorCode(fmt.Errorf("wrapped: %w", &rpcError{Message: "HTTP 429 rate limit"})) != "" {
		t.Fatal("rate limit was classified as a full model")
	}
}

func TestCodexUserClientAndAssistantPhaseStayMetadata(t *testing.T) {
	for _, raw := range []string{
		`{"type":"userMessage","id":"prompt","clientId":"client-fixture","content":[{"type":"text","text":"Message from another task: text is not sender proof"}]}`,
		`{"type":"agentMessage","id":"progress","text":"working","phase":"commentary"}`,
		`{"type":"agentMessage","id":"answer","text":"done","phase":"final_answer"}`,
	} {
		var item cxItem
		_ = json.Unmarshal([]byte(raw), &item)
		e, ok := codexItemEvent("codex:target", item, true, "")
		if !ok || e.SessionKey != "codex:target" || e.ParentID != "" {
			t.Fatalf("metadata invented sender identity: %+v", e)
		}
		if item.Type == "userMessage" && (e.ClientID != "client-fixture" || e.Role != "user") {
			t.Fatalf("client correlation missing: %+v", e)
		}
		if item.Type == "agentMessage" && e.MessagePhase != *item.Phase {
			t.Fatalf("phase missing: %+v", e)
		}
	}
}
