package agents

import (
	"context"
	"errors"
	"testing"
)

func TestPendingTreeCleanupFencesBeforeTransportRetirement(t *testing.T) {
	p := &claudeProc{stopCh: make(chan struct{}), done: make(chan struct{})}
	p.treeTermination.begin()
	a := &claudeAgent{workers: map[*claudeProc]struct{}{p: {}}}
	a.mu.Lock()
	fenced := a.hasUnconfirmedWorkerLocked()
	a.mu.Unlock()
	if !fenced {
		t.Fatal("pending Claude tree cleanup required stopCh/done closure to fence startup")
	}
	if next, err := a.spawn(context.Background(), "another-session", t.TempDir(), "", "ask", false); next != nil || !errors.Is(err, errClaudeProcessExitUnconfirmed) {
		t.Fatal("pending Claude tree cleanup admitted another session", err)
	}
	c := &rpcConn{killCh: make(chan struct{})}
	c.treeTermination.begin()
	if c.alive() {
		t.Fatal("pending RPC tree cleanup remained eligible for connection reuse")
	}
	// stdin is deliberately nil: this must be rejected before any pipe write.
	if attempted, err := c.writeContext(context.Background(), map[string]any{"method": "turn/start"}); attempted || !errors.Is(err, errRPCProcessExitUnconfirmed) {
		t.Fatal("pending RPC cleanup attempted a new command", attempted, err)
	}
	// A successful termination result must not revive the killed transport
	// during the interval before shutdown publishes done.
	close(c.killCh)
	c.treeTermination.finish(nil)
	if c.alive() {
		t.Fatal("finished tree cleanup revived a retired RPC transport")
	}
}

// Wait can compute its root-only error before a concurrent explicit cleanup.
// shutdown must apply the tree state at final publication, before closing done.
func TestRPCTreeTerminationOverridesPrecomputedRootExit(t *testing.T) {
	for _, failed := range []bool{false, true} {
		c := &rpcConn{done: make(chan struct{})}
		c.treeTermination.begin()
		if failed {
			c.treeTermination.finish(errors.New("synthetic failed terminator"))
		}
		c.shutdown(errors.New("previously computed root-only exit"))
		<-c.done
		if !errors.Is(c.err, errRPCProcessExitUnconfirmed) {
			t.Fatal("final RPC error publication lost pending/failed tree state")
		}
		published := c.err
		c.treeTermination.finish(errors.New("synthetic late failure"))
		c.shutdown(errors.New("late duplicate shutdown"))
		if c.err != published {
			t.Fatal("RPC error changed after done publication")
		}
	}
}
