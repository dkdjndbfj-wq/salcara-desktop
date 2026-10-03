package hubclient

import (
	"context"
	"testing"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/protocol"
)

type updateAgent struct {
	*fakeAgent
	busy bool
}

func TestAbandonedUpdateLeaseAndCancelledPrepareRestoreAdmission(t *testing.T) {
	a := &updateAgent{fakeAgent: &fakeAgent{id: "codex"}}
	c := New(Options{})
	c.SetManager(&fakeManager{list: []agents.Agent{a}})
	if err := c.PrepareUpdate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !c.CancelUpdate() || c.updatePrepared {
		t.Fatal("cancel did not restore admission")
	}
	if err := c.PrepareUpdate(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.updateUntil = time.Now().Add(-time.Second)
	if err := c.CommitUpdate(); err == nil || c.updatePrepared {
		t.Fatal("expired prepare committed")
	}
	if err := c.PrepareUpdate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.CommitUpdate(); err != nil {
		t.Fatal(err)
	}
	if c.CancelUpdate() {
		t.Fatal("committed shutdown admitted new tasks")
	}
}

type updateDesktop struct {
	routeDesktop
	sessions []protocol.SessionInfo
}

func (d *updateDesktop) NativeList(context.Context) ([]protocol.SessionInfo, error) {
	return d.sessions, nil
}

func TestUpdatePreparationPreservesActiveNativeDesktopTasks(t *testing.T) {
	a := &updateAgent{fakeAgent: &fakeAgent{id: "codex"}}
	d := &updateDesktop{routeDesktop: routeDesktop{st: liveDesktop("codex:01a0ae56-e9f6-4933-9ad4-5e08cd7874e5")}, sessions: []protocol.SessionInfo{{Status: "running"}}}
	c := New(Options{Desktop: d})
	c.SetManager(&fakeManager{list: []agents.Agent{a}})
	if c.PrepareUpdate(context.Background()) == nil || c.updatePrepared {
		t.Fatal("active native task allowed update")
	}
	d.sessions = nil
	if err := c.PrepareUpdate(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (a *updateAgent) ActiveRemoteTurns() bool { return a.busy }

func TestUpdatePreparationNeverInterruptsBusyTasksAndClosesAdmission(t *testing.T) {
	a := &updateAgent{fakeAgent: &fakeAgent{id: "codex"}, busy: true}
	c := New(Options{})
	c.SetManager(&fakeManager{list: []agents.Agent{a}})
	if c.PrepareUpdate(context.Background()) == nil {
		t.Fatal("busy task admitted update")
	}
	if c.updatePrepared || len(a.stopped) != 0 {
		t.Fatal("busy update changed live state")
	}
	a.busy = false
	if err := c.PrepareUpdate(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, typ := range []string{"session.start", "session.send", "desktop.session.send", "agents.api.set"} {
		if _, err := c.Dispatch(context.Background(), map[string]any{"type": typ}); err == nil {
			t.Fatalf("%s admitted during update", typ)
		}
	}
	if _, err := c.Dispatch(context.Background(), map[string]any{"type": "device.ping", "nonce": "test"}); err != nil {
		t.Fatal(err)
	}
}

func TestUpdatePreparationRejectsInFlightAdmissionAndUnknownActivity(t *testing.T) {
	c := New(Options{})
	c.taskConfigMu.Lock()
	if c.PrepareUpdate(context.Background()) == nil {
		t.Fatal("in-flight admission accepted")
	}
	c.taskConfigMu.Unlock()
	c.SetManager(&fakeManager{list: []agents.Agent{&fakeAgent{id: "codex"}}})
	if c.PrepareUpdate(context.Background()) == nil {
		t.Fatal("unknown activity accepted")
	}
}
