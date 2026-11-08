package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/protocol"
)

func requiredUpdateFixture(t *testing.T, c *Client) {
	t.Helper()
	c.o.Version = "1.6.4"
	if err := c.SetUpdatePolicy(UpdatePolicy{Required: true, MinimumSupportedVersion: "1.6.5", TargetVersion: "1.7.0", Reason: "连接协议已更新"}); err != nil {
		t.Fatal(err)
	}
}

func TestRequiredUpdateBlocksAllTaskAdmissions(t *testing.T) {
	c := New(Options{Version: "1.6.4"})
	requiredUpdateFixture(t, c)
	commands := []map[string]any{
		{"type": "session.start"}, {"type": "session.send"}, {"type": "session.continue"}, {"type": "desktop.session.send"},
		{"type": "session.compact"}, {"type": cooperationPrepareCommand}, {"type": "session.side-question.ask"},
		{"type": "session.side-question.send"}, {"type": "session.side-question.tools", "action": "send"},
		{"type": "session.workflow", "kind": "review"}, {"type": "session.workflow", "kind": "goal.set"},
		{"type": "session.workflow", "kind": "goal.resume"}, {"type": "session.action", "action": "fork"},
		{"type": "workspace.terminal.open"}, {"type": "workspace.project.preview", "action": "start"},
	}
	for _, ctx := range []context.Context{context.Background(), WithLocalExecution(context.Background())} {
		for _, cmd := range commands {
			if _, err := c.Dispatch(ctx, cmd); !errors.Is(err, ErrUpdateRequired) {
				t.Errorf("%v admitted: %v", cmd, err)
			}
		}
	}
	if _, err := c.BeginLocalToolChange(context.Background()); !errors.Is(err, ErrUpdateRequired) {
		t.Fatalf("managed tool launch admitted: %v", err)
	}
	if _, err := c.Dispatch(context.Background(), map[string]any{"type": "device.ping", "nonce": "test"}); err != nil {
		t.Fatal(err)
	}
}

func TestRequiredUpdateDoesNotExpireOrCancelWithInstallerLease(t *testing.T) {
	c := New(Options{Version: "1.6.4"})
	a := &updateAgent{fakeAgent: &fakeAgent{id: "codex"}, busy: true}
	c.SetManager(&fakeManager{list: []agents.Agent{a}})
	requiredUpdateFixture(t, c)
	if c.PrepareUpdate(context.Background()) == nil || len(a.stopped) != 0 {
		t.Fatal("policy must not stop busy work or bypass idle check")
	}
	if !c.CancelUpdate() || !errors.Is(c.CheckUpdateAdmission(), ErrUpdateRequired) {
		t.Fatal("installer cancellation released required policy")
	}
	a.busy = false
	if err := c.PrepareUpdate(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.updateUntil = time.Now().Add(-time.Second)
	c.expireUpdateLocked()
	if !errors.Is(c.CheckUpdateAdmission(), ErrUpdateRequired) {
		t.Fatal("installer timeout released required policy")
	}
	if err := c.SetUpdatePolicy(UpdatePolicy{}); err != nil || c.CheckUpdateAdmission() != nil {
		t.Fatal("explicit verified policy removal did not restore admission")
	}
}

func TestRequiredUpdatePreservesReadApprovalAndWorkflowStop(t *testing.T) {
	c, a, _ := aggregationFixture(t, "codex")
	a.descriptor.Capabilities.Approval = true
	requiredUpdateFixture(t, c)
	for _, cmd := range []map[string]any{
		{"type": "sessions.list", "tool": "codex"},
		{"type": "session.open", "sessionKey": "codex:s", "controlSurface": "cli"},
		{"type": "approval.respond", "sessionKey": "codex:s", "approvalId": "fixture", "decision": "allow", "controlSurface": "cli"},
	} {
		if _, err := c.Dispatch(context.Background(), cmd); err != nil {
			t.Fatalf("existing work %v blocked: %v", cmd, err)
		}
	}
	if a.responseCalls != 1 || len(a.opened) != 1 {
		t.Fatalf("read or approval did not reach worker: %+v", a)
	}
	workflow, _ := workflowClient(t)
	requiredUpdateFixture(t, workflow)
	for _, kind := range []string{"", "goal.pause", "goal.clear"} {
		if _, err := workflow.Dispatch(context.Background(), workflowCommand(kind)); err != nil {
			t.Fatalf("workflow %q blocked: %v", kind, err)
		}
	}
}

func TestRequiredUpdateDoesNotCancelAdmittedSideQuestion(t *testing.T) {
	c, a := sideFixture(t)
	c.o.Version = "1.6.4"
	rememberSideFixture(c, []protocol.Event{{SessionKey: "codex:s", ID: "u1", Type: "message", Role: "user", Text: "question"}}, "")
	reading := sideReading(t, c)
	a.wait = func() { requiredUpdateFixture(t, c) }
	if _, err := c.Dispatch(context.Background(), sideCommand(reading.Revision)); err != nil {
		t.Fatalf("required update interrupted already admitted work: %v", err)
	}
	if _, err := c.Dispatch(context.Background(), sideCommand(reading.Revision)); !errors.Is(err, ErrUpdateRequired) {
		t.Fatalf("next side question admitted: %v", err)
	}
}

func TestRequiredUpdateValidationAndStartupRestore(t *testing.T) {
	policy := UpdatePolicy{Required: true, MinimumSupportedVersion: "1.6.5", TargetVersion: "1.7.0", Reason: "协议更新"}
	encoded, _ := json.Marshal(policy)
	t.Setenv("SALCARA_UPDATE_POLICY", string(encoded))
	c := New(Options{Version: "1.6.4"})
	if c.UpdatePolicy() != policy || !errors.Is(c.CheckUpdateAdmission(), ErrUpdateRequired) {
		t.Fatal("startup did not close admission before manager initialization")
	}
	for _, bad := range []UpdatePolicy{
		{Required: true}, {Required: true, MinimumSupportedVersion: "1.7.0", TargetVersion: "1.6.5"},
		{Required: true, MinimumSupportedVersion: "1.6.4", TargetVersion: "1.7.0"},
		{MinimumSupportedVersion: "1.6.5", TargetVersion: "1.7.0"},
		{Required: true, MinimumSupportedVersion: "1.6.x", TargetVersion: "1.7.0"},
		{Reason: strings.Repeat("x", 1001)},
	} {
		if c.SetUpdatePolicy(bad) == nil || c.UpdatePolicy() != policy {
			t.Fatalf("invalid policy changed admission: %+v", bad)
		}
	}
	t.Setenv("SALCARA_UPDATE_POLICY", "")
	if New(Options{Version: "1.7.0"}).CheckUpdateAdmission() != nil {
		t.Fatal("new version without cached policy was blocked")
	}
}
