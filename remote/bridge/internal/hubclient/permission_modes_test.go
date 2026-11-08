package hubclient

import (
	"context"
	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
	"strings"
	"testing"
)

type permissionOptionAgent struct {
	*aggregationFixtureAgent
	opts  agents.TurnOptions
	known string
}

func (a *permissionOptionAgent) StartWithOptions(_ context.Context, _, _, _ string, opts agents.TurnOptions) (string, error) {
	a.opts = opts
	return "created", nil
}
func (a *permissionOptionAgent) SendWithOptions(_ context.Context, _, _ string, opts agents.TurnOptions) error {
	a.opts = opts
	return nil
}
func (a *permissionOptionAgent) ContinueWithOptions(_ context.Context, _, _, _, _ string, opts agents.TurnOptions) (protocol.ContinueResult, error) {
	a.opts = opts
	return protocol.ContinueResult{Accepted: true}, nil
}
func (a *permissionOptionAgent) KnownSessionPermission(string) string { return a.known }

func TestPermissionModeSurvivesStartSendAndContinueDispatch(t *testing.T) {
	for _, typ := range []string{"session.start", "session.send", "session.continue"} {
		t.Run(typ, func(t *testing.T) {
			c, base, cwd := aggregationFixture(t, "codex")
			a := &permissionOptionAgent{aggregationFixtureAgent: base}
			a.descriptor.Capabilities.PermissionModes = []string{"ask", "auto_review", "full_access"}
			a.descriptor.Capabilities.Continue = true
			c.SetManager(&fakeManager{list: []agents.Agent{a}})
			cmd := map[string]any{"type": typ, "tool": "codex", "sessionKey": "codex:s", "controlSurface": "cli", "cwd": cwd, "prompt": "synthetic", "text": "synthetic", "permissionMode": "auto_review", "expectedTurnId": "terminal", "operationId": "10000000-0000-4000-8000-000000000001"}
			if _, err := c.Dispatch(context.Background(), cmd); err != nil {
				t.Fatal(err)
			}
			if a.opts.PermissionMode != "auto_review" {
				t.Fatal("permission mode dropped")
			}
		})
	}
}

func TestPermissionModeCannotRetainFullAccessAfterDesktopRevokes(t *testing.T) {
	c, base, _ := aggregationFixture(t, "codex")
	a := &permissionOptionAgent{aggregationFixtureAgent: base, known: "full_access"}
	if _, err := commandPermissionMode(map[string]any{"sessionKey": "codex:s"}, a, config.Config{}); err == nil {
		t.Fatal("omitted field retained revoked full access")
	}
	if err := c.permissionConsentGuard(map[string]any{"sessionKey": "codex:s"}, a)(); err == nil {
		t.Fatal("late check failed to revoke inherited full access")
	}
}

type observedPermissionAgent struct {
	*permissionOptionAgent
	inheritedFull bool
}

func (a *observedPermissionAgent) SessionRequiresFullAccessConsent(string) bool {
	return a.inheritedFull
}

func TestPermissionModeFirstResumeCannotDiscoverUnguardedFullAccess(t *testing.T) {
	c, base, _ := aggregationFixture(t, "codex")
	a := &observedPermissionAgent{permissionOptionAgent: &permissionOptionAgent{aggregationFixtureAgent: base}}
	cmd := map[string]any{"sessionKey": "codex:existing"}
	guard := c.permissionConsentGuard(cmd, a)
	if guard == nil || guard() != nil {
		t.Fatal("unknown inherited mode needs a non-mutating late guard")
	}
	// A resume reports an unsandboxed original thread. The phone did not ask
	// to change it, but that must not bypass desktop admission restrictions.
	a.inheritedFull = true
	if guard() == nil {
		t.Fatal("newly observed full access bypassed consent")
	}
	if _, err := commandPermissionMode(cmd, a, config.Config{}); err == nil {
		t.Fatal("next turn bypassed observed full access")
	}
	if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.AllowPhoneAutoAll = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := guard(); err != nil {
		t.Fatal("explicit local consent was ignored", err)
	}
	if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.AllowPhoneAutoAll = false; return nil }); err != nil {
		t.Fatal(err)
	}
	if guard() == nil {
		t.Fatal("late consent revocation was ignored")
	}
	for _, mode := range []string{"ask", "auto_review"} {
		if c.permissionConsentGuard(map[string]any{"sessionKey": "codex:existing", "permissionMode": mode}, a) != nil {
			t.Fatal("safe explicit downgrade incorrectly retained old full-access guard")
		}
	}
}

func TestAggregationTurnControlsRespectExactCapabilities(t *testing.T) {
	_, base, _ := aggregationFixture(t, "codex")
	a := &turnContractFixture{aggregationFixtureAgent: base}
	for _, op := range []string{"continue", "preciseInterrupt", "steer"} {
		if err := requireAgentCapability(a, op); err == nil {
			t.Fatalf("unadvertised %s accepted", op)
		}
	}
	a.descriptor.Capabilities.Continue = true
	a.descriptor.Capabilities.PreciseInterrupt = true
	a.descriptor.Capabilities.Steer = true
	for _, op := range []string{"continue", "preciseInterrupt"} {
		if err := requireAgentCapability(a, op); err != nil {
			t.Fatalf("advertised %s rejected: %v", op, err)
		}
	}
	if requireAgentCapability(a, "steer") == nil {
		t.Fatal("steer declaration invented a dispatcher that does not exist")
	}
	a.descriptor.Capabilities.Send = false
	a.descriptor.Capabilities.Interrupt = false
	for _, op := range []string{"continue", "preciseInterrupt", "steer"} {
		if err := requireAgentCapability(a, op); err == nil {
			t.Fatalf("disabled primitive bypassed by %s", op)
		}
	}
}

func TestPermissionModesValidateCapabilityAndDesktopGrant(t *testing.T) {
	_, base, _ := aggregationFixture(t, "codex")
	a := &permissionOptionAgent{aggregationFixtureAgent: base}
	for _, raw := range []any{nil, true, "", "auto_all", "FULL_ACCESS", map[string]any{"mode": "full_access"}} {
		if _, err := commandPermissionMode(map[string]any{"permissionMode": raw}, a, config.Config{AllowPhoneAutoAll: true}); err == nil {
			t.Fatalf("malformed mode accepted: %v", raw)
		}
	}
	if _, err := commandPermissionMode(map[string]any{"permissionMode": "ask"}, a, config.Config{}); err == nil {
		t.Fatal("unadvertised capability accepted")
	}
	a.descriptor.Capabilities.PermissionModes = []string{"ask", "auto_review", "full_access"}
	for _, mode := range a.descriptor.Capabilities.PermissionModes {
		got, err := commandPermissionMode(map[string]any{"permissionMode": mode}, a, config.Config{AllowPhoneAutoAll: true})
		if err != nil || got != mode {
			t.Fatalf("supported %s: %v", mode, err)
		}
	}
	if _, err := commandPermissionMode(map[string]any{"permissionMode": "full_access"}, a, config.Config{}); err == nil {
		t.Fatal("full access bypassed local desktop consent")
	}
}

func TestPermissionModeUnsupportedNeverFallsThroughToLegacySend(t *testing.T) {
	c, a, _ := aggregationFixture(t, "claude")
	_, err := c.Dispatch(context.Background(), map[string]any{"type": "session.send", "sessionKey": "claude:s", "text": "synthetic", "permissionMode": "auto_review"})
	if err == nil || !strings.Contains(err.Error(), "permission_mode_unsupported") || len(a.sent) != 0 {
		t.Fatalf("silently dropped permission: %v", err)
	}
	_, err = c.dispatchNativeSend(context.Background(), map[string]any{"permissionMode": "full_access"})
	if err == nil || !strings.Contains(err.Error(), "permission_mode_unsupported") {
		t.Fatalf("native silently dropped permission: %v", err)
	}
}
