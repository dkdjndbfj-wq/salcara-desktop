package agents

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/protocol"
)

func TestApprovalSnapshotOnlyCurrentScopedRequestsAndNoAutomaticDecision(t *testing.T) {
	a := newApprovals()
	p, visible := a.addRequest("codex:original", "codex", "ref-original", protocol.Event{Kind: "command", Title: "Run fixture command", Detail: "fixture only"})
	a.addRequest("codex:another", "codex", "ref-another", protocol.Event{Kind: "command", Title: "Other thread"})
	first := a.snapshot("codex:original")
	if len(first) != 1 || !reflect.DeepEqual(first[0], visible) {
		t.Fatalf("wrong scoped snapshot: %+v", first)
	}
	first[0].Title = "changed copy"
	if a.snapshot("codex:original")[0].Title != visible.Title {
		t.Fatal("caller mutated registry snapshot")
	}
	select {
	case <-p.ch:
		t.Fatal("read-only snapshot answered a request")
	default:
	}
	a.resolveByRef("ref-original")
	if len(a.snapshot("codex:original")) != 0 {
		t.Fatal("withdrawn request was resurrected")
	}
	if a.answer(p.id, approvalAnswer{decision: "allow", by: "phone"}) {
		t.Fatal("withdrawn approval was accepted")
	}
	if len(newApprovals().snapshot("codex:original")) != 0 {
		t.Fatal("new process reconstructed cached approvals")
	}
}

func TestApprovalSnapshotExpiryNeverRenewedOrAuthorized(t *testing.T) {
	a := newApprovals()
	p, _ := a.addRequest("claude:original", "claude", "", protocol.Event{Kind: "command", Title: "Fixture"})
	a.mu.Lock()
	p.expiresAt = time.Now().Add(-time.Minute)
	a.mu.Unlock()
	if len(a.snapshot("claude:original")) != 0 || a.countFor("claude:original") != 0 {
		t.Fatal("expired request remained actionable")
	}
	if a.answer(p.id, approvalAnswer{decision: "allow_session", by: "phone"}) {
		t.Fatal("late answer was accepted")
	}
	answer := <-p.ch
	if answer.decision != "deny" || answer.by != "timeout" {
		t.Fatalf("late answer ran a tool: %+v", answer)
	}
}

func TestApprovalSnapshotCancellationRemovesLiveRequests(t *testing.T) {
	for _, all := range []bool{false, true} {
		a := newApprovals()
		p, _ := a.addRequest("claude:original", "claude", "", protocol.Event{Kind: "tool", Title: "Fixture"})
		if all {
			a.cancelAll()
		} else {
			a.cancelSession("claude:original")
		}
		if len(a.snapshot("claude:original")) != 0 {
			t.Fatal("cancelled request recovered")
		}
		if answer := <-p.ch; answer.decision != "deny" {
			t.Fatal("snapshot cancelled by approving")
		}
	}
}

func TestApprovalSnapshotBoundedAndHistoryDoesNotEvictActiveRequests(t *testing.T) {
	a := newApprovals()
	for i := 0; i < maxHistoryEvents+25; i++ {
		a.addRequest("codex:original", "codex", "", protocol.Event{Kind: "command", Title: strings.Repeat("x", 1000), Detail: strings.Repeat("x", 10000), Diff: strings.Repeat("x", 40000)})
	}
	pending := a.snapshot("codex:original")
	if len(pending) != maxHistoryEvents || cap(pending) != maxHistoryEvents {
		t.Fatalf("snapshot grew beyond bound: %d/%d", len(pending), cap(pending))
	}
	for _, event := range pending {
		if len(event.Title) > maxTitleChars+4 || len(event.Detail) > maxDetailChars+4 || len(event.Diff) > maxDiffChars+4 {
			t.Fatal("unbounded request fields")
		}
	}
	history := make([]protocol.Event, maxHistoryEvents)
	merged := historyWithApprovals(history, pending)
	if len(merged) != maxHistoryEvents || merged[0].Type != "approval.request" {
		t.Fatal("history evicted currently active requests")
	}
}

func TestCodexOpenReturnsActiveApprovalSnapshotWithoutMutatingRPC(t *testing.T) {
	a, transport, _ := newResumeSafetyAgent(t, cxThread{ID: "original", Cwd: t.TempDir(), Status: cxThreadStatus{Type: "idle"}}, false)
	p, visible := a.aps.addRequest("codex:original", "codex", "fixture-rpc", protocol.Event{Kind: "command", Title: "Fixture command"})
	info, events, err := a.Open(context.Background(), "original")
	if err != nil || info.Status != "waiting_approval" || len(events) != 1 || !reflect.DeepEqual(events[0], visible) {
		t.Fatalf("Open lost approval: %v %+v %+v", err, info, events)
	}
	if !reflect.DeepEqual(transport.calls, []string{"thread/read"}) {
		t.Fatalf("snapshot made mutating RPCs: %v", transport.calls)
	}
	select {
	case <-p.ch:
		t.Fatal("Open authorized a request")
	default:
	}
	a.aps.resolveByRef("fixture-rpc")
	_, events, err = a.Open(context.Background(), "original")
	if err != nil || len(events) != 0 {
		t.Fatalf("Open resurrected withdrawn request: %v %+v", err, events)
	}
}

func TestClaudeOpenReturnsRegistryApprovalNotCachedTranscriptPermission(t *testing.T) {
	settings := func() Settings { return Settings{Approval: "ask"} }
	a := newClaudeAgent(func(protocol.Event) {}, settings, "fixture-not-launched", t.TempDir())
	t.Cleanup(a.Close)
	a.live["original"] = protocol.SessionInfo{SessionKey: "claude:original", Tool: "claude", Status: "idle"}
	p, visible := a.aps.addRequest("claude:original", "claude", "", protocol.Event{Kind: "file_change", Title: "Fixture edit"})
	info, events, err := a.Open(context.Background(), "original")
	if err != nil || info.Status != "waiting_approval" || len(events) != 1 || !reflect.DeepEqual(events[0], visible) {
		t.Fatalf("Open lost live permission: %v %+v %+v", err, info, events)
	}
	select {
	case <-p.ch:
		t.Fatal("Open automatically allowed the edit")
	default:
	}
	a.aps.cancelSession("claude:original")
	_, events, err = a.Open(context.Background(), "original")
	if err != nil || len(events) != 0 {
		t.Fatalf("Open restored cancelled permission: %v %+v", err, events)
	}
}
