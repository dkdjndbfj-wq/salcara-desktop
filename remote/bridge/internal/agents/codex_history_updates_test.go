package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"salcara/bridge/internal/protocol"
)

// This fixture never launches a worker or calls a provider. A delayed reply is
// released asynchronously so a second read can complete first on the same RPC.
type historyUpdateTransport struct {
	conn              *rpcConn
	mu                sync.Mutex
	turns             []cxTurn // newest first
	unsupportedPaging bool
	wrongIdentity     bool
	delayNext         bool
	paused            chan struct{}
	release           chan struct{}
	releaseOnce       sync.Once
	fullReads         int
}

func (*historyUpdateTransport) Close() error { return nil }
func (s *historyUpdateTransport) resume()    { s.releaseOnce.Do(func() { close(s.release) }) }
func (s *historyUpdateTransport) set(turns ...cxTurn) {
	s.mu.Lock()
	s.turns = append([]cxTurn(nil), turns...)
	s.mu.Unlock()
}

func (s *historyUpdateTransport) Write(data []byte) (int, error) {
	var req struct {
		ID     int64          `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return 0, err
	}
	s.mu.Lock()
	response := map[string]any{"id": req.ID}
	delayed := false
	thread := cxThread{ID: "original", Cwd: "fixture", UpdatedAt: 1700000000, Status: cxThreadStatus{Type: "idle"}}
	if len(s.turns) > 0 && s.turns[0].Status == "inProgress" {
		thread.Status.Type = "active"
	}
	if s.wrongIdentity {
		thread.ID = "other"
	}
	switch req.Method {
	case "thread/list":
		response["result"] = map[string]any{"data": []cxThread{thread}}
	case "thread/read":
		if req.Params["includeTurns"] == true {
			s.fullReads++
			thread.Turns = append([]cxTurn(nil), s.turns...)
			slices.Reverse(thread.Turns)
		}
		response["result"] = map[string]any{"thread": thread}
	case "thread/turns/list":
		if s.unsupportedPaging {
			response["error"] = map[string]any{"code": -32601, "message": "fixture unsupported paging"}
			break
		}
		start := 0
		if cursor, ok := req.Params["cursor"].(string); ok {
			for i, turn := range s.turns {
				if turn.ID == cursor {
					start = i + 1
					break
				}
			}
		}
		limit := int(req.Params["limit"].(float64))
		end := min(start+limit, len(s.turns))
		next := ""
		if end < len(s.turns) {
			next = s.turns[end-1].ID
		}
		response["result"] = map[string]any{"data": s.turns[start:end], "nextCursor": next}
		if s.delayNext {
			s.delayNext = false
			delayed = true
		}
	default:
		s.mu.Unlock()
		return 0, fmt.Errorf("unexpected read-only synthetic method %s", req.Method)
	}
	raw, err := json.Marshal(response)
	s.mu.Unlock()
	if err != nil {
		return 0, err
	}
	if delayed {
		close(s.paused)
		go func() { <-s.release; s.conn.handleLine(raw) }()
	} else {
		s.conn.handleLine(raw)
	}
	return len(data), nil
}

func updateHistoryFixture(t *testing.T) (*codexAgent, *historyUpdateTransport, *recorder) {
	t.Helper()
	a, _, rec := newResumeSafetyAgent(t, cxThread{}, false)
	transport := &historyUpdateTransport{conn: a.conn, paused: make(chan struct{}), release: make(chan struct{})}
	a.conn.stdin = transport
	t.Cleanup(transport.resume)
	return a, transport, rec
}

func updateHistoryTurn(t *testing.T, status, text, phase, toolStatus, output string) cxTurn {
	t.Helper()
	turn := cxTurn{ID: "current", Status: status}
	for _, item := range []map[string]any{
		{"type": "userMessage", "id": "prompt", "clientId": "client-fixture", "content": []map[string]string{{"type": "text", "text": "task"}}},
		{"type": "agentMessage", "id": "answer", "text": text, "phase": phase},
		{"type": "commandExecution", "id": "tool", "command": "fixture", "status": toolStatus, "aggregatedOutput": output},
	} {
		raw, err := json.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		turn.Items = append(turn.Items, raw)
	}
	return turn
}

func TestCodexTailWatcherUpdatesExistingItemsPhaseAndTerminalExactlyOnce(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint("legacy=", legacy), func(t *testing.T) {
			a, transport, rec := updateHistoryFixture(t)
			transport.unsupportedPaging = legacy
			transport.set(updateHistoryTurn(t, "inProgress", "partial", "commentary", "inProgress", "part"))
			if _, _, _, err := a.OpenMessagesPage(context.Background(), "original", "", 2); err != nil {
				t.Fatal(err)
			}
			transport.set(updateHistoryTurn(t, "completed", "complete answer", "final_answer", "completed", "complete output"))
			a.streamNewItems(context.Background(), "original")
			events := rec.all()
			if len(events) != 3 || events[0].Type != "message" || events[0].ID != "answer" || events[0].Text != "complete answer" || events[0].MessagePhase != "final_answer" || events[1].Output != "complete output" || events[1].Status != "done" || events[2].Type != "turn" || events[2].ID != "turn:current" || events[2].TurnID != "current" || events[2].Status != "completed" {
				t.Fatalf("updated snapshot was skipped or polluted: %+v", events)
			}
			a.streamNewItems(context.Background(), "original")
			if got := len(rec.all()); got != len(events) {
				t.Fatalf("unchanged snapshot replayed: %d -> %d", len(events), got)
			}
			if !legacy && transport.fullReads != 0 {
				t.Fatal("paged watcher did a full history read")
			}
		})
	}
}

func TestCodexTailWatcherStableItemIDsSurviveInsertion(t *testing.T) {
	a, transport, rec := updateHistoryFixture(t)
	initial := updateHistoryTurn(t, "inProgress", "same answer", "commentary", "inProgress", "same output")
	transport.set(initial)
	if _, _, _, err := a.OpenMessagesPage(context.Background(), "original", "", 2); err != nil {
		t.Fatal(err)
	}
	inserted := json.RawMessage(`{"type":"agentMessage","id":"inserted","text":"new item"}`)
	initial.Items = append([]json.RawMessage{inserted}, initial.Items...)
	transport.set(initial)
	a.streamNewItems(context.Background(), "original")
	if events := rec.all(); len(events) != 1 || events[0].ID != "inserted" || events[0].Text != "new item" {
		t.Fatalf("index-based identity duplicated old items: %+v", events)
	}
}

func TestCodexLateTailReplyCannotRevertNewerOrReopenedHistory(t *testing.T) {
	for _, mode := range []string{"newer-read", "reopen", "process-unload"} {
		t.Run(mode, func(t *testing.T) {
			a, transport, rec := updateHistoryFixture(t)
			transport.set(updateHistoryTurn(t, "inProgress", "initial", "commentary", "inProgress", "initial"))
			if _, _, _, err := a.OpenMessagesPage(context.Background(), "original", "", 2); err != nil {
				t.Fatal(err)
			}
			transport.set(updateHistoryTurn(t, "inProgress", "late old answer", "commentary", "inProgress", "old"))
			transport.mu.Lock()
			transport.delayNext = true
			transport.mu.Unlock()
			done := make(chan struct{})
			go func() { a.streamNewItems(context.Background(), "original"); close(done) }()
			select {
			case <-transport.paused:
			case <-time.After(time.Second):
				t.Fatal("old synthetic read did not pause")
			}
			transport.set(updateHistoryTurn(t, "completed", "new answer", "final_answer", "completed", "new"))
			if mode == "reopen" {
				if _, _, _, err := a.OpenMessagesPage(context.Background(), "original", "", 2); err != nil {
					t.Fatal(err)
				}
			} else if mode == "process-unload" {
				a.mu.Lock()
				a.markUnloadedLocked()
				a.mu.Unlock()
			} else {
				a.streamNewItems(context.Background(), "original")
			}
			transport.resume()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("old synthetic read did not complete")
			}
			for _, event := range rec.all() {
				if strings.Contains(event.Text, "late old") || event.Output == "old" || mode != "newer-read" {
					t.Fatalf("late read reverted a newer snapshot: %+v", rec.all())
				}
			}
			if mode == "newer-read" && rec.count(func(e protocol.Event) bool { return e.Type == "turn" && e.Status == "completed" }) != 1 {
				t.Fatal("new real completion was lost")
			}
		})
	}
}

func TestCodexStatusOnlyPollStillBackfillsFinalAnswer(t *testing.T) {
	a, transport, rec := updateHistoryFixture(t)
	transport.set(updateHistoryTurn(t, "inProgress", "partial", "commentary", "inProgress", "part"))
	info, _, _, err := a.OpenMessagesPage(context.Background(), "original", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	prev := map[string]protocol.SessionInfo{info.SessionKey: info}
	transport.set(updateHistoryTurn(t, "completed", "complete answer", "final_answer", "completed", "complete output"))
	a.pollOnce(context.Background(), prev, false)
	if rec.count(func(e protocol.Event) bool {
		return e.Type == "message" && e.Text == "complete answer" && e.MessagePhase == "final_answer"
	}) != 1 || rec.count(func(e protocol.Event) bool { return e.Type == "turn" && e.Status == "completed" }) != 1 {
		t.Fatalf("unchanged updatedAt lost final state transition: %+v", rec.all())
	}
}

func TestCodexTailDigestBudgetsAndMissingBoundaryRequestResnapshot(t *testing.T) {
	tooMany := cxTurn{ID: "large", Status: "inProgress"}
	for i := 0; i < codexTailItemLimit+1; i++ {
		tooMany.Items = append(tooMany.Items, json.RawMessage(fmt.Sprintf(`{"type":"agentMessage","id":"m%d","text":"fixture"}`, i)))
	}
	items, states, ok := codexHistoryDigests([]cxTurn{tooMany})
	if ok || len(items) > codexTailItemLimit || len(states) > 100 {
		t.Fatal("unbounded item fingerprint index", len(items), len(states), ok)
	}
	raw, _ := json.Marshal(map[string]string{"type": "agentMessage", "id": "large", "text": strings.Repeat("x", codexTailByteLimit+1)})
	if _, _, ok := codexHistoryDigests([]cxTurn{{ID: "large", Items: []json.RawMessage{raw}}}); ok {
		t.Fatal("unbounded byte fingerprint index")
	}
	a, transport, rec := updateHistoryFixture(t)
	transport.set(updateHistoryTurn(t, "inProgress", "partial", "commentary", "inProgress", "part"))
	if _, _, _, err := a.OpenMessagesPage(context.Background(), "original", "", 2); err != nil {
		t.Fatal(err)
	}
	var future []cxTurn
	for i := 101; i >= 0; i-- {
		future = append(future, cxTurn{ID: fmt.Sprintf("new%d", i), Status: "completed"})
	}
	transport.set(future...)
	a.streamNewItems(context.Background(), "original")
	if events := rec.all(); len(events) != 1 || events[0].Type != "notice" || !strings.Contains(events[0].Text, "[salcara:history-gap:v1]") || transport.fullReads != 0 {
		t.Fatalf("missing boundary silently lost history or read everything: %+v", events)
	}
	if tState := a.threads["original"]; len(tState.historyItemDigests) > codexTailItemLimit || len(tState.historyTurnDigests) > 100 {
		t.Fatal("gap retained unbounded state")
	}
}

func TestCodexLegacyReadRejectsDifferentThreadIdentity(t *testing.T) {
	a, transport, _ := updateHistoryFixture(t)
	transport.wrongIdentity = true
	transport.set(updateHistoryTurn(t, "completed", "other answer", "final_answer", "completed", "other"))
	if _, events, err := a.Open(context.Background(), "original"); err == nil || len(events) != 0 {
		t.Fatalf("different identity projected into requested task: %v %+v", err, events)
	}
}
