package agents

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"salcara/bridge/internal/protocol"
)

func TestCodexUnknownThreadStatusCannotResumeAnExternalConversation(t *testing.T) {
	thread := cxThread{ID: "fixture-original", Status: cxThreadStatus{Type: "futureBusyState"}}
	a, transport, rec := newResumeSafetyAgent(t, thread, false)
	err := a.Send(context.Background(), thread.ID, "must not be sent")
	if err == nil || !strings.Contains(err.Error(), "无法确认原 Codex 会话状态") || !reflect.DeepEqual(transport.calls, []string{"thread/read"}) {
		t.Fatalf("unknown state allowed a mutating RPC: %v %v", err, transport.calls)
	}
	info := a.threads[thread.ID].info
	if !a.threads[thread.ID].statusUnconfirmed || info.Status == "idle" || info.Controllable || rec.count(func(e protocol.Event) bool { return e.Type == "turn" }) != 0 {
		t.Fatalf("unknown external status became idle/completed/controllable: %+v", info)
	}
}

func TestCodexKnownIdleAndNotLoadedRecoverUnknownStateWithoutNewPolicy(t *testing.T) {
	for _, status := range []string{"idle", "notLoaded"} {
		t.Run(status, func(t *testing.T) {
			thread := cxThread{ID: "fixture-original", Status: cxThreadStatus{Type: status}}
			a, transport, _ := newResumeSafetyAgent(t, thread, false)
			a.applyThreadLocked(cxThread{ID: thread.ID, Status: cxThreadStatus{Type: "futureState"}})
			if err := a.Send(context.Background(), thread.ID, "safe fixture"); err != nil {
				t.Fatal("explicit known idle state did not recover existing continuation", err)
			}
			if !reflect.DeepEqual(transport.calls, []string{"thread/read", "config/read", "thread/resume", "config/read", "turn/start"}) || a.threads[thread.ID].statusUnconfirmed || !a.threads[thread.ID].info.Controllable {
				t.Fatalf("known idle/notLoaded semantics changed: %v %+v", transport.calls, a.threads[thread.ID].info)
			}
		})
	}
}

func TestCodexMissingLegacySnapshotCannotConfirmEarlierUnknownStatus(t *testing.T) {
	a, _, _ := newResumeSafetyAgent(t, cxThread{}, false)
	unknown := a.applyThreadLocked(cxThread{ID: "fixture-original", Status: cxThreadStatus{Type: "futureState"}})
	unknown = a.applyThreadLocked(cxThread{ID: "fixture-original"})
	if !unknown.statusUnconfirmed || unknown.info.Status == "idle" || unknown.info.Controllable {
		t.Fatalf("partial legacy metadata proved an unknown state idle: %+v", unknown.info)
	}
	legacy := a.applyThreadLocked(cxThread{ID: "fixture-legacy"})
	if legacy.statusUnconfirmed || legacy.info.Status != "idle" || !legacy.info.Controllable {
		t.Fatalf("first old-runtime metadata path changed unnecessarily: %+v", legacy.info)
	}
}

func TestCodexUnknownLoadedThreadBlocksSendAndRestartButKeepsInterrupt(t *testing.T) {
	a, transport, rec := newResumeSafetyAgent(t, cxThread{}, false)
	target := a.threadLocked("target")
	target.loaded, target.turnID, target.info.Status = true, "actual-turn", "running"
	parent := a.threadLocked("parent")
	parent.childStates = map[string]string{"target": "running"}
	version := target.turnVersion
	a.onNotify("thread/status/changed", json.RawMessage(`{"threadId":"target","status":{"type":"futureBusyState"}}`))
	if !target.statusUnconfirmed || target.info.Controllable || target.info.Status != "running" || target.turnID != "actual-turn" || target.turnVersion != version || parent.childStates["target"] != "running" {
		t.Fatalf("unknown notification erased actual work/child state: %+v", target)
	}
	if err := a.Send(context.Background(), "target", "must not send"); err == nil || len(transport.calls) != 0 {
		t.Fatalf("loaded unknown state sent a request: %v %v", err, transport.calls)
	}
	a.mu.Lock()
	busy := a.hasActiveTurnsLocked()
	a.mu.Unlock()
	if !busy || rec.count(func(e protocol.Event) bool { return e.Type == "turn" }) != 0 {
		t.Fatal("unknown loaded runtime became restartable or emitted completion")
	}
	interrupt := &conversationInterruptTransport{conn: a.conn}
	a.conn.stdin = interrupt
	if _, err := a.InterruptTurn(context.Background(), "target", "actual-turn", "10000000-0000-4000-8000-000000000005"); err != nil || interrupt.call["threadId"] != "target" || interrupt.call["turnId"] != "actual-turn" {
		t.Fatalf("interrupt lost existing runtime identity: %v %+v", err, interrupt.call)
	}
	// A later explicit supported idle notification can recover. It still does
	// not clear the actual turn ID; only the genuine turn lifecycle owns that.
	a.onNotify("thread/status/changed", json.RawMessage(`{"threadId":"target","status":{"type":"idle"}}`))
	if target.statusUnconfirmed || !target.info.Controllable || target.info.Status != "idle" || target.turnID != "actual-turn" {
		t.Fatalf("supported status recovery changed ownership/lifecycle: %+v", target)
	}
}

func TestCodexUnknownThreadStatusAlsoGuardsDirectTurnAdmission(t *testing.T) {
	a, transport, _ := newResumeSafetyAgent(t, cxThread{}, false)
	target := a.applyThreadLocked(cxThread{ID: "target", Status: cxThreadStatus{Type: "futureState"}})
	target.loaded = true
	if err := a.startTurn(context.Background(), a.conn, "target", "must not send", TurnOptions{}); err == nil || len(transport.calls) != 0 || len(target.pendingAttachments) != 0 {
		t.Fatalf("direct admission bypassed the observed unknown state: %v %v", err, transport.calls)
	}
}

func TestCodexUnknownThreadStatusNotificationDoesNotInventEmptyTask(t *testing.T) {
	a, _, rec := newResumeSafetyAgent(t, cxThread{}, false)
	for _, raw := range []string{`{"status":{"type":"futureState"}}`, `{"threadId":"../other","status":{"type":"futureState"}}`} {
		a.onNotify("thread/status/changed", json.RawMessage(raw))
	}
	if len(a.threads) != 0 || len(rec.all()) != 0 {
		t.Fatal("invalid thread attribution invented a new task")
	}
}
