package agents

import (
	"context"
	"encoding/json"
	"time"
)

type codexStartupLease struct {
	ctx    context.Context
	cancel context.CancelFunc
	conn   *rpcConn // accessed only with a.mu, including before publication
}

// The caller holds a.mu and startMu and has checked the closed admission gate.
// Counting initialization before unlocking means Close cannot miss an owned
// process or finish a zero-counter Wait before a late watcher is registered.
func (a *codexAgent) beginStartupLocked(ctx context.Context) *codexStartupLease {
	childCtx, cancel := context.WithCancel(ctx)
	lease := &codexStartupLease{ctx: childCtx, cancel: cancel}
	a.starting = lease
	a.wg.Add(1)
	return lease
}

func (a *codexAgent) finishStartup(lease *codexStartupLease) {
	a.mu.Lock()
	if a.starting == lease {
		a.starting = nil
	}
	a.mu.Unlock()
	lease.cancel()
	a.wg.Done()
}

func (a *codexAgent) installCodexRequestHandler(c *rpcConn) {
	c.setRequestHandler(func(id json.RawMessage, method string, params json.RawMessage) {
		a.mu.Lock()
		if a.closed {
			a.mu.Unlock()
			return
		}
		a.wg.Add(1)
		a.mu.Unlock()
		go func() {
			defer a.wg.Done()
			a.onRequest(c, id, method, params)
		}()
	})
}

// An explicit app close may abandon a non-cooperative local event consumer,
// but does not claim a pending operation was unsent or that OS exit occurred.
const codexCloseDrainTimeout = 5 * time.Second

func waitCodexCloseTransport(c *rpcConn, deadline time.Time) {
	waitCodexCloseDrain(c.done, deadline)
}

func waitCodexCloseDrain(done <-chan struct{}, deadline time.Time) {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}
