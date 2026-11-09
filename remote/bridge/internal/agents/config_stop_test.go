package agents

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestConfigurationStopRequiresExplicitTreeReceipt(t *testing.T) {
	var tree processTreeTermination
	if tree.confirmed() {
		t.Fatal("zero value proved descendants stopped")
	}
	tree.begin()
	tree.finish(nil)
	if !tree.confirmed() {
		t.Fatal("explicit success receipt missing")
	}
	tree.begin()
	tree.finish(errors.New("unknown"))
	if tree.confirmed() || !tree.unconfirmed() {
		t.Fatal("failed stop erased fence")
	}
	// No real process is spawned or signalled: the already-reaped synthetic
	// root illustrates that Wait alone says nothing about descendants.
	done := make(chan struct{})
	close(done)
	c := &rpcConn{cmd: &exec.Cmd{ProcessState: &os.ProcessState{}}, done: done, processWaitDone: done}
	a := newCodexAgent(nil, func() Settings { return Settings{} }, "", "")
	a.conn = c
	if err := a.StopForAPIChange(context.Background()); err == nil || a.unconfirmedProcess != c || !c.treeTermination.unconfirmed() {
		t.Fatal("natural root exit was accepted as a tree receipt", err)
	}
}

func TestConfigurationStopCancellationDoesNotCloseAdapter(t *testing.T) {
	a := newCodexAgent(nil, func() Settings { return Settings{} }, "", "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(a.StopForAPIChange(ctx), context.Canceled) || a.closed {
		t.Fatal("cancelled stop permanently closed adapter")
	}
	a.sideQuestions.active = 1
	if err := a.StopForAPIChange(context.Background()); err == nil || a.closed {
		t.Fatal("uncancellable side question was treated as stopped")
	}
}

func TestHermesConfigurationStopRequestedIsNotCompletion(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		t.Run(map[bool]string{false: "requested-only", true: "observed-idle"}[confirmed], func(t *testing.T) {
			a, f, _, snapshot := hermesFixture(t)
			snapshot.Generation = 1
			hermesStart(t, a, snapshot)
			snapshot.Running = true
			original := f.call
			f.call = func(method string, p map[string]any) (any, error) {
				v, err := original(method, p)
				if method == "session.interrupt" && confirmed {
					snapshot.Running = false
				}
				return v, err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 180*time.Millisecond)
			defer cancel()
			err := a.StopForAPIChange(ctx)
			if confirmed && err != nil {
				t.Fatal(err)
			}
			if !confirmed && (err == nil || !a.ActiveRemoteTurns()) {
				t.Fatal("interrupt request authorized API replacement")
			}
			if !strings.Contains(strings.Join(f.calls, ","), "session.interrupt,session.resume") {
				t.Fatal("missing post-interrupt authoritative state", f.calls)
			}
			if a.closed {
				t.Fatal("API stop permanently closed adapter")
			}
		})
	}
}

func TestDeepSeekConfigurationStopPreservesRecoveredUnknownFence(t *testing.T) {
	a := newDeepSeekAgent(func() Settings { return Settings{} }, "")
	a.sessions["unknown"] = &deepSeekSession{Unconfirmed: true, RuntimePending: true, busy: false}
	if err := a.StopForAPIChange(context.Background()); err == nil || !a.sessions["unknown"].Unconfirmed || !a.sessions["unknown"].RuntimePending {
		t.Fatal("nil connection erased persisted runtime fence")
	}
}

func TestCodexConfigurationStopRejectsRetiredNaturalRoot(t *testing.T) {
	done := make(chan struct{})
	close(done)
	c := &rpcConn{cmd: &exec.Cmd{Process: &os.Process{}, ProcessState: &os.ProcessState{}}, done: done, processWaitDone: done}
	a := newCodexAgent(nil, func() Settings { return Settings{} }, "", "")
	a.conn = c
	a.onExit(c)
	if a.conn != nil || a.ActiveRemoteTurns() {
		t.Fatal("fixture did not retire normal registry")
	}
	if !a.apiChangeRetiredUnknown || a.StopForAPIChange(context.Background()) == nil {
		t.Fatal("retired root disappearance permitted API replacement")
	}
	// A normal reconnect/registry cleanup cannot turn a lost owner into proof.
	a.mu.Lock()
	a.markUnloadedLocked()
	a.mu.Unlock()
	if a.StopForAPIChange(context.Background()) == nil {
		t.Fatal("ordinary cleanup cleared safe fence")
	}
}

func TestClaudeConfigurationStopRejectsRetiredNaturalRoot(t *testing.T) {
	done := make(chan struct{})
	close(done)
	a := newClaudeAgent(nil, func() Settings { return Settings{} }, "", t.TempDir())
	p := &claudeProc{id: "synthetic-retired", featureProbe: true, cmd: &exec.Cmd{Process: &os.Process{}, ProcessState: &os.ProcessState{}}, processWaitDone: done, done: make(chan struct{})}
	a.procs[p.id] = p
	a.workers[p] = struct{}{}
	a.onExit(p, nil)
	if len(a.procs) != 0 || len(a.workers) != 0 || a.ActiveRemoteTurns() {
		t.Fatal("fixture did not retire normal registry")
	}
	if !a.apiChangeRetiredUnknown || a.StopForAPIChange(context.Background()) == nil {
		t.Fatal("retired Claude root permitted API replacement")
	}
}
