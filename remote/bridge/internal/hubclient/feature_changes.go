package hubclient

import (
	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/protocol"
)

func (c *Client) invalidateAgentFeatures(ev protocol.Event) (protocol.Event, bool) {
	m := c.manager()
	if m == nil || ev.SessionKey != "" || ev.Session != nil {
		return protocol.Event{}, false
	}
	a := m.Get(ev.Tool)
	if a == nil {
		return protocol.Event{}, false
	}
	d, valid := agents.CanonicalDescriptor(a)
	if !valid || d.ID != ev.Tool || d.ControlSurface != "cli" || !d.Capabilities.Features {
		return protocol.Event{}, false
	}
	// Clear locally even while unpaired/offline; upload admission is separate.
	c.featureCatalog.invalidateAgent(ev.Tool)
	return protocol.Event{Type: "features.changed", Tool: ev.Tool, TS: ev.TS}, true
}

// qmu is held. Repeated saves need only one outstanding invalidation per agent,
// but an immutable in-flight batch cannot absorb a later change.
func (c *Client) coalesceFeatureChangeLocked(ev protocol.Event) bool {
	if ev.Type != "features.changed" {
		return false
	}
	for i := len(c.queue) - 1; i >= c.inFlight; i-- {
		previous := c.queue[i]
		if previous.Type == ev.Type && previous.Tool == ev.Tool && previous.SessionKey == "" {
			c.queueBytes += eventWeight(ev) - eventWeight(previous)
			c.queue[i] = ev
			return true
		}
	}
	return false
}
