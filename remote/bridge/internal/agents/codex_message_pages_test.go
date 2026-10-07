package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"salcara/bridge/internal/protocol"
)

type pagedCodexTransport struct {
	conn      *rpcConn
	turns     []cxTurn // newest first
	code      int64
	fullReads int
	calls     []string
	params    []map[string]any
}

func (s *pagedCodexTransport) Close() error { return nil }
func (s *pagedCodexTransport) Write(data []byte) (int, error) {
	var request struct {
		ID     int64          `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(data, &request); err != nil {
		return 0, err
	}
	s.calls = append(s.calls, request.Method)
	s.params = append(s.params, request.Params)
	response := map[string]any{"id": request.ID}
	switch request.Method {
	case "thread/read":
		thread := cxThread{ID: "original", Cwd: "fixture", UpdatedAt: 1700000000}
		if request.Params["includeTurns"] == true {
			s.fullReads++
			thread.Turns = append([]cxTurn{}, s.turns...)
			slices.Reverse(thread.Turns)
		}
		response["result"] = map[string]any{"thread": thread}
	case "thread/turns/list":
		if s.code != 0 {
			response["error"] = map[string]any{"code": s.code, "message": "synthetic"}
			break
		}
		start := 0
		if value, ok := request.Params["cursor"].(string); ok {
			for index, turn := range s.turns {
				if turn.ID == value {
					start = index + 1
					break
				}
			}
		}
		limit := int(request.Params["limit"].(float64))
		end := min(start+limit, len(s.turns))
		next := ""
		if end < len(s.turns) {
			next = s.turns[end-1].ID
		}
		response["result"] = map[string]any{"data": s.turns[start:end], "nextCursor": next}
	default:
		return 0, fmt.Errorf("unexpected fixture method %s", request.Method)
	}
	raw, _ := json.Marshal(response)
	s.conn.handleLine(raw)
	return len(data), nil
}

func codexPageFixture(t *testing.T) (*codexAgent, *pagedCodexTransport, *recorder) {
	t.Helper()
	a, _, rec := newResumeSafetyAgent(t, cxThread{}, false)
	transport := &pagedCodexTransport{conn: a.conn}
	a.conn.stdin = transport
	for i := 8; i >= 0; i-- {
		turn := cxTurn{ID: fmt.Sprint("turn", i), Status: "completed"}
		for j := 0; j < 3; j++ {
			raw, _ := json.Marshal(map[string]any{"type": "agentMessage", "id": fmt.Sprintf("m%d-%d", i, j), "text": "fixture"})
			turn.Items = append(turn.Items, raw)
		}
		transport.turns = append(transport.turns, turn)
	}
	return a, transport, rec
}

func TestCodexOfficialMessagePagingNeverReadsFullHistoryAndRetainsInsideTurn(t *testing.T) {
	a, transport, _ := codexPageFixture(t)
	_, page, cursor, err := a.OpenMessagesPage(context.Background(), "original", "", 2)
	if err != nil || len(page) != 2 || cursor == "" || transport.params[1]["itemsView"] != "full" {
		t.Fatal(len(page), err)
	}
	seen := map[string]bool{}
	for _, event := range page {
		seen[event.ID] = true
	}
	// Host cursor anchors are turn IDs, so appends do not move older pages.
	for cursor != "" {
		_, page, cursor, err = a.OpenMessagesPage(context.Background(), "original", cursor, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range page {
			if seen[event.ID] {
				t.Fatal("duplicate", event.ID)
			}
			seen[event.ID] = true
		}
	}
	if len(seen) != 27 || transport.fullReads != 0 {
		t.Fatal("missing history or full read", len(seen), transport.fullReads)
	}
}

func TestCodexHistoryFallbackIsOnlyForDefiniteUnsupportedMethod(t *testing.T) {
	for _, code := range []int64{-32601, -32602, -32603, -32000} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			a, transport, _ := codexPageFixture(t)
			transport.code = code
			_, page, cursor, err := a.OpenMessagesPage(context.Background(), "original", "", 2)
			if code == -32601 || code == -32602 {
				if err != nil || len(page) != 2 || cursor == "" || transport.fullReads != 1 {
					t.Fatal("legacy fallback failed", err)
				}
				_, _, _, err = a.OpenMessagesPage(context.Background(), "original", cursor, 10)
				if err != nil || transport.fullReads != 2 {
					t.Fatal("legacy cursor mode changed", err)
				}
			} else if err == nil || transport.fullReads != 0 {
				t.Fatal("unproven failure retried")
			}
		})
	}
}

func TestCodexTailWatcherDoesNotReplayOldHistoryAfterSmallInitialRead(t *testing.T) {
	a, transport, rec := codexPageFixture(t)
	if _, _, _, err := a.OpenMessagesPage(context.Background(), "original", "", 2); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"id": "new-item", "type": "agentMessage", "text": "new answer"})
	transport.turns[0].Items = append(transport.turns[0].Items, raw)
	a.streamNewItems(context.Background(), "original")
	var messages []protocol.Event
	for _, event := range rec.all() {
		if event.Type == "message" {
			messages = append(messages, event)
		}
	}
	if len(messages) != 1 || messages[0].ID != "new-item" || transport.fullReads != 0 {
		t.Fatal("old history replayed", messages)
	}
}
