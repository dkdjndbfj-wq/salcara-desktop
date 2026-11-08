package hubclient

import (
	"errors"
	"strings"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

// agentAPIRoute is the small, normalized join between the Agent registry and
// the API-account directory. Keeping it here prevents dispatch, station
// handover and status publication from each inventing their own
// claude-desktop/CLI/provider mapping.
type agentAPIRoute struct {
	ID         string
	Family     string
	Descriptor protocol.AgentDescriptor
}

func builtinAPIRoute(id string) (agentAPIRoute, bool) {
	switch id {
	case "codex", "codex-desktop":
		return agentAPIRoute{ID: id, Family: "codex", Descriptor: protocol.AgentDescriptor{ID: id, Name: "Codex", Tool: "codex", APIFamily: "codex", ControlSurface: "cli"}}, true
	case "claude", "claude-desktop":
		return agentAPIRoute{ID: id, Family: "claude", Descriptor: protocol.AgentDescriptor{ID: id, Name: "Claude Code", Tool: "claude", APIFamily: "claude", ControlSurface: "cli"}}, true
	default:
		return agentAPIRoute{}, false
	}
}

// routeForAgent resolves a public Agent id to one API family. Unknown ids are
// rejected instead of being guessed as Codex or Claude; this is the key safety
// property that lets new adapters be added without cross-routing their keys.
func (c *Client) routeForAgent(id string) (agentAPIRoute, error) {
	id = strings.TrimSpace(id)
	if route, ok := builtinAPIRoute(id); ok {
		return route, nil
	}
	if m := c.manager(); m != nil {
		if a := m.Get(id); a != nil {
			d, valid := agents.CanonicalDescriptor(a)
			if !valid || d.ControlSurface != "cli" {
				return agentAPIRoute{}, errors.New("不支持的 Agent 控制方式")
			}
			family := strings.TrimSpace(d.APIFamily)
			if family == "" {
				family = d.Tool
			}
			family = config.NormalizeAgentFamily(family)
			if config.ValidAgentFamily(family) && d.ID == id {
				d.APIFamily = family
				return agentAPIRoute{ID: id, Family: family, Descriptor: d}, nil
			}
		}
	}
	return agentAPIRoute{}, errors.New("不支持的 Agent")
}

// routesForFamily returns every adapter sharing an API family. A family is
// normally the adapter id, but adapters may explicitly share a provider
// family with another control surface. Callers that mutate a family must
// inspect all of these routes to avoid leaving a sibling turn in flight.
func (c *Client) routesForFamily(family string) []agentAPIRoute {
	family = config.NormalizeAgentFamily(family)
	out := []agentAPIRoute{}
	seen := map[string]bool{}
	if family == "codex" || family == "claude" {
		out = append(out, agentAPIRoute{ID: family, Family: family})
		seen[family] = true
	}
	if m := c.manager(); m != nil {
		for _, a := range m.Agents() {
			if a == nil {
				continue
			}
			d, valid := agents.CanonicalDescriptor(a)
			if !valid || d.ControlSurface != "cli" {
				continue
			}
			candidate := config.NormalizeAgentFamily(d.APIFamily)
			if candidate == "" {
				candidate = config.NormalizeAgentFamily(d.Tool)
			}
			if candidate == family && config.ValidAgentFamily(candidate) && !seen[d.ID] {
				d.APIFamily = candidate
				out = append(out, agentAPIRoute{ID: d.ID, Family: candidate, Descriptor: d})
				seen[d.ID] = true
			}
		}
	}
	return out
}

func (c *Client) routeForFamily(family string) (agentAPIRoute, bool) {
	routes := c.routesForFamily(family)
	if len(routes) == 0 {
		return agentAPIRoute{}, false
	}
	return routes[0], true
}

func (c *Client) remoteAPIFamilies() []string {
	seen := map[string]bool{}
	out := make([]string, 0, 2)
	add := func(family string) {
		family = config.NormalizeAgentFamily(family)
		if config.ValidAgentFamily(family) && !seen[family] {
			seen[family] = true
			out = append(out, family)
		}
	}
	add("codex")
	add("claude")
	if m := c.manager(); m != nil {
		for _, a := range m.Agents() {
			if a == nil {
				continue
			}
			d, valid := agents.CanonicalDescriptor(a)
			if valid && d.ControlSurface == "cli" {
				add(d.APIFamily)
			}
		}
	}
	return out
}

func (c *Client) apiFamilyForAgent(id string) string {
	if route, err := c.routeForAgent(id); err == nil {
		return route.Family
	}
	return config.NormalizeAgentFamily(id)
}
