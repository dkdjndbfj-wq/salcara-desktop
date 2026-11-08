package agents

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/protocol"
)

func TestApprovalContractPreservesIdentityAndRejectsUnofferedDecision(t *testing.T) {
	a := newApprovals()
	options := &protocol.ApprovalOptions{Version: 1, Decisions: []string{"allow", "deny"}}
	p, event := a.addRequest("claude:s", "claude", "tool-1", protocol.Event{Kind: "tool", TurnID: "turn-1", ParentID: "child-1", ApprovalOptions: options})
	options.Decisions[0] = "allow_session"
	event.ApprovalOptions.Decisions[0] = "allow_session"
	snap := a.snapshot("claude:s")
	if len(snap) != 1 || snap[0].TurnID != "turn-1" || snap[0].ParentID != "child-1" || !reflect.DeepEqual(snap[0].ApprovalOptions.Decisions, []string{"allow", "deny"}) {
		t.Fatal("approval identity/options not isolated", snap)
	}
	snap[0].ApprovalOptions.Decisions[0] = "allow_session"
	if a.answer(p.id, approvalAnswer{decision: "allow_session", by: "phone"}) || a.answer(p.id, approvalAnswer{decision: "unexpected", by: "phone"}) {
		t.Fatal("unoffered action consumed request")
	}
	if !a.answer(p.id, approvalAnswer{decision: "allow", by: "phone"}) {
		t.Fatal("valid action lost")
	}
}

func TestApprovalContractUnknownVersionIsNotPermission(t *testing.T) {
	a := newApprovals()
	p, _ := a.addRequest("extension:s", "extension", "req", protocol.Event{ApprovalOptions: &protocol.ApprovalOptions{Version: 2, Decisions: []string{"allow"}}})
	if a.answer(p.id, approvalAnswer{decision: "allow", by: "phone"}) {
		t.Fatal("unknown contract granted permission")
	}
	a.cancelSession("extension:s")
	if ans := <-p.ch; ans.decision != "deny" {
		t.Fatal("internal cancellation blocked")
	}
}

func TestApprovalPerRequestCancelImmediatelyFencesLiveWorker(t *testing.T) {
	a := newApprovals()
	worker, cancelled := make(chan struct{}), make(chan struct{})
	old, _ := a.addValidatedRequestForLifetimes("claude:s", "claude", "old", protocol.Event{}, nil, worker, cancelled)
	live, _ := a.addValidatedRequestForSource("claude:s", "claude", "live", protocol.Event{}, nil, worker)
	close(cancelled)
	if a.countFor("claude:s") != 1 || len(a.snapshot("claude:s")) != 1 || a.answerForSessionSource("claude:s", old.id, approvalAnswer{decision: "allow"}, worker) {
		t.Fatal("cancelled request survived in active worker")
	}
	if !a.answerForSessionSource("claude:s", live.id, approvalAnswer{decision: "allow"}, worker) {
		t.Fatal("unrelated request lost")
	}
	if p, _ := a.addValidatedRequestForLifetimes("claude:s", "claude", "late", protocol.Event{}, nil, worker, cancelled); p != nil {
		t.Fatal("cancelled request registered late")
	}
}

func TestClaudeNativePromptNeverUsesBroadRememberedGrant(t *testing.T) {
	for _, policy := range []string{"ask", "auto_edits", "auto_all"} {
		t.Run(policy, func(t *testing.T) {
			rec := &recorder{}
			a := newClaudeAgent(rec.sink, func() Settings { return Settings{Approval: policy} }, "not-launched", t.TempDir())
			t.Cleanup(a.Close)
			p := claudeSyntheticPermissionWorker(t, a, "fixture-session")
			a.mu.Lock()
			a.allowed[p.id] = map[string]bool{"Edit": true}
			a.mu.Unlock()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- claudeScopedPermissionRequest(a, p, "Edit", `{}`, ctx) }()
			ev := rec.wait(t, "native explicit prompt", func(e protocol.Event) bool { return e.Type == "approval.request" })
			if ev.ApprovalOptions == nil || !reflect.DeepEqual(ev.ApprovalOptions.Decisions, []string{"allow", "deny"}) || a.Respond(ev.ApprovalID, "allow_session", "") {
				t.Fatal("unsafe session grant")
			}
			cancel()
			if a.Respond(ev.ApprovalID, "allow", "") {
				t.Fatal("cancelled request remained answerable")
			}
			select {
			case response := <-done:
				var result PermissionDecision
				if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Behavior != "deny" {
					t.Fatal("late allow after cancellation")
				}
			case <-time.After(time.Second):
				t.Fatal("per-request cancellation did not retire")
			}
		})
	}
}

func TestCodexApprovalAvailableDecisionsAreRespected(t *testing.T) {
	for _, native := range []string{`["accept","decline"]`, `["cancel"]`, `[]`, `null`, `[{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":["echo"]}}]`} {
		t.Run(native, func(t *testing.T) {
			a, c, rec, replies := newCodexQuestionFixture(t, "ask")
			done := startQuestionRequest(a, c, "item/commandExecution/requestApproval", `{"threadId":"fixture-thread","turnId":"turn-1","itemId":"tool-1","command":"echo synthetic","availableDecisions":`+native+`}`)
			if len(cxRequestOptions(json.RawMessage(native)).Decisions) == 0 {
				questionFixtureWait(t, done)
				select {
				case reply := <-replies:
					if reply.Error == nil {
						t.Fatal("unknown native options silently accepted")
					}
				case <-time.After(time.Second):
					t.Fatal("unsupported request hung")
				}
				if len(a.aps.snapshot("codex:fixture-thread")) != 0 {
					t.Fatal("unanswerable request exposed to phone")
				}
				return
			}
			ev := rec.wait(t, "bounded decisions", func(e protocol.Event) bool { return e.Type == "approval.request" })
			if ev.TurnID != "turn-1" || ev.ParentID != "tool-1" || native != "null" && a.Respond(ev.ApprovalID, "allow_session", "") {
				t.Fatal("lost identity or widened native decision")
			}
			canAllow := native == `["accept","decline"]` || native == "null"
			if native == `["cancel"]` && ev.ApprovalOptions.DenyEffect != "cancel_turn" {
				t.Fatal("cancel rendered as a simple tool rejection")
			}
			if a.Respond(ev.ApprovalID, "allow", "") != canAllow {
				t.Fatal("allow availability mismatch")
			}
			if !canAllow {
				if native == `["cancel"]` {
					if !a.Respond(ev.ApprovalID, "deny", "") {
						t.Fatal("cancel cannot be expressed as reject")
					}
				} else {
					a.aps.cancelSession("codex:fixture-thread")
				}
			}
			questionFixtureWait(t, done)
			reply := questionFixtureReply(t, replies)
			if native == `["cancel"]` && reply["decision"] != "cancel" {
				t.Fatal("invalid native cancel")
			}
		})
	}
}

func TestCodexNetworkApprovalPreservesDestinationAndExtraPermissions(t *testing.T) {
	a, c, rec, replies := newCodexQuestionFixture(t, "ask")
	done := startQuestionRequest(a, c, "item/commandExecution/requestApproval", `{"threadId":"fixture-thread","turnId":"t","itemId":"i","environmentId":"local-fixture","command":"curl example.invalid","networkApprovalContext":{"host":"example.invalid","protocol":"https"},"additionalPermissions":{"file_system":{"write":["C:/fixture"]}},"availableDecisions":["accept","decline"]}`)
	ev := rec.wait(t, "network approval", func(e protocol.Event) bool { return e.Type == "approval.request" })
	if ev.Kind != "permission" || !strings.Contains(ev.Title, "example.invalid") || !strings.Contains(ev.Detail, "curl example.invalid") || !strings.Contains(ev.Detail, "https") || !strings.Contains(ev.Detail, "C:/fixture") || !strings.Contains(ev.Detail, "local-fixture") {
		t.Fatal("permission request lost native context", ev)
	}
	if !a.Respond(ev.ApprovalID, "deny", "") {
		t.Fatal("native request not answerable")
	}
	questionFixtureWait(t, done)
	if questionFixtureReply(t, replies)["decision"] != "decline" {
		t.Fatal("network approval used wrong response envelope")
	}
}

func TestClaudeCancelledRequestReleasesWaitingWithoutEndingOtherRequests(t *testing.T) {
	rec := &recorder{}
	a := newClaudeAgent(rec.sink, func() Settings { return Settings{Approval: "ask"} }, "not-launched", t.TempDir())
	t.Cleanup(a.Close)
	worker := claudeSyntheticPermissionWorker(t, a, "fixture-session")
	a.mu.Lock()
	a.live[worker.id] = protocol.SessionInfo{SessionKey: "claude:" + worker.id, Tool: "claude", Status: "running"}
	a.mu.Unlock()
	first, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	second, cancelSecond := context.WithCancel(context.Background())
	defer cancelSecond()
	done := make(chan struct{}, 2)
	go func() {
		_ = claudeScopedPermissionRequest(a, worker, "Bash", `{"command":"first"}`, first)
		done <- struct{}{}
	}()
	one := rec.wait(t, "first prompt", func(e protocol.Event) bool { return e.Type == "approval.request" })
	go func() {
		_ = claudeScopedPermissionRequest(a, worker, "Bash", `{"command":"second"}`, second)
		done <- struct{}{}
	}()
	two := rec.wait(t, "second prompt", func(e protocol.Event) bool { return e.Type == "approval.request" && e.ApprovalID != one.ApprovalID })
	cancelFirst()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("first cancellation hung")
	}
	if a.aps.countFor("claude:"+worker.id) != 1 || a.Respond(one.ApprovalID, "allow", "") {
		t.Fatal("one cancellation damaged dynamic queue")
	}
	a.mu.Lock()
	status := a.live[worker.id].Status
	a.mu.Unlock()
	if status != "waiting_approval" {
		t.Fatal("second request lost waiting state", status)
	}
	cancelSecond()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("second cancellation hung")
	}
	if a.aps.countFor("claude:"+worker.id) != 0 || a.Respond(two.ApprovalID, "allow", "") {
		t.Fatal("cancelled queue remained actionable")
	}
	a.mu.Lock()
	status = a.live[worker.id].Status
	a.mu.Unlock()
	if status == "waiting_approval" {
		t.Fatal("last cancellation left session stuck waiting")
	}
}
