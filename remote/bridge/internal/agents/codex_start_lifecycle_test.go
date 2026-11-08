package agents

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/protocol"
)

func TestCodexClosedAgentRefusesLateNativeRequestDispatch(t *testing.T) {
	a := newCodexAgent(func(protocol.Event) {}, func() Settings { return Settings{} }, "not-launched", "")
	writer := &rpcFixtureWriter{calls: make(chan []byte, 1)}
	c := &rpcConn{stdin: writer, pending: map[int64]chan rpcResult{}, done: make(chan struct{})}
	a.installCodexRequestHandler(c)
	a.Close()
	// This malformed request would synchronously reply before onRequest's
	// normal state checks if a late goroutine were still admitted after Close.
	c.handleLine([]byte(`{"id":1,"method":"item/tool/requestUserInput","params":{}}`))
	select {
	case <-writer.calls:
		t.Fatal("closed agent admitted a late native request handler")
	case <-time.After(50 * time.Millisecond):
	}
	a.wg.Wait()
}

func TestRPCRequestHandlerReplacementUsesLockedSnapshots(t *testing.T) {
	c := &rpcConn{}
	var calls atomic.Int64
	var workers sync.WaitGroup
	for range 6 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 100 {
				c.setRequestHandler(func(json.RawMessage, string, json.RawMessage) { calls.Add(1) })
				c.handleLine([]byte(`{"id":1,"method":"synthetic/request","params":{}}`))
			}
		}()
	}
	workers.Wait()
	if calls.Load() != 600 {
		t.Fatal("request handler snapshot was lost during replacement")
	}
}

func TestCodexCloseHasBoundedWaitForNonCooperativeTransport(t *testing.T) {
	a := newCodexAgent(func(protocol.Event) {}, func() Settings { return Settings{} }, "not-launched", "")
	// A synthetic transport with no OS process models a reader held inside a
	// non-cooperative event callback; closing its pipes cannot finish done.
	a.conn = &rpcConn{done: make(chan struct{}), pending: map[int64]chan rpcResult{}}
	finished := make(chan struct{})
	go func() { a.Close(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(6 * time.Second):
		t.Fatal("explicit Close waited forever for a non-cooperative reader")
	}
}

func TestCodexCloseUsesOneDeadlineForTransportAndWorkerDrain(t *testing.T) {
	a := newCodexAgent(func(protocol.Event) {}, func() Settings { return Settings{} }, "not-launched", "")
	transportDone := make(chan struct{})
	a.conn = &rpcConn{done: transportDone, pending: map[int64]chan rpcResult{}}
	// The owned reader and its watcher remain behind the same synthetic sink.
	// Sequential five-second waits used to take ten seconds even without any
	// Windows terminator or OS process. Neither wait may reset the budget.
	a.wg.Add(1)
	t.Cleanup(func() { close(transportDone); a.wg.Done() })
	finished := make(chan struct{})
	go func() { a.Close(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(codexCloseDrainTimeout + time.Second):
		t.Fatal("Close reset its drain budget while waiting on the same sink")
	}
}

func TestCodexCloseDrainDoesNotResetExpiredDeadline(t *testing.T) {
	deadline := time.Now().Add(-time.Second)
	started := time.Now()
	waitCodexCloseDrain(make(chan struct{}), deadline)
	if time.Since(started) > 100*time.Millisecond {
		t.Fatal("expired shared drain deadline started another wait")
	}
}
