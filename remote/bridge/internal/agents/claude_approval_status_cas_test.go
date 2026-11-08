package agents

import (
	"testing"
	"time"

	"salcara/bridge/internal/protocol"
)

func TestClaudeApprovalStatusOldCallbackCannotUpdateReplacement(t *testing.T) {
	for _, elicitation := range []bool{false, true} {
		t.Run(map[bool]string{false: "permission", true: "elicitation"}[elicitation], func(t *testing.T) {
			a, old, rec := newClaudeResultFixture(t)
			oldHook := &claudeElicitationLease{done: make(chan struct{})}
			old.elicitationHook = oldHook
			a.elicitationHooks[old.id] = oldHook
			// Exact scheduling boundary: the callback has read its registry
			// snapshot, then the old worker retires before native-state commit.
			snapshot := a.aps.statusSnapshot("claude:"+old.id, old.permissionLease.done, oldHook.done)
			old.permissionLease.revoke()
			oldHook.revoke()
			next := claudeSyntheticPermissionWorker(t, a, old.id)
			next.elicitationHook = &claudeElicitationLease{done: make(chan struct{})}
			a.elicitationHooks[next.id] = next.elicitationHook
			a.live[next.id] = protocol.SessionInfo{SessionKey: "claude:" + next.id, Status: "idle", LastTerminalTurnID: "new-terminal"}
			var hook *claudeElicitationLease
			if elicitation {
				hook = oldHook
			}
			if !a.commitApprovalStatus(old, hook, snapshot) {
				t.Fatal("retired callback requested retry on another worker")
			}
			a.setElicitationApprovalStatus(old.id, oldHook)
			if got := a.live[next.id]; got.Status != "idle" || got.LastTerminalTurnID != "new-terminal" || len(rec.all()) != 0 {
				t.Fatal("retired callback changed or published replacement status", got)
			}
		})
	}
}

func TestClaudeApprovalStatusRegistryRevisionRejectsOldSameWorkerSnapshot(t *testing.T) {
	a, p, rec := newClaudeResultFixture(t)
	a.live[p.id] = protocol.SessionInfo{SessionKey: "claude:" + p.id, Status: "idle"}
	beforeAdd := a.aps.statusSnapshot("claude:"+p.id, p.permissionLease.done)
	pending, _ := a.aps.addValidatedRequestForSource("claude:"+p.id, "claude", "synthetic", protocol.Event{}, nil, p.permissionLease.done)
	a.setApprovalStatus(p)
	if a.live[p.id].Status != "waiting_approval" || a.commitApprovalStatus(p, nil, beforeAdd) || a.live[p.id].Status != "waiting_approval" {
		t.Fatal("old empty snapshot erased a newer pending request")
	}
	beforeRemove := a.aps.statusSnapshot("claude:"+p.id, p.permissionLease.done)
	a.aps.remove(pending.id)
	a.setApprovalStatus(p)
	if a.live[p.id].Status != "idle" || a.commitApprovalStatus(p, nil, beforeRemove) || a.live[p.id].Status != "idle" {
		t.Fatal("old pending snapshot resurrected waiting or invented running")
	}
	if len(rec.all()) != 2 {
		t.Fatal("rejected status snapshots emitted extra updates")
	}
}

func TestClaudeApprovalStatusSnapshotChecksLiveCancellationWithoutMutation(t *testing.T) {
	a, p, _ := newClaudeResultFixture(t)
	a.live[p.id] = protocol.SessionInfo{SessionKey: "claude:" + p.id, Status: "waiting_approval"}
	cancelled := make(chan struct{})
	a.aps.addValidatedRequestForLifetimes("claude:"+p.id, "claude", "synthetic", protocol.Event{}, nil, p.permissionLease.done, cancelled)
	snapshot := a.aps.statusSnapshot("claude:"+p.id, p.permissionLease.done)
	close(cancelled) // Native cancellation precedes asynchronous registry removal.
	if !a.commitApprovalStatus(p, nil, snapshot) || a.live[p.id].Status != "idle" {
		t.Fatal("cancelled request remained waiting until registry cleanup")
	}
}

func TestClaudeApprovalStatusTracksRealWorkAndOnlyOwnedRequestSources(t *testing.T) {
	for _, mode := range []string{"idle", "turn", "subagent", "foreign_request", "own_request"} {
		t.Run(mode, func(t *testing.T) {
			a, p, _ := newClaudeResultFixture(t)
			want := "idle"
			switch mode {
			case "turn":
				p.turns = 1
				p.turnIDs = []string{"actual-turn"}
				want = "running"
			case "subagent":
				p.activeSubtasks.Store(true)
				want = "running"
			case "foreign_request":
				a.aps.addValidatedRequestForSource("claude:"+p.id, "claude", "foreign", protocol.Event{}, nil, make(chan struct{}))
			case "own_request":
				a.aps.addValidatedRequestForSource("claude:"+p.id, "claude", "own", protocol.Event{}, nil, p.permissionLease.done)
				want = "waiting_approval"
			}
			a.setApprovalStatus(p)
			if got := a.live[p.id]; got.Status != want || mode == "turn" && got.ActiveTurnID != "actual-turn" {
				t.Fatal("status did not match owned worker state", got)
			}
		})
	}
}

func TestApprovalStatusRevisionCoversEveryRegistryMutation(t *testing.T) {
	for _, mutation := range []string{"legacy_add", "add", "remove", "resolve", "answer", "expire", "source_closed", "cancel_session", "cancel_all"} {
		t.Run(mutation, func(t *testing.T) {
			a := newApprovals()
			source := make(chan struct{})
			p, _ := a.addValidatedRequestForSource("claude:s", "claude", "ref", protocol.Event{}, nil, source)
			before := a.revision.Load()
			switch mutation {
			case "legacy_add":
				a.add("claude:s", "claude")
			case "add":
				a.addRequest("claude:s", "claude", "other", protocol.Event{})
			case "remove":
				a.remove(p.id)
			case "resolve":
				a.resolveByRef("ref")
			case "answer":
				a.answer(p.id, approvalAnswer{decision: "deny"})
			case "expire":
				p.expiresAt = time.Now().Add(-time.Second)
				a.answer(p.id, approvalAnswer{decision: "deny"})
			case "source_closed":
				close(source)
				a.answer(p.id, approvalAnswer{decision: "deny"})
			case "cancel_session":
				a.cancelSession("claude:s")
			case "cancel_all":
				a.cancelAll()
			}
			if a.revision.Load() <= before {
				t.Fatal("registry mutation did not fence old status")
			}
		})
	}
}
