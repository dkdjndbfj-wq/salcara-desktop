package agents

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"salcara/bridge/internal/protocol"
)

func TestUnconfirmedWorkerExitBlocksAPISwitchWithoutActiveTurn(t *testing.T) {
	c := &rpcConn{processWaitDone: make(chan struct{})}
	c.treeTermination.begin()
	codex := &codexAgent{unconfirmedProcess: c}
	if !codex.ActiveRemoteTurns() {
		t.Fatal("retired Codex process with unconfirmed exit allowed API replacement")
	}
	p := &claudeProc{}
	p.treeTermination.begin()
	claude := &claudeAgent{workers: map[*claudeProc]struct{}{p: {}}}
	if !claude.ActiveRemoteTurns() {
		t.Fatal("retired Claude process with unconfirmed exit allowed API replacement")
	}
}

func TestPendingApprovalAPISwitchGuardsWithoutActiveProcess(t *testing.T) {
	for _, tool := range []string{"codex", "claude"} {
		t.Run(tool, func(t *testing.T) {
			registry := newApprovals()
			t.Cleanup(registry.cancelAll)
			var active func() bool
			if tool == "codex" {
				adapter := &codexAgent{aps: registry, threads: map[string]*codexThread{}}
				active = adapter.ActiveRemoteTurns
			} else {
				adapter := &claudeAgent{aps: registry, procs: map[string]*claudeProc{}}
				active = adapter.ActiveRemoteTurns
			}
			if active() {
				t.Fatal("empty adapter is busy")
			}
			pending, _ := registry.addRequest(tool+":idle", tool, "synthetic-request", protocol.Event{Kind: "command"})
			if !active() {
				t.Fatal("pending-only approval did not protect API/station change")
			}
			if !registry.answerForSession(tool+":idle", pending.id, approvalAnswer{decision: "allow"}) || active() {
				t.Fatal("answered request kept the idle adapter busy")
			}
			registry.addRequest(tool+":idle", tool, "synthetic-revoke", protocol.Event{Kind: "command"})
			registry.resolveByRef("synthetic-revoke")
			if active() {
				t.Fatal("withdrawn request kept the idle adapter busy")
			}
			registry.addRequest(tool+":idle", tool, "synthetic-expired", protocol.Event{Kind: "command", ExpiresAt: time.Now().Add(-time.Minute).UnixMilli()})
			if active() {
				t.Fatal("expired request kept the idle adapter busy")
			}
		})
	}
}

func TestPendingApprovalClaudeMCPResultIdleStillBlocksAPISwitch(t *testing.T) {
	for _, result := range []string{
		`{"type":"result","subtype":"success","result":"Synthetic completion"}`,
		`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"Synthetic failure"}`,
	} {
		t.Run(result, func(t *testing.T) {
			a, rec, lease := newClaudeMCPQuestionFixture(t)
			proc := &claudeProc{id: "fixture-thread", turns: 1, mapper: newClaudeMapper("claude:fixture-thread", "")}
			// This is an in-memory process model only. No executable is launched.
			a.procs[proc.id] = proc
			t.Cleanup(func() {
				a.mu.Lock()
				delete(a.procs, proc.id)
				a.mu.Unlock()
			})
			w, done := startClaudeMCPQuestion(a, claudeMCPFormFixture, lease.token, nil)
			ev := rec.wait(t, "synthetic pending MCP question", func(e protocol.Event) bool { return e.Type == "approval.request" })
			a.handleLine(proc, []byte(result))
			proc.mu.Lock()
			remaining := proc.turns
			proc.mu.Unlock()
			if remaining != 0 || proc.activeSubtasks.Load() {
				t.Fatal("fixture result did not establish idle turn state")
			}
			if !a.ActiveRemoteTurns() {
				t.Fatal("idle result hid the authenticated MCP form still waiting for the phone")
			}
			if !a.RespondWithAnswers(ev.ApprovalID, ApprovalResponse{Decision: "deny"}) {
				t.Fatal("synthetic form could not be answered")
			}
			questionFixtureWait(t, done)
			if claudeMCPQuestionOutput(t, w).Specific.Action != "decline" || a.ActiveRemoteTurns() {
				t.Fatal("answered form left idle adapter blocked")
			}
		})
	}
}

func TestPendingApprovalCodexTerminalStateReleasesBusyGuard(t *testing.T) {
	for _, status := range []string{"completed", "failed", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			a := newCodexAgent(func(protocol.Event) {}, func() Settings { return Settings{} }, "", "")
			t.Cleanup(a.thr.Close)
			t.Cleanup(a.aps.cancelAll)
			a.mu.Lock()
			thread := a.threadLocked("synthetic-thread")
			thread.loaded, thread.turnID = true, "synthetic-turn"
			a.mu.Unlock()
			a.aps.addRequest("codex:synthetic-thread", "codex", "synthetic-rpc", protocol.Event{Kind: "command"})
			payload, _ := json.Marshal(map[string]any{"threadId": "synthetic-thread", "turn": map[string]any{"id": "synthetic-turn", "status": status}})
			a.onNotify("turn/completed", payload)
			if a.ActiveRemoteTurns() || len(a.aps.snapshot("codex:synthetic-thread")) != 0 {
				t.Fatal("existing terminal cleanup left stale approval blocking the adapter")
			}
		})
	}
}

func TestPendingApprovalCodexChangedSettingsReadPreservesExistingConnection(t *testing.T) {
	a, transport, _ := newResumeSafetyAgent(t, cxThread{ID: "original", Status: cxThreadStatus{Type: "idle"}}, false)
	originalConn, originalSig := a.conn, a.connSig
	originalSettings := a.settings()
	changedSettings := originalSettings
	changedSettings.CodexKey = "synthetic-changed-key"
	a.settings = func() Settings { return changedSettings }
	pending, _ := a.aps.addRequest("codex:original", "codex", "synthetic-rpc", protocol.Event{Kind: "question"})
	if conn, err := a.ensure(context.Background()); err == nil || conn != nil {
		t.Fatal("pending-only process accepted changed settings")
	}
	if a.conn != originalConn || a.connSig != originalSig || !originalConn.alive() || len(transport.calls) != 0 || len(a.aps.snapshot("codex:original")) != 1 {
		t.Fatal("read/catalog configuration check restarted the process or consumed its pending question")
	}
	// After withdrawal the normal no-change read path is available. A real
	// changed-settings restart is covered by the existing fake-executable test.
	a.aps.resolveByRef("synthetic-rpc")
	if a.aps.answer(pending.id, approvalAnswer{decision: "deny"}) {
		t.Fatal("withdrawn request was revived")
	}
	a.settings = func() Settings { return originalSettings }
	if conn, err := a.ensure(context.Background()); err != nil || conn != originalConn || a.ActiveRemoteTurns() {
		t.Fatalf("released request kept the read path blocked: %v", err)
	}
}
