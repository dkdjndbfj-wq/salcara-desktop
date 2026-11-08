package agents

import (
	"testing"

	"salcara/bridge/internal/protocol"
)

func TestClaudeRetiredReaderCannotPublishFramesIntoSuccessor(t *testing.T) {
	for name, frame := range map[string]string{
		"init":        `{"type":"system","subtype":"init","model":"old-model"}`,
		"compact":     `{"type":"system","subtype":"compact_boundary"}`,
		"task":        `{"type":"system","subtype":"task_started","task_id":"late-task"}`,
		"assistant":   `{"type":"assistant","message":{"id":"old-msg","content":[{"type":"text","text":"old output"}]}}`,
		"child":       `{"type":"assistant","parent_tool_use_id":"old-child","message":{"id":"old-msg","content":[{"type":"text","text":"old child output"}]}}`,
		"tool result": `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"old-tool","content":"old result"}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			a, old, rec := newClaudeResultFixture(t)
			next := claudeSyntheticPermissionWorker(t, a, old.id)
			a.live[next.id] = protocol.SessionInfo{SessionKey: "claude:" + next.id, Status: "idle", Model: "successor-model"}
			a.handleLine(old, []byte(frame))
			if len(rec.all()) != 0 || old.activeSubtasks.Load() || a.live[next.id].Model != "successor-model" || a.live[next.id].Status != "idle" {
				t.Fatal("retired reader changed successor state or emitted stale output")
			}
		})
	}
}

func TestClaudeTaskBusyIsVisibleBeforePublishingTask(t *testing.T) {
	for _, frame := range []string{
		`{"type":"system","subtype":"task_started","task_id":"new-task"}`,
		`{"type":"assistant","message":{"id":"new-message","content":[{"type":"tool_use","id":"task-tool","name":"Task","input":{"description":"synthetic child"}}]}}`,
	} {
		a, p, rec := newClaudeResultFixture(t)
		observed := false
		a.sink = func(e protocol.Event) {
			rec.sink(e)
			if e.Type == "tool" && e.Status == "running" {
				observed = true
				if !a.ActiveRemoteTurns() || !p.activeSubtasks.Load() {
					t.Error("task was published before its busy guard")
				}
				if err := a.retireIdleClaudeWorker(p); err == nil {
					t.Error("published active task was retired as idle")
				}
			}
		}
		a.thr.emit = a.sink
		a.handleLine(p, []byte(frame))
		if !observed {
			t.Fatal("synthetic task did not reach sink")
		}
	}
}

func TestClaudeTaskSinkReplacementCannotOverwriteSuccessorStatus(t *testing.T) {
	a, old, rec := newClaudeResultFixture(t)
	var next *claudeProc
	a.sink = func(e protocol.Event) {
		rec.sink(e)
		if e.Type == "tool" && next == nil {
			next = claudeSyntheticPermissionWorker(t, a, old.id)
			a.live[next.id] = protocol.SessionInfo{SessionKey: "claude:" + next.id, Status: "idle", Model: "successor-model"}
		}
	}
	a.thr.emit = a.sink
	a.handleLine(old, []byte(`{"type":"system","subtype":"task_started","task_id":"synthetic-task"}`))
	if next == nil || a.live[next.id].Status != "idle" || a.live[next.id].Model != "successor-model" {
		t.Fatal("retired task reader overwrote replacement after sink returned")
	}
	for _, e := range rec.all() {
		if e.Type == "session.updated" {
			t.Fatal("retired task reader emitted status for successor")
		}
	}
}

func TestClaudeOwnedStartupInitStillPublishesResolvedModel(t *testing.T) {
	a, p, _ := newClaudeResultFixture(t)
	delete(a.procs, p.id) // spawn owns this generation before registration.
	a.live[p.id] = protocol.SessionInfo{SessionKey: "claude:" + p.id, Model: "requested-alias"}
	a.handleLine(p, []byte(`{"type":"system","subtype":"init","model":"resolved-model"}`))
	if a.live[p.id].Model != "resolved-model" {
		t.Fatal("generation fencing discarded owned startup metadata")
	}
}

func TestClaudeBackgroundCompletionKeepsOriginalContinueTarget(t *testing.T) {
	a, p, _ := newClaudeResultFixture(t)
	turn := sendClaudeResultFixture(t, a, p)
	a.handleLine(p, []byte(`{"type":"system","subtype":"task_started","task_id":"background-task"}`))
	a.handleLine(p, []byte(`{"type":"result","subtype":"success","uuid":"root-result"}`))
	if info := a.live[p.id]; info.Status != "running" || info.ActiveTurnID != "" || info.LastTerminalTurnID != turn {
		t.Fatal("root terminal identity was lost while background task runs")
	}
	a.handleLine(p, []byte(`{"type":"system","subtype":"task_notification","task_id":"background-task","status":"completed"}`))
	if info := a.live[p.id]; info.Status != "idle" || info.LastTerminalTurnID != turn || p.activeSubtasks.Load() {
		t.Fatal("background completion did not retain the actual root continuation target")
	}
}
