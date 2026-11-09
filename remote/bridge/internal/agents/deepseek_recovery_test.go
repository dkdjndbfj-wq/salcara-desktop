package agents

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"salcara/bridge/internal/protocol"
)

func deepSeekTestBoot(host, boot string) deepSeekBootProof {
	return deepSeekBootFromIDs("fixture", host, boot)
}

func deepSeekRecoveryFixture(t *testing.T, status string, pending bool, proof deepSeekBootProof) (string, string) {
	t.Helper()
	state, id := t.TempDir(), deepSeekNewID()
	a := newDeepSeekAgentWithBoot(func() Settings { return Settings{} }, state, proof)
	s := &deepSeekSession{Info: protocol.SessionInfo{SessionKey: deepSeekID + ":" + id, Tool: deepSeekID, Cwd: t.TempDir(), Status: status, Controllable: true}, Started: true, RuntimePending: pending, RuntimeBoot: proof}
	s.Events = []protocol.Event{{Tool: deepSeekID, SessionKey: s.Info.SessionKey, Type: "message", ID: "original", Role: "user", Text: "Do not replay this task"}}
	if err := a.persistLocked(id, s); err != nil {
		t.Fatal(err)
	}
	return state, id
}

func TestDeepSeekRecoveryRequiresSameHostDifferentBoot(t *testing.T) {
	old := deepSeekTestBoot(strings.Repeat("1", 32), strings.Repeat("2", 32))
	next := deepSeekTestBoot(strings.Repeat("1", 32), strings.Repeat("3", 32))
	for _, status := range []string{"running", "waiting_approval", "unknown", "idle"} {
		t.Run(status, func(t *testing.T) {
			state, id := deepSeekRecoveryFixture(t, status, true, old)
			a := newDeepSeekAgentWithBoot(func() Settings { t.Fatal("recovery consulted API credentials"); return Settings{} }, state, next)
			s := a.sessions[id]
			if s == nil || s.Unconfirmed || s.RuntimePending || s.busy || s.conn != nil || !s.Started || s.Info.Status != "failed" || !s.Info.Controllable || a.ActiveRemoteTurns() {
				t.Fatalf("unsafe recovery state: %+v", s)
			}
			if len(s.Events) != 2 || s.Events[0].Text != "Do not replay this task" || s.Events[1].ID != "deepseek-runtime-recovered" {
				t.Fatalf("history/recovery notice lost: %+v", s.Events)
			}
			b := newDeepSeekAgentWithBoot(func() Settings { return Settings{} }, state, next)
			if b.ActiveRemoteTurns() || b.sessions[id].Unconfirmed || b.sessions[id].RuntimePending {
				t.Fatal("durable recovery lost on next Bridge launch")
			}
		})
	}
}

func TestDeepSeekRecoveryDoesNotInferExitFromMissingOrForeignProof(t *testing.T) {
	old := deepSeekTestBoot(strings.Repeat("1", 32), strings.Repeat("2", 32))
	for name, current := range map[string]deepSeekBootProof{"same boot": old, "read failure": {}, "foreign host": deepSeekTestBoot(strings.Repeat("4", 32), strings.Repeat("3", 32))} {
		t.Run(name, func(t *testing.T) {
			state, id := deepSeekRecoveryFixture(t, "idle", true, old)
			a := newDeepSeekAgentWithBoot(func() Settings { return Settings{} }, state, current)
			if !a.ActiveRemoteTurns() || !a.sessions[id].Unconfirmed || a.sessions[id].Info.Controllable {
				t.Fatal("unproven tree released")
			}
			if err := a.Send(context.Background(), id, "do not send"); err == nil {
				t.Fatal("unproven runtime admitted a task")
			}
		})
	}
	state, id := deepSeekRecoveryFixture(t, "running", false, deepSeekBootProof{})
	a := newDeepSeekAgentWithBoot(func() Settings { return Settings{} }, state, old)
	if !a.ActiveRemoteTurns() || !a.sessions[id].Unconfirmed || a.sessions[id].RuntimeBoot.valid() {
		t.Fatal("legacy identity was invented")
	}
}

func TestDeepSeekRecoveryWriteFailureKeepsFence(t *testing.T) {
	old := deepSeekTestBoot(strings.Repeat("1", 32), strings.Repeat("2", 32))
	next := deepSeekTestBoot(strings.Repeat("1", 32), strings.Repeat("3", 32))
	state, id := deepSeekRecoveryFixture(t, "running", true, old)
	if err := os.Mkdir(filepath.Join(state, deepSeekID, id, "session.json.tmp"), 0700); err != nil {
		t.Fatal(err)
	}
	a := newDeepSeekAgentWithBoot(func() Settings { return Settings{} }, state, next)
	s := a.sessions[id]
	if !a.ActiveRemoteTurns() || !s.Unconfirmed || s.Info.Controllable || len(s.Events) != 1 {
		t.Fatalf("failed persistence released or advertised recovery: %+v", s)
	}
}

func TestDeepSeekSendCannotPassFailedAdmissionCheckpoint(t *testing.T) {
	proof := deepSeekTestBoot(strings.Repeat("1", 32), strings.Repeat("2", 32))
	state, id := deepSeekRecoveryFixture(t, "idle", false, proof)
	a := newDeepSeekAgentWithBoot(func() Settings { t.Fatal("read API/start runtime before durable admission"); return Settings{} }, state, proof)
	if err := os.Mkdir(filepath.Join(state, deepSeekID, id, "session.json.tmp"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := a.SendWithOptions(context.Background(), id, "must not be delivered", TurnOptions{Model: "new-model"}); err == nil {
		t.Fatal("write failure ignored")
	}
	s := a.sessions[id]
	if s.busy || s.conn != nil || s.Info.Status != "idle" || s.Info.Model != "" {
		t.Fatalf("failed admission mutated execution: %+v", s)
	}
}

func TestDeepSeekStaleStopCannotReleaseReplacementFence(t *testing.T) {
	proof := deepSeekTestBoot(strings.Repeat("1", 32), strings.Repeat("2", 32))
	state, id := deepSeekRecoveryFixture(t, "idle", false, proof)
	a := newDeepSeekAgentWithBoot(func() Settings { return Settings{} }, state, proof)
	s := a.sessions[id]
	s.conn = &rpcConn{}
	s.RuntimePending = true
	if err := a.confirmDeepSeekRuntimeStopped(s, &rpcConn{}); err != nil {
		t.Fatal(err)
	}
	if !s.RuntimePending {
		t.Fatal("old runtime cleared replacement marker")
	}
}

func TestDeepSeekReapedRootDoesNotProveDescendantExit(t *testing.T) {
	proof := deepSeekTestBoot(strings.Repeat("1", 32), strings.Repeat("2", 32))
	state, id := deepSeekRecoveryFixture(t, "idle", false, proof)
	a := newDeepSeekAgentWithBoot(func() Settings { return Settings{} }, state, proof)
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	prepareCmd(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	close(done)
	conn := &rpcConn{cmd: cmd, processWaitDone: done}
	s := a.sessions[id]
	s.conn = conn
	s.RuntimePending = true
	if err := a.stopRuntime(s, conn); err == nil {
		t.Fatal("root Wait incorrectly released an unproven tree")
	}
	if !s.Unconfirmed || !a.ActiveRemoteTurns() {
		t.Fatal("unexpected root exit does not fence API changes")
	}
	// A previously confirmed old worker may still have delayed Go waiters.
	// The replacement has reserved its fence but has not installed conn yet.
	s.stoppedConn, s.conn = conn, nil
	s.Unconfirmed, s.RuntimePending = false, true
	if err := a.stopRuntime(s, conn); err != nil {
		t.Fatal(err)
	}
	if s.Unconfirmed || !s.RuntimePending {
		t.Fatal("old stop waiter changed the successor reservation")
	}
}

func TestDeepSeekFailedStopCheckpointNeverBecomesSuccess(t *testing.T) {
	proof := deepSeekTestBoot(strings.Repeat("1", 32), strings.Repeat("2", 32))
	state, id := deepSeekRecoveryFixture(t, "idle", false, proof)
	a := newDeepSeekAgentWithBoot(func() Settings { return Settings{} }, state, proof)
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	prepareCmd(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	close(done)
	conn := &rpcConn{cmd: cmd, processWaitDone: done}
	s := a.sessions[id]
	s.conn, s.RuntimePending = conn, true
	if err := os.Mkdir(filepath.Join(state, deepSeekID, id, "session.json.tmp"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := a.confirmDeepSeekRuntimeStopped(s, conn); err == nil {
		t.Fatal("failed stop checkpoint reported success")
	}
	if s.stoppedConn == conn || !s.Unconfirmed || !s.RuntimePending {
		t.Fatal("failed checkpoint registered a successful stop")
	}
	if err := a.stopRuntime(s, conn); err == nil {
		t.Fatal("repeated stop reported success after failed persistence")
	}
	if !a.ActiveRemoteTurns() {
		t.Fatal("failed stop checkpoint released configuration admission")
	}
}

func TestDeepSeekRecoveryRejectsApprovalsDuringStop(t *testing.T) {
	proof := deepSeekTestBoot(strings.Repeat("1", 32), strings.Repeat("2", 32))
	state, id := deepSeekRecoveryFixture(t, "idle", false, proof)
	a := newDeepSeekAgentWithBoot(func() Settings { return Settings{} }, state, proof)
	s := a.sessions[id]
	conn := &rpcConn{}
	s.conn, s.busy, s.Unconfirmed = conn, true, true
	requestJSON, _ := json.Marshal(map[string]string{"sessionId": id, "toolName": "write"})
	a.request(id, s, conn, json.RawMessage(`"new-approval"`), "approval/request", requestJSON)
	if len(s.approvals) != 0 {
		t.Fatal("stopping runtime queued a new approval")
	}
	approval := deepSeekID + ":" + id + ":" + deepSeekNewID()
	s.approvals[approval] = deepSeekApproval{conn: conn, rpcID: json.RawMessage(`"pending"`), source: extensionSource(ExtensionSettings{}, "")}
	if a.Respond(approval, "allow", "") {
		t.Fatal("stopping runtime received approval")
	}
	s.Unconfirmed = false
	a.settings = func() Settings {
		a.mu.Lock()
		s.Unconfirmed = true
		a.mu.Unlock()
		return Settings{}
	}
	if a.RespondForSession(context.Background(), s.Info.SessionKey, approval, ApprovalResponse{Decision: "allow"}) {
		t.Fatal("approval skipped final stopping-state recheck")
	}
	if len(s.approvals) != 1 {
		t.Fatal("rejected approval consumed pending grant")
	}
}
