package agents

import (
	"encoding/json"

	"salcara/bridge/internal/protocol"
)

func (a *codexAgent) installCodexNotificationHandler(c *rpcConn) {
	c.setNotificationHandler(func(method string, params json.RawMessage) {
		a.mu.Lock()
		current := !a.closed && (a.conn == c || a.starting != nil && a.starting.conn == c && a.starting.ctx.Err() == nil)
		a.mu.Unlock()
		if !current {
			return
		}
		if method != "skills/changed" {
			if !a.acceptCodexContextNotification(c, method, params) {
				return
			}
			a.onNotify(method, params)
			a.onCodexContextNotify(c, method, params)
			a.onCodexWorkflowNotify(c, method, params)
			return
		}
		// The native notification has no cwd, thread or file identity. Invalidate
		// this agent's whole catalog, without inventing a content revision. Never
		// read skills/list from the RPC reader: its response needs this same reader.
		a.emit(protocol.Event{Type: "features.changed"})
	})
}
