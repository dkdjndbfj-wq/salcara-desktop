package hubclient

import (
	"context"
	"strings"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

type claudeModeFixture struct {
	*modelOptionsAgent
	modes []string
	full  bool
}

func (a *claudeModeFixture) Descriptor() protocol.AgentDescriptor {
	d := aggregationDescriptor(a.ID())
	d.Capabilities.ApprovalModes = a.modes
	d.Capabilities.Continue = true
	return d
}
func (a *claudeModeFixture) SessionRequiresFullAccessConsent(string) bool { return a.full }
func (a *claudeModeFixture) ContinueWithOptions(ctx context.Context, id, text, expected, operation string, opts agents.TurnOptions) (protocol.ContinueResult, error) {
	a.sendOptions = append(a.sendOptions, opts)
	return protocol.ContinueResult{Accepted: true}, nil
}

func TestClaudeStartRequiresExplicitPhoneChoiceInsteadOfComputerBypassDefault(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		c, base, cwd := aggregationFixture(t, "claude")
		a := &modelOptionsAgent{fakeAgent: base.fakeAgent}
		c.SetManager(&fakeManager{list: []agents.Agent{a}})
		if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.Approval = "auto_all"; cfg.AllowPhoneAutoAll = true; return nil }); err != nil {
			t.Fatal(err)
		}
		cmd := map[string]any{"type": "session.start", "tool": "claude", "cwd": cwd, "prompt": "synthetic"}
		if explicit {
			cmd["approval"] = "auto_all"
		}
		if _, err := c.Dispatch(context.Background(), cmd); err != nil {
			t.Fatal(err)
		}
		want := "|ask"
		if explicit {
			want = "|auto_all"
		}
		if len(a.started) != 1 || !strings.HasSuffix(a.started[0], want) {
			t.Fatal("desktop default escalated absent phone choice", a.started)
		}
	}
}

func TestClaudeNextTurnPermissionContractAndLiveRevocation(t *testing.T) {
	for _, typ := range []string{"session.send", "session.continue"} {
		for _, mode := range []string{"ask", "auto_edits", "auto_all"} {
			c, base, _ := aggregationFixture(t, "claude")
			a := &claudeModeFixture{modelOptionsAgent: &modelOptionsAgent{fakeAgent: base.fakeAgent}, modes: []string{"ask", "auto_edits", "auto_all"}, full: true}
			c.SetManager(&fakeManager{list: []agents.Agent{a}})
			if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.AllowPhoneAutoAll = mode == "auto_all"; return nil }); err != nil {
				t.Fatal(err)
			}
			cmd := map[string]any{"type": typ, "sessionKey": "claude:s", "text": "synthetic", "controlSurface": "cli", "approval": mode, "expectedTurnId": "finished", "operationId": "10000000-0000-4000-8000-000000000001"}
			if _, err := c.Dispatch(context.Background(), cmd); err != nil {
				t.Fatalf("%s/%s: %v", typ, mode, err)
			}
			if len(a.sendOptions) != 1 || a.sendOptions[0].ClaudeApproval != mode {
				t.Fatal("native policy not delivered")
			}
			if mode == "auto_all" {
				guard := a.sendOptions[0].ValidatePermission
				if guard == nil {
					t.Fatal("missing live consent")
				}
				if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.AllowPhoneAutoAll = false; return nil }); err != nil {
					t.Fatal(err)
				}
				if guard() == nil {
					t.Fatal("late consent revocation ignored")
				}
				if _, err := c.Dispatch(context.Background(), cmd); err == nil {
					t.Fatal("revoked full access admitted")
				}
			}
			a.modes = nil
			if _, err := c.Dispatch(context.Background(), cmd); err == nil {
				t.Fatal("undeclared switch admitted")
			}
		}
	}
}

func TestClaudePermissionValuesCannotLeakToCodexOrNativeSurface(t *testing.T) {
	c, base, _ := aggregationFixture(t, "codex")
	a := &claudeModeFixture{modelOptionsAgent: &modelOptionsAgent{fakeAgent: base.fakeAgent}, modes: []string{"auto_all"}}
	if len(publishedRemoteCapabilities(a, a.Descriptor().Capabilities).ApprovalModes) != 0 {
		t.Fatal("Claude mode leaked to Codex")
	}
	if _, err := commandClaudeApproval(map[string]any{"approval": "ask", "type": "session.send"}, a, config.Config{}); err == nil {
		t.Fatal("Codex accepted Claude enum")
	}
	if _, err := c.dispatchNativeSend(context.Background(), map[string]any{"approval": "ask"}); err == nil || !strings.Contains(err.Error(), "permission_mode_unsupported") {
		t.Fatal("native send silently discarded approval")
	}
}
