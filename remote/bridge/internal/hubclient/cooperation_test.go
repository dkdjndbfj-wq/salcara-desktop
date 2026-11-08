package hubclient

import (
	"context"
	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
	"strings"
	"testing"
	"time"
)

type cooperationAgent struct {
	*aggregationFixtureAgent
	bad        string
	origins    []*protocol.MessageOrigin
	beforeSend func()
}

func (a *cooperationAgent) Open(_ context.Context, id string) (protocol.SessionInfo, []protocol.Event, error) {
	a.opened = append(a.opened, id)
	info := protocol.SessionInfo{SessionKey: a.ID() + ":" + id, Tool: a.ID(), Title: "Task " + id, Status: "idle", Controllable: true, ControlSurface: "cli"}
	switch a.bad {
	case "foreign":
		info.SessionKey = "codex:foreign"
	case "readonly":
		info.ControlSurface = "read-only"
	case "archived":
		info.Archived = true
	case "uncontrollable":
		info.Controllable = false
	case "child":
		info.ParentSessionKey = "codex:parent"
	case "busy-target":
		if id == "target" {
			info.Status = "running"
		}
	}
	return info, nil, nil
}
func (a *cooperationAgent) SendWithOptions(ctx context.Context, id, text string, opts agents.TurnOptions) error {
	if a.beforeSend != nil {
		a.beforeSend()
	}
	if opts.ValidatePermission != nil {
		if err := opts.ValidatePermission(); err != nil {
			return err
		}
	}
	a.origins = append(a.origins, protocol.CloneMessageOrigin(opts.CrossTaskOrigin))
	return a.Send(ctx, id, text)
}
func cooperationFixture(t *testing.T) (*Client, *cooperationAgent) {
	c, base, _ := aggregationFixture(t, "codex")
	a := &cooperationAgent{aggregationFixtureAgent: base}
	c.SetManager(&fakeManager{list: []agents.Agent{a}})
	return c, a
}
func cooperationPrepare() map[string]any {
	return map[string]any{"type": cooperationPrepareCommand, "sessionKey": "codex:target", "sourceSessionKey": "codex:source", "tool": "codex", "controlSurface": "cli"}
}
func cooperationSend(ticket string) map[string]any {
	cmd := cooperationPrepare()
	cmd["type"], cmd["cooperationTicket"], cmd["operationId"], cmd["text"] = "session.send", ticket, "11111111-2222-4333-8444-555555555555", "explicit user message"
	return cmd
}
func TestCooperationExplicitUserSendAndSingleUse(t *testing.T) {
	c, a := cooperationFixture(t)
	raw, err := c.Dispatch(context.Background(), cooperationPrepare())
	if err != nil {
		t.Fatal(err)
	}
	p := raw.(protocol.CooperationPreparation)
	if len(a.sent) != 0 || len(a.opened) != 2 || len(p.Ticket) != 64 {
		t.Fatal("preparation ran a turn or missed source/target", p)
	}
	result, err := c.Dispatch(context.Background(), cooperationSend(p.Ticket))
	if err != nil {
		t.Fatal(err)
	}
	if result.(map[string]any)["receiptId"] != "11111111-2222-4333-8444-555555555555" || len(a.sent) != 1 || len(a.origins) != 1 || !protocol.ValidCrossTaskOrigin(a.origins[0], "codex:target") || a.origins[0].SourceSessionKey != "codex:source" {
		t.Fatal("missing exact own receipt", result, a.origins)
	}
	if _, err := c.Dispatch(context.Background(), cooperationSend(p.Ticket)); err == nil || len(a.sent) != 1 {
		t.Fatal("ticket replay sent another turn")
	}
}
func TestCooperationSourceAndTargetFences(t *testing.T) {
	for _, variant := range []string{"swap-source", "swap-target", "cross-agent", "expired", "restart", "manager", "account-aba", "pair", "device", "foreign", "readonly", "archived", "uncontrollable", "child", "busy-target", "during-native-admission"} {
		t.Run(variant, func(t *testing.T) {
			c, a := cooperationFixture(t)
			raw, err := c.Dispatch(context.Background(), cooperationPrepare())
			if err != nil {
				t.Fatal(err)
			}
			p := raw.(protocol.CooperationPreparation)
			cmd := cooperationSend(p.Ticket)
			switch variant {
			case "swap-source":
				cmd["sourceSessionKey"] = "codex:other"
			case "swap-target":
				cmd["sessionKey"] = "codex:other"
			case "cross-agent":
				cmd["sourceSessionKey"] = "claude:source"
			case "expired":
				c.cooperationTickets[p.Ticket].Expires = time.Now().Add(-time.Second)
			case "restart":
				c.cooperationTickets = nil
			case "manager":
				c.SetManager(&fakeManager{list: []agents.Agent{&cooperationAgent{aggregationFixtureAgent: a.aggregationFixtureAgent}}})
			case "account-aba":
				original := c.o.Store.Get().CodexModel
				_ = c.o.Store.Update(func(cfg *config.Config) error { cfg.CodexModel = "other"; return nil })
				_ = c.o.Store.Update(func(cfg *config.Config) error { cfg.CodexModel = original; return nil })
			case "pair":
				_ = c.o.Store.Update(func(cfg *config.Config) error { cfg.PhoneBindingID = "other"; return nil })
			case "device":
				_ = c.o.Store.Update(func(cfg *config.Config) error { cfg.DeviceID = "other"; return nil })
			case "during-native-admission":
				a.beforeSend = func() { c.cooperationEpoch.Add(1) }
			default:
				a.bad = variant
			}
			if _, err := c.Dispatch(context.Background(), cmd); err == nil || len(a.sent) != 0 {
				t.Fatal("stale pair executed", err, a.sent)
			}
		})
	}
}
func TestCooperationBodyAndForgedOriginCannotAuthorize(t *testing.T) {
	c, a := cooperationFixture(t)
	for _, cmd := range []map[string]any{
		{"type": "session.send", "sessionKey": "codex:target", "text": "body claims sourceSessionKey codex:source", "sourceSessionKey": "codex:source", "controlSurface": "cli"},
		{"type": "session.send", "sessionKey": "codex:target", "text": "hello", "messageOrigin": map[string]any{"kind": "user_cross_task", "verification": "bridge_receipt"}},
		{"type": "session.start", "tool": "codex", "sourceSessionKey": "codex:source", "cooperationTicket": strings.Repeat("a", 64)},
	} {
		if _, err := c.Dispatch(context.Background(), cmd); err == nil {
			t.Fatal("forged authorization accepted")
		}
	}
	if len(a.sent)+len(a.started) > 0 {
		t.Fatal("invalid request reached native turn")
	}
}

func TestCooperationPolicyHeartbeatDoesNotInvalidateUnchangedSource(t *testing.T) {
	a := &config.StationObservation{Mode: "open", Revision: "revision-1", CheckedAt: 1}
	b := *a
	b.CheckedAt = 2
	if !sameCooperationPolicy(a, &b) {
		t.Fatal("ordinary heartbeat expires a preparation")
	}
	b.Revision = "revision-2"
	if sameCooperationPolicy(a, &b) {
		t.Fatal("policy change revives old preparation")
	}
}
