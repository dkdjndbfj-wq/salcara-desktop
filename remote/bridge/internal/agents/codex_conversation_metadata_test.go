package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"salcara/bridge/internal/protocol"
)

func TestCodexThreadReportedAgentMetadataIsBoundedAndNotAuthority(t *testing.T) {
	a, _, _ := newResumeSafetyAgent(t, cxThread{}, false)
	nickname, role, parent, fork := strings.Repeat("名", 200), "  explorer  ", "fixture-parent", "fixture-fork"
	thread := cxThread{ID: "fixture-child", ParentThreadID: &parent, ForkedFromID: &fork, AgentNickname: &nickname, AgentRole: &role, Status: cxThreadStatus{Type: "active"}}
	info := a.applyThreadLocked(thread).info
	if len([]rune(info.AgentNickname)) != 160 || info.AgentRole != "explorer" || info.ParentSessionKey != "codex:fixture-parent" || info.ForkedFromSessionKey != "codex:fixture-fork" {
		t.Fatalf("reported metadata was lost or unbounded: %+v", info)
	}
	if info.Controllable || a.threads[thread.ID].loaded {
		t.Fatal("agent metadata made an external active conversation controllable")
	}
	invalid := "../../not-a-session"
	info = a.applyThreadLocked(cxThread{ID: thread.ID, ForkedFromID: &invalid}).info
	if info.ForkedFromSessionKey != "" {
		t.Fatal("invalid source handle was projected")
	}
}

type conversationStartTransport struct {
	conn   *rpcConn
	agent  *codexAgent
	reject bool
	notify string
}

func (*conversationStartTransport) Close() error { return nil }

func (s *conversationStartTransport) Write(data []byte) (int, error) {
	var req struct {
		ID     int64  `json:"id"`
		Method string `json:"method"`
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return 0, err
	}
	response := map[string]any{"id": req.ID}
	switch req.Method {
	case "config/read":
		cfg, _ := guardedProviderConfig(s.agent.settings())
		response["result"] = map[string]any{"config": cfg}
	case "thread/start":
		response["result"] = map[string]any{"thread": cxThread{ID: "fixture-created", Status: cxThreadStatus{Type: "idle"}}}
	case "turn/start":
		if s.notify != "" {
			started, _ := json.Marshal(map[string]any{"threadId": "fixture-created", "turn": cxTurn{ID: "fixture-accepted", Status: "inProgress"}})
			s.agent.onNotify("turn/started", started)
			if s.notify == "completed" {
				completed, _ := json.Marshal(map[string]any{"threadId": "fixture-created", "turn": cxTurn{ID: "fixture-accepted", Status: "completed"}})
				s.agent.onNotify("turn/completed", completed)
			}
		}
		if s.reject {
			response["error"] = map[string]any{"code": -32000, "message": "Selected model is at capacity. Please try a different model."}
		} else {
			response["result"] = map[string]any{"turn": cxTurn{ID: "fixture-accepted", Status: "inProgress"}}
		}
	default:
		return 0, fmt.Errorf("unexpected synthetic RPC %s", req.Method)
	}
	encoded, _ := json.Marshal(response)
	s.conn.handleLine(encoded)
	return len(data), nil
}

func TestCodexSynchronousTurnStartStateDoesNotInventRunningWork(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		reject               bool
		notify               string
		wantStatus, wantTurn string
	}{
		{"capacity-rejection", true, "", "idle", ""},
		{"accepted-no-notification-yet", false, "", "running", "fixture-accepted"},
		{"started-before-reply", false, "started", "running", "fixture-accepted"},
		{"completed-before-reply", false, "completed", "idle", ""},
		{"late-rejection-must-not-stop-accepted-work", true, "started", "running", "fixture-accepted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, rec := newResumeSafetyAgent(t, cxThread{}, false)
			a.conn.stdin = &conversationStartTransport{conn: a.conn, agent: a, reject: tc.reject, notify: tc.notify}
			id, err := a.Start(context.Background(), "fixture-workspace", "synthetic task", "", "ask")
			if id != "fixture-created" || (err != nil) != tc.reject {
				t.Fatalf("unexpected start result %q %v", id, err)
			}
			if tc.reject && protocol.ExecutionErrorCode(err) != "model_capacity" {
				t.Fatalf("capacity classification lost: %v", err)
			}
			state := a.threads[id]
			if state.info.Status != tc.wantStatus || state.turnID != tc.wantTurn {
				t.Fatalf("late/rejected reply changed real state: %+v turn=%q", state.info, state.turnID)
			}
			if tc.notify == "completed" && rec.count(func(e protocol.Event) bool { return e.Type == "turn" && e.Status == "completed" }) != 1 {
				t.Fatal("completion was not retained")
			}
		})
	}
}

func TestCodexStartProjectsWorkspaceModeImmediately(t *testing.T) {
	a, _, rec := newResumeSafetyAgent(t, cxThread{}, false)
	a.conn.stdin = &conversationStartTransport{conn: a.conn, agent: a}

	id, err := a.StartWithOptions(context.Background(), "fixture-workspace", "synthetic task", "ask", TurnOptions{
		WorkspaceMode: protocol.WorkspaceModeWork,
	})
	if err != nil || id != "fixture-created" {
		t.Fatalf("start failed: %q %v", id, err)
	}
	a.mu.Lock()
	mode := a.threads[id].info.WorkspaceMode
	a.mu.Unlock()
	if mode != protocol.WorkspaceModeWork {
		t.Fatalf("workspace mode was missing from immediate session projection: %q", mode)
	}
	updated := rec.wait(t, "session.updated workspace mode", func(event protocol.Event) bool {
		return event.Type == "session.updated" && event.SessionKey == "codex:"+id
	})
	if updated.Session == nil || updated.Session.WorkspaceMode != protocol.WorkspaceModeWork {
		t.Fatalf("live session event lost workspace mode: %+v", updated)
	}
}
