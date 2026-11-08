package hubclient

import (
	"context"
	"errors"
	"sync"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/protocol"
)

type workspaceStartEventsAgent struct {
	*aggregationFixtureAgent
	client    *Client
	statuses  []string
	resultErr error
}

func (a *workspaceStartEventsAgent) Start(_ context.Context, cwd, _, _, _ string) (string, error) {
	for _, status := range a.statuses {
		a.client.Push(protocol.Event{Type: "session.updated", SessionKey: a.ID() + ":fast", Session: &protocol.SessionInfo{SessionKey: a.ID() + ":fast", Cwd: cwd, Status: status}})
	}
	return "fast", a.resultErr
}

func TestWorkspaceStartReceiptDoesNotOverwriteObservedTerminalState(t *testing.T) {
	for _, family := range []string{"codex", "claude"} {
		for _, partial := range []bool{false, true} {
			t.Run(family+map[bool]string{false: "/success", true: "/partial"}[partial], func(t *testing.T) {
				c, base, cwd := aggregationFixture(t, family)
				a := &workspaceStartEventsAgent{aggregationFixtureAgent: base, client: c, statuses: []string{"running", "idle"}}
				if partial {
					a.resultErr = errors.New("synthetic post-creation failure")
				}
				m := &fakeManager{list: []agents.Agent{a}}
				c.SetManager(m)
				_, err := c.Dispatch(context.Background(), map[string]any{"type": "session.start", "tool": family, "cwd": cwd, "prompt": "finish immediately"})
				if !partial && err != nil {
					t.Fatal(err)
				}
				if partial && protocol.ExecutionErrorCode(err) != "command_delivery_uncertain" {
					t.Fatal("partial creation lost uncertainty")
				}
				if err := c.checkWorkspaceAdmission(context.Background(), m, cwd, ""); err != nil {
					t.Fatalf("start receipt revived completed workspace: %v", err)
				}
			})
		}
	}
}

func TestWorkspaceStartKeepsUnconfirmedAndRunningReservations(t *testing.T) {
	for _, statuses := range [][]string{nil, {"running"}, {"waiting_approval"}} {
		c, base, cwd := aggregationFixture(t, "claude")
		a := &workspaceStartEventsAgent{aggregationFixtureAgent: base, client: c, statuses: statuses}
		m := &fakeManager{list: []agents.Agent{a}}
		c.SetManager(m)
		if _, err := c.Dispatch(context.Background(), map[string]any{"type": "session.start", "tool": "claude", "cwd": cwd, "prompt": "active"}); err != nil {
			t.Fatal(err)
		}
		if err := c.checkWorkspaceAdmission(context.Background(), m, cwd, ""); !errors.Is(err, errWorkspaceBusy) {
			t.Fatalf("unconfirmed/running creation lost reservation: %v", err)
		}
		if c.workspaceStartObserver != nil {
			t.Fatal("start observer outlived admission")
		}
	}
}

func TestObserveLocalEventChangesOnlyWorkspaceAdmission(t *testing.T) {
	c, _, cwd := aggregationFixture(t, "claude")
	key := "claude:private"
	c.workspaceScanned = true
	for _, status := range []string{"running", "waiting_approval", "idle"} {
		c.ObserveLocalEvent(protocol.Event{Type: "session.updated", SessionKey: key, Session: &protocol.SessionInfo{SessionKey: key, Cwd: cwd, Status: status}})
		err := c.checkWorkspaceAdmission(context.Background(), c.manager(), cwd, "")
		if status == "idle" && err != nil || status != "idle" && !errors.Is(err, errWorkspaceBusy) {
			t.Fatalf("wrong local admission state: %s %v", status, err)
		}
	}
	c.ObserveLocalEvent(protocol.Event{Type: "message", SessionKey: key, Text: "private text must remain local"})
	if c.Status().Queued != 0 || len(c.queue) != 0 || c.queueBytes != 0 || c.inFlight != 0 {
		t.Fatal("local observation changed outbound Hub queue")
	}
}

func TestWorkspaceTerminalConcurrentWithStartReceiptNeverRevivesLease(t *testing.T) {
	cwd := t.TempDir()
	for i := 0; i < 64; i++ {
		c := &Client{workspaceScanned: true}
		finish := c.beginWorkspaceStart()
		var done sync.WaitGroup
		done.Add(2)
		go func() { defer done.Done(); finish("claude:fast", cwd) }()
		go func() {
			defer done.Done()
			c.ObserveLocalEvent(protocol.Event{Type: "session.updated", Session: &protocol.SessionInfo{SessionKey: "claude:fast", Cwd: cwd, Status: "idle"}})
		}()
		done.Wait()
		if err := c.checkWorkspaceAdmission(context.Background(), &fakeManager{}, cwd, ""); err != nil {
			t.Fatal("terminal event lost to start receipt")
		}
		if c.workspaceStartObserver != nil {
			t.Fatal("start observer was retained")
		}
	}
}
