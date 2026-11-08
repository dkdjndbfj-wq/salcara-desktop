package agents

import (
	"testing"

	"salcara/bridge/internal/protocol"
)

func TestClaudeLiveSnapshotsHaveStrictlyIncreasingRevisionDespiteClockTick(t *testing.T) {
	a, p, rec := newClaudeResultFixture(t)
	// Freeze wall time behind the known live revision to exercise equal ticks
	// and a clock moving backwards without sleeping or changing the host clock.
	base := nowMs() + 60_000
	a.live[p.id] = protocol.SessionInfo{SessionKey: "claude:" + p.id, Status: "idle", UpdatedAt: base}
	a.setLive(p.id, func(info *protocol.SessionInfo) { info.Status = "waiting_approval" })
	a.setWorkerRunning(p)
	a.setWorkerTaskStatus(p)
	a.handleLine(p, []byte(`{"type":"system","subtype":"init","model":"resolved-model"}`))
	last := base
	for _, e := range rec.all() {
		if e.Type == "session.updated" && e.Session != nil {
			if e.Session.UpdatedAt <= last {
				t.Fatal("same-tick live snapshots have no ordering fence")
			}
			last = e.Session.UpdatedAt
		}
	}
	if a.live[p.id].UpdatedAt != base+4 || a.live[p.id].Model != "resolved-model" {
		t.Fatal("startup metadata bypassed the live revision clock")
	}
	turn := sendClaudeResultFixture(t, a, p)
	beforeTerminal := a.live[p.id].UpdatedAt
	a.handleLine(p, []byte(`{"type":"result","subtype":"success","uuid":"revision-terminal"}`))
	if info := a.live[p.id]; info.UpdatedAt != beforeTerminal+1 || info.LastTerminalTurnID != turn || info.Status != "idle" {
		t.Fatal("terminal state bypassed the same-tick live revision clock")
	}
}

func TestClaudeApprovalSnapshotUsesSameLiveRevisionClock(t *testing.T) {
	a, p, _ := newClaudeResultFixture(t)
	base := nowMs() + 60_000
	a.live[p.id] = protocol.SessionInfo{SessionKey: "claude:" + p.id, Status: "running", UpdatedAt: base}
	pending, _ := a.aps.addValidatedRequestForSource("claude:"+p.id, "claude", "", protocol.Event{Kind: "command"}, nil, p.permissionLease.done)
	a.setApprovalStatus(p)
	if info := a.live[p.id]; info.Status != "waiting_approval" || info.UpdatedAt != base+1 {
		t.Fatal("approval state did not advance the live revision")
	}
	if !a.Respond(pending.id, "deny", "synthetic") {
		t.Fatal("synthetic pending approval could not be resolved")
	}
	a.setApprovalStatus(p)
	if info := a.live[p.id]; info.Status != "idle" || info.UpdatedAt != base+2 {
		t.Fatal("resolved approval state did not advance the live revision")
	}
}
