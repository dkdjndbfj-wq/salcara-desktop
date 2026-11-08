package hubclient

import (
	"context"
	"strings"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

type sessionActionsFixture struct {
	*aggregationFixtureAgent
	calls         int
	before, after func()
	wrong         bool
}

func (a *sessionActionsFixture) check(validate func() error) error {
	a.calls++
	if a.before != nil {
		a.before()
	}
	if err := validate(); err != nil {
		return err
	}
	if a.after != nil {
		a.after()
	}
	return nil
}
func (a *sessionActionsFixture) SessionActions(_ context.Context, id string, validate func() error) (protocol.SessionActions, error) {
	err := a.check(validate)
	if a.wrong {
		id += "-other"
	}
	return protocol.SessionActions{SessionKey: "codex:" + id, Revision: "native-1", Actions: []string{"rename", "archive", "fork"}}, err
}
func (a *sessionActionsFixture) ApplySessionAction(_ context.Context, id string, request protocol.SessionActionRequest, validate func() error) (protocol.SessionActionResult, error) {
	err := a.check(validate)
	if a.wrong {
		id += "-other"
	}
	return protocol.SessionActionResult{SessionKey: "codex:" + id, OperationID: request.OperationID, Action: request.Action, State: "completed"}, err
}
func (a *sessionActionsFixture) ArchivedSessions(_ context.Context, _ string, _ int, validate func() error) ([]protocol.SessionInfo, string, error) {
	err := a.check(validate)
	item := protocol.SessionInfo{SessionKey: "codex:s", Tool: "codex", Archived: true}
	if a.wrong {
		item.Tool = "claude"
	}
	return []protocol.SessionInfo{item}, "", err
}
func sessionActionsCommand(typ string) map[string]any {
	cmd := map[string]any{"type": typ, "tool": "codex", "controlSurface": "cli"}
	if typ != "sessions.archived" {
		cmd["sessionKey"] = "codex:s"
	}
	if typ == "session.action" {
		cmd["action"], cmd["name"], cmd["expectedRevision"], cmd["operationId"] = "rename", "新名称", "native-1", "11111111-2222-4333-8444-555555555555"
	}
	return cmd
}
func TestSessionActionsDispatchBoundary(t *testing.T) {
	for _, typ := range []string{"session.actions", "session.action", "sessions.archived"} {
		for _, variant := range []string{"valid", "disabled", "missing-interface", "missing-surface", "desktop", "foreign", "unknown-field", "bad-account", "wrong-result"} {
			t.Run(typ+"/"+variant, func(t *testing.T) {
				c, base, _ := aggregationFixture(t, "codex")
				a := &sessionActionsFixture{aggregationFixtureAgent: base}
				a.descriptor.Capabilities.SessionActions, a.descriptor.Capabilities.ArchivedSessions = true, true
				c.SetManager(&fakeManager{list: []agents.Agent{a}})
				cmd := sessionActionsCommand(typ)
				switch variant {
				case "disabled":
					a.descriptor.Capabilities.SessionActions, a.descriptor.Capabilities.ArchivedSessions = false, false
				case "missing-interface":
					c.SetManager(&fakeManager{list: []agents.Agent{base}})
				case "missing-surface":
					delete(cmd, "controlSurface")
				case "desktop":
					cmd["controlSurface"] = "desktop"
				case "foreign":
					cmd["agent"] = "claude"
				case "unknown-field":
					cmd["config"] = map[string]any{"model_provider": "other"}
				case "bad-account":
					cmd["accountId"] = 5
				case "wrong-result":
					a.wrong = true
				}
				result, err := c.Dispatch(context.Background(), cmd)
				if (err == nil) != (variant == "valid") {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				if variant != "valid" && variant != "wrong-result" && a.calls != 0 {
					t.Fatal("invalid command reached native adapter")
				}
				if len(a.started)+len(a.sent)+len(a.opened)+a.modelCalls != 0 {
					t.Fatal("management used a generation/history fallback")
				}
			})
		}
	}
}
func TestSessionActionsDispatchSourceFences(t *testing.T) {
	for _, typ := range []string{"session.actions", "session.action", "sessions.archived"} {
		for _, phase := range []string{"before-write", "after-response", "manager-before", "manager-after", "manager-removed-before", "manager-removed-after", "cancelled"} {
			t.Run(typ+"/"+phase, func(t *testing.T) {
				c, base, _ := aggregationFixture(t, "codex")
				a := &sessionActionsFixture{aggregationFixtureAgent: base}
				a.descriptor.Capabilities.SessionActions, a.descriptor.Capabilities.ArchivedSessions = true, true
				c.SetManager(&fakeManager{list: []agents.Agent{a}})
				change := func() {
					if strings.HasPrefix(phase, "manager-removed-") {
						c.SetManager(nil)
						return
					}
					if strings.HasPrefix(phase, "manager-") {
						c.SetManager(&fakeManager{list: []agents.Agent{&sessionActionsFixture{aggregationFixtureAgent: base}}})
						return
					}
					if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.CodexModel = "changed"; return nil }); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if phase == "before-write" || strings.HasSuffix(phase, "-before") {
					a.before = change
				} else if phase == "after-response" || strings.HasSuffix(phase, "-after") {
					a.after = change
				} else {
					cancel()
				}
				if _, err := c.Dispatch(ctx, sessionActionsCommand(typ)); err == nil {
					t.Fatal("stale source/cancelled action succeeded")
				}
			})
		}
	}
}
func TestSessionActionParameterValidation(t *testing.T) {
	for _, variant := range []string{"slash", "empty", "long", "newline", "bidi", "invalid-operation", "invalid-revision", "path", "model"} {
		t.Run(variant, func(t *testing.T) {
			c, base, _ := aggregationFixture(t, "codex")
			a := &sessionActionsFixture{aggregationFixtureAgent: base}
			a.descriptor.Capabilities.SessionActions = true
			c.SetManager(&fakeManager{list: []agents.Agent{a}})
			cmd := sessionActionsCommand("session.action")
			switch variant {
			case "slash":
				cmd["action"] = "/fork"
			case "empty":
				cmd["name"] = ""
			case "long":
				cmd["name"] = strings.Repeat("中", 81)
			case "newline":
				cmd["name"] = "name\nother"
			case "bidi":
				cmd["name"] = "name\u202eother"
			case "invalid-operation":
				cmd["operationId"] = "guess"
			case "invalid-revision":
				cmd["expectedRevision"] = ""
			default:
				cmd[variant] = "external"
			}
			if _, err := c.Dispatch(context.Background(), cmd); err == nil || a.calls != 0 {
				t.Fatalf("invalid action reached adapter: err=%v calls=%d", err, a.calls)
			}
		})
	}
}

func TestSessionActionsSourceGuardRejectsABA(t *testing.T) {
	for _, typ := range []string{"session.actions", "session.action", "sessions.archived"} {
		t.Run(typ, func(t *testing.T) {
			c, store, _, env := sourceGuardFixture(t, "api_key")
			_, base, _ := aggregationFixture(t, "codex")
			a := &sessionActionsFixture{aggregationFixtureAgent: base}
			a.descriptor.Capabilities.SessionActions, a.descriptor.Capabilities.ArchivedSessions = true, true
			c.SetManager(&fakeManager{list: []agents.Agent{a}})
			env.Command = sessionActionsCommand(typ)
			env.Command["accountId"] = env.Source.CredentialHandle
			env.Source.Model = ""
			a.before = func() {
				for _, key := range []string{"key-b", "key-a"} {
					if err := store.Update(func(cfg *config.Config) error { cfg.LocalAccounts[0].Key = key; return nil }); err != nil {
						t.Fatal(err)
					}
				}
			}
			if value, err := c.Dispatch(withExecutionSource(context.Background(), env), env.Command); err == nil || !strings.Contains(err.Error(), "session_action_source_changed") {
				t.Fatalf("ABA result=%+v err=%v", value, err)
			}
		})
	}
}
