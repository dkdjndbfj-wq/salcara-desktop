package agents

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"salcara/bridge/internal/protocol"
)

func TestPendingApprovalCodexEveryRequestUsesConnectionLifetime(t *testing.T) {
	requests := []struct{ name, method, body string }{
		{"command", "item/commandExecution/requestApproval", `{"threadId":"fixture-thread","command":"synthetic command"}`},
		{"file", "item/fileChange/requestApproval", `{"threadId":"fixture-thread","reason":"synthetic edit"}`},
		{"permission", "item/permissions/requestApproval", `{"threadId":"fixture-thread","permissions":{"network":{"enabled":true}}}`},
		{"question", "item/tool/requestUserInput", codexQuestionFixture},
		{"MCP", "mcpServer/elicitation/request", `{"threadId":"fixture-thread","serverName":"synthetic-server","mode":"form","message":"Choose settings","requestedSchema":{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}}`},
	}
	for _, request := range requests {
		t.Run(request.name, func(t *testing.T) {
			a, oldConn, rec, oldReplies := newCodexQuestionFixture(t, "ask")
			a.mu.Lock()
			a.conn, a.connSig = oldConn, codexSig(a.settings())
			thread := a.threadLocked("fixture-thread")
			thread.loaded, thread.turnID = true, ""
			a.mu.Unlock()
			t.Cleanup(func() {
				a.mu.Lock()
				a.conn = nil // neither connection owns an OS process
				a.mu.Unlock()
			})
			if a.ActiveRemoteTurns() {
				t.Fatal("synthetic connection did not start idle")
			}
			// A genuine source request arrives after the prior idle admission.
			oldDone := startQuestionRequest(a, oldConn, request.method, request.body)
			oldEvent := rec.wait(t, "old connection approval", func(e protocol.Event) bool { return e.Type == "approval.request" })
			if !a.ActiveRemoteTurns() {
				t.Fatal("late approval did not establish pending-only busy state")
			}
			newCapture := &questionReplyCapture{replies: make(chan rpcMessage, 8)}
			newConn := &rpcConn{stdin: newCapture, pending: map[int64]chan rpcResult{}, done: make(chan struct{})}
			a.mu.Lock()
			a.conn = newConn
			a.mu.Unlock()
			// The session and API may be identical, but the process epoch is not.
			if a.RespondForSession(context.Background(), "codex:fixture-thread", oldEvent.ApprovalID, ApprovalResponse{Decision: "deny"}) {
				t.Fatal("new connection answered an old connection's request")
			}
			oldConn.shutdown(errors.New("synthetic retired connection"))
			if a.ActiveRemoteTurns() || len(a.aps.snapshot("codex:fixture-thread")) != 0 || a.aps.countFor("codex:fixture-thread") != 0 {
				t.Fatal("closed source remained busy or visible while cancellation was draining")
			}
			if a.RespondWithAnswers(oldEvent.ApprovalID, ApprovalResponse{Decision: "deny"}) {
				t.Fatal("unscoped/local answer revived a closed source")
			}
			questionFixtureWait(t, oldDone)
			select {
			case <-oldReplies:
				t.Fatal("retired connection received an approval response")
			default:
			}
			beforeRequests := rec.count(func(e protocol.Event) bool { return e.Type == "approval.request" })
			questionFixtureWait(t, startQuestionRequest(a, oldConn, request.method, request.body))
			if rec.count(func(e protocol.Event) bool { return e.Type == "approval.request" }) != beforeRequests {
				t.Fatal("already retired callback published a new stale approval")
			}
			newDone := startQuestionRequest(a, newConn, request.method, request.body)
			newEvent := rec.wait(t, "current connection approval", func(e protocol.Event) bool {
				return e.Type == "approval.request" && e.ApprovalID != oldEvent.ApprovalID
			})
			if !a.RespondForSession(context.Background(), "codex:fixture-thread", newEvent.ApprovalID, ApprovalResponse{Decision: "deny"}) {
				t.Fatal("current connection lost normal scoped answer support")
			}
			questionFixtureWait(t, newDone)
			_ = questionFixtureReply(t, newCapture.replies)
			if a.ActiveRemoteTurns() || len(a.aps.snapshot("codex:fixture-thread")) != 0 {
				t.Fatal("retiring the old source polluted the current request")
			}
		})
	}
}

func TestPendingApprovalCodexClosedSourceCannotRegister(t *testing.T) {
	a, conn, rec, _ := newCodexQuestionFixture(t, "ask")
	conn.shutdown(errors.New("synthetic closed source"))
	if _, accepted := a.askPhoneRequest("codex:fixture-thread", "synthetic-rpc", protocol.Event{Kind: "command"}, nil, conn.done); accepted {
		t.Fatal("request registered after its source closed")
	}
	if len(a.aps.snapshot("codex:fixture-thread")) != 0 || rec.count(func(e protocol.Event) bool { return e.Type == "approval.request" }) != 0 {
		t.Fatal("closed source created a visible or busy request")
	}
}

func TestPendingApprovalCodexOldDrainCannotConsumeNewSameSessionRequest(t *testing.T) {
	a, oldConn, rec, oldReplies := newCodexQuestionFixture(t, "ask")
	a.mu.Lock()
	a.conn, a.connSig = oldConn, codexSig(a.settings())
	a.threadLocked("fixture-thread").turnID = ""
	a.mu.Unlock()
	var seen atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	a.sink = func(e protocol.Event) {
		rec.sink(e)
		if e.Type == "approval.request" && seen.Add(1) == 1 {
			close(entered)
			<-release
		}
	}
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		a.mu.Lock()
		a.conn = nil
		a.mu.Unlock()
	})
	body := `{"threadId":"fixture-thread","command":"synthetic command"}`
	oldDone := startQuestionRequest(a, oldConn, "item/commandExecution/requestApproval", body)
	oldEvent := rec.wait(t, "old scoped command", func(e protocol.Event) bool { return e.Type == "approval.request" })
	questionFixtureWait(t, entered)
	newCapture := &questionReplyCapture{replies: make(chan rpcMessage, 8)}
	newConn := &rpcConn{stdin: newCapture, pending: map[int64]chan rpcResult{}, done: make(chan struct{})}
	a.mu.Lock()
	a.conn = newConn
	a.mu.Unlock()
	oldConn.shutdown(errors.New("synthetic retired source"))
	newDone := startQuestionRequest(a, newConn, "item/commandExecution/requestApproval", body)
	newEvent := rec.wait(t, "new same-session command", func(e protocol.Event) bool {
		return e.Type == "approval.request" && e.ApprovalID != oldEvent.ApprovalID
	})
	// Both host requests use raw JSON-RPC id 7 and the same session. Retiring
	// the old waiter may only remove its unique registry entry.
	releaseOnce.Do(func() { close(release) })
	questionFixtureWait(t, oldDone)
	visible := a.aps.snapshot("codex:fixture-thread")
	if len(visible) != 1 || visible[0].ApprovalID != newEvent.ApprovalID || !a.ActiveRemoteTurns() {
		t.Fatal("old connection cleanup consumed or hid the current request")
	}
	if a.RespondForSession(context.Background(), "codex:fixture-thread", oldEvent.ApprovalID, ApprovalResponse{Decision: "deny"}) {
		t.Fatal("old approval was revived by the current connection")
	}
	if !a.RespondForSession(context.Background(), "codex:fixture-thread", newEvent.ApprovalID, ApprovalResponse{Decision: "deny"}) {
		t.Fatal("current same-session request could not be answered")
	}
	questionFixtureWait(t, newDone)
	_ = questionFixtureReply(t, newCapture.replies)
	select {
	case <-oldReplies:
		t.Fatal("retired source received a stale reply")
	default:
	}
}

func TestPendingApprovalResolvedRefSkipsClosedConnectionAlias(t *testing.T) {
	registry := newApprovals()
	t.Cleanup(registry.cancelAll)
	oldSource, newSource := make(chan struct{}), make(chan struct{})
	old, _ := registry.addValidatedRequestForSource("codex:same", "codex", "7", protocol.Event{Kind: "command"}, nil, oldSource)
	current, _ := registry.addValidatedRequestForSource("codex:same", "codex", "7", protocol.Event{Kind: "command"}, nil, newSource)
	close(oldSource)
	registry.resolveByRef("7")
	select {
	case result := <-current.ch:
		if !result.noReply {
			t.Fatal("provider withdrawal did not retire the active request")
		}
	default:
		t.Fatal("closed connection alias captured the current provider's withdrawal")
	}
	if len(registry.snapshot("codex:same")) != 0 || registry.answerForSessionSource("codex:same", current.id, approvalAnswer{decision: "allow"}, newSource) {
		t.Fatal("withdrawn active request remained visible or answerable")
	}
	_ = registry.wait(old, oldSource)
}
