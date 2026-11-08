package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

type contextDispatchAgent struct {
	*aggregationFixtureAgent
	reads, compacts      int
	before, after        func()
	wrongSession         bool
	lifecycle, admission func() error
	fullAccess           bool
}

func (a *contextDispatchAgent) contextResult(id string, validate func() error) (protocol.SessionContext, error) {
	if a.before != nil {
		a.before()
	}
	if err := validate(); err != nil {
		return protocol.SessionContext{}, err
	}
	if a.after != nil {
		a.after()
	}
	key := a.ID() + ":" + id
	if a.wrongSession {
		key += "-other"
	}
	return protocol.SessionContext{SessionKey: key, Revision: "runtime-1", CanCompact: true}, nil
}
func (a *contextDispatchAgent) SessionContext(_ context.Context, id string, validate func() error) (protocol.SessionContext, error) {
	a.reads++
	return a.contextResult(id, validate)
}
func (a *contextDispatchAgent) CompactContext(ctx context.Context, id, revision, operation string, validate func() error) (protocol.SessionContext, error) {
	a.compacts++
	a.lifecycle, a.admission = agents.ContextLifecycleGuard(ctx), validate
	return a.contextResult(id, validate)
}

func (a *contextDispatchAgent) SessionRequiresFullAccessConsent(string) bool { return a.fullAccess }

func TestContextLifecycleGuardSurvivesCommandCancelButKeepsLiveSourceAndPermissions(t *testing.T) {
	for _, change := range []string{"cancel-only", "source-revoked", "source-aba", "permission-revoked", "capability-revoked", "manager-replaced", "manager-cleared"} {
		t.Run(change, func(t *testing.T) {
			c, store, _, env := sourceGuardFixture(t, "api_key")
			_, base, _ := aggregationFixture(t, "codex")
			a := &contextDispatchAgent{aggregationFixtureAgent: base}
			a.descriptor.Capabilities.Context, a.descriptor.Capabilities.Compact = true, true
			if change == "permission-revoked" {
				a.fullAccess = true
				if err := store.Update(func(cfg *config.Config) error { cfg.AllowPhoneAutoAll = true; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			c.SetManager(&fakeManager{list: []agents.Agent{a}})
			env.Command = contextCommand(true)
			env.Command["accountId"] = env.Source.CredentialHandle
			env.Source.Model = ""
			ctx, cancel := context.WithCancel(withExecutionSource(context.Background(), env))
			if _, err := c.Dispatch(ctx, env.Command); err != nil {
				cancel()
				t.Fatal(err)
			}
			cancel()
			if a.lifecycle == nil || a.admission == nil || !errors.Is(a.admission(), context.Canceled) {
				t.Fatal("admission cancellation lost or lifecycle guard missing")
			}
			if err := a.lifecycle(); err != nil {
				t.Fatalf("command cancellation canceled admitted native work: %v", err)
			}
			switch change {
			case "source-revoked":
				if err := store.Update(func(cfg *config.Config) error { cfg.LocalAccounts[0].Key = "revoked-key"; return nil }); err != nil {
					t.Fatal(err)
				}
			case "source-aba":
				for _, key := range []string{"key-b", "key-a"} {
					if err := store.Update(func(cfg *config.Config) error { cfg.LocalAccounts[0].Key = key; return nil }); err != nil {
						t.Fatal(err)
					}
				}
			case "permission-revoked":
				if err := store.Update(func(cfg *config.Config) error { cfg.AllowPhoneAutoAll = false; return nil }); err != nil {
					t.Fatal(err)
				}
			case "capability-revoked":
				a.descriptor.Capabilities.Compact = false
			case "manager-replaced":
				c.SetManager(&fakeManager{list: []agents.Agent{&contextDispatchAgent{aggregationFixtureAgent: base}}})
			case "manager-cleared":
				c.SetManager(nil)
			}
			if err := a.lifecycle(); (err == nil) != (change == "cancel-only") {
				t.Fatalf("lifecycle %s: %v", change, err)
			}
		})
	}
}

func TestContextLifecycleGuardRetainsPhoneBindingAndUploadAuthorizationAfterCancellation(t *testing.T) {
	for _, change := range []string{"binding", "phone-hash", "unpaired", "upload-authorization", "device-identity"} {
		t.Run(change, func(t *testing.T) {
			c, store, account, env := sourceGuardFixture(t, "api_key")
			if err := store.Update(func(cfg *config.Config) error {
				cfg.RemoteDeviceOnly = true
				cfg.PhoneBindingID, cfg.PhoneHash = strings.Repeat("a", 64), strings.Repeat("b", 64)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			cfg := store.Get()
			c.publishPair(PairStatus{DeviceID: cfg.DeviceID, Paired: true}, eventIdentity(cfg))
			_, base, _ := aggregationFixture(t, "codex")
			a := &contextDispatchAgent{aggregationFixtureAgent: base}
			a.descriptor.Capabilities.Context, a.descriptor.Capabilities.Compact = true, true
			c.SetManager(&fakeManager{list: []agents.Agent{a}})
			env.Command = contextCommand(true)
			env.Command["accountId"] = APIHandle(cfg, account.ID)
			env.DeviceID, env.BindingID, env.PhoneHash = cfg.DeviceID, cfg.PhoneBindingID, cfg.PhoneHash
			env.Source.CredentialHandle, env.Source.ConfigRevision, env.Source.Model = APIHandle(cfg, account.ID), cfg.AccountSourceRevision(account), ""
			guarded := withCommandGuard(context.Background(), commandGuard{identity: config.RemoteIdentity(cfg), deviceID: cfg.DeviceID, bindingID: cfg.PhoneBindingID, phoneHash: cfg.PhoneHash})
			ctx, cancel := context.WithCancel(withExecutionSource(guarded, env))
			defer cancel()
			if _, err := c.Dispatch(ctx, env.Command); err != nil {
				t.Fatal(err)
			}
			cancel()
			if a.lifecycle == nil || a.admission == nil || !errors.Is(a.admission(), context.Canceled) {
				t.Fatal("admission cancellation lost or lifecycle guard missing")
			}
			if err := a.lifecycle(); err != nil {
				t.Fatalf("valid paired lifecycle lost to caller cancellation: %v", err)
			}
			if change == "upload-authorization" {
				c.publishPairState(PairStatus{DeviceID: cfg.DeviceID}, "", true)
			} else if err := store.Update(func(current *config.Config) error {
				switch change {
				case "binding":
					current.PhoneBindingID = strings.Repeat("c", 64)
				case "phone-hash":
					current.PhoneHash = strings.Repeat("c", 64)
				case "unpaired":
					current.PhoneBindingID, current.PhoneHash = "", ""
				case "device-identity":
					current.DeviceID += "-replacement"
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := a.lifecycle(); err == nil {
				t.Fatalf("canceled command's native lifecycle accepted revoked %s", change)
			}
		})
	}
}

func TestContextDispatchRejectsReplacedManagerAtBothBoundaries(t *testing.T) {
	for _, compact := range []bool{false, true} {
		for _, phase := range []string{"before-write", "after-response"} {
			t.Run(map[bool]string{false: "read", true: "compact"}[compact]+"/"+phase, func(t *testing.T) {
				c, base, _ := aggregationFixture(t, "codex")
				a := &contextDispatchAgent{aggregationFixtureAgent: base}
				a.descriptor.Capabilities.Context, a.descriptor.Capabilities.Compact = true, true
				c.SetManager(&fakeManager{list: []agents.Agent{a}})
				replace := func() {
					c.SetManager(&fakeManager{list: []agents.Agent{&contextDispatchAgent{aggregationFixtureAgent: base}}})
				}
				if phase == "before-write" {
					a.before = replace
				} else {
					a.after = replace
				}
				if out, err := c.Dispatch(context.Background(), contextCommand(compact)); err == nil || !strings.Contains(err.Error(), "context_source_changed") {
					t.Fatalf("replaced adapter returned context: %+v %v", out, err)
				}
			})
		}
	}
}

func contextCommand(compact bool) map[string]any {
	cmd := map[string]any{"type": "session.context", "sessionKey": "codex:s", "tool": "codex", "controlSurface": "cli"}
	if compact {
		cmd["type"], cmd["expectedRevision"], cmd["operationId"] = "session.compact", "runtime-1", "11111111-2222-4333-8444-555555555555"
	}
	return cmd
}

func TestContextDispatchRequiresExplicitCapabilityAndExactControl(t *testing.T) {
	for _, compact := range []bool{false, true} {
		for _, variant := range []string{"valid", "disabled", "missing-interface", "desktop", "missing-surface", "foreign-agent", "bad-operation", "bad-revision", "model", "approval", "attachments", "unknown", "bad-account", "wrong-session"} {
			t.Run(strings.Join([]string{map[bool]string{false: "read", true: "compact"}[compact], variant}, "/"), func(t *testing.T) {
				c, base, _ := aggregationFixture(t, "codex")
				a := &contextDispatchAgent{aggregationFixtureAgent: base}
				a.descriptor.Capabilities.Context, a.descriptor.Capabilities.Compact = true, true
				c.SetManager(&fakeManager{list: []agents.Agent{a}})
				cmd := contextCommand(compact)
				switch variant {
				case "valid":
				case "disabled":
					a.descriptor.Capabilities.Context = false
				case "missing-interface":
					c.SetManager(&fakeManager{list: []agents.Agent{base}})
				case "desktop":
					cmd["controlSurface"] = "desktop"
				case "missing-surface":
					delete(cmd, "controlSurface")
				case "foreign-agent":
					cmd["agent"] = "claude"
				case "bad-operation":
					cmd["operationId"] = "bad"
				case "bad-revision":
					cmd["expectedRevision"] = ""
				case "bad-account":
					cmd["accountId"] = 7
				case "wrong-session":
					a.wrongSession = true
				default:
					cmd[variant] = ""
				}
				result, err := c.Dispatch(context.Background(), cmd)
				if (err == nil) != (variant == "valid") {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				if variant != "valid" && variant != "wrong-session" && a.reads+a.compacts != 0 {
					t.Fatal("invalid command reached adapter")
				}
				if len(a.started)+len(a.sent)+len(a.opened)+a.modelCalls != 0 {
					t.Fatal("context operation started/resumed a conversation or called models")
				}
				if variant == "valid" {
					encoded, _ := json.Marshal(result)
					if strings.Contains(string(encoded), "snapshot") {
						t.Fatal("unknown usage became a zero reading")
					}
				}
			})
		}
	}
}

func TestContextDispatchFencesCancellationAndSourceChangesAtBothBoundaries(t *testing.T) {
	for _, phase := range []string{"before-write", "after-response", "cancelled"} {
		for _, compact := range []bool{false, true} {
			t.Run(phase+map[bool]string{true: "-compact", false: "-read"}[compact], func(t *testing.T) {
				c, base, _ := aggregationFixture(t, "codex")
				a := &contextDispatchAgent{aggregationFixtureAgent: base}
				a.descriptor.Capabilities.Context, a.descriptor.Capabilities.Compact = true, true
				c.SetManager(&fakeManager{list: []agents.Agent{a}})
				change := func() {
					if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.CodexModel = "changed"; return nil }); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if phase == "before-write" {
					a.before = change
				} else if phase == "after-response" {
					a.after = change
				} else {
					a.before = cancel
				}
				if out, err := c.Dispatch(ctx, contextCommand(compact)); err == nil {
					t.Fatalf("stale operation returned %+v", out)
				}
			})
		}
	}
}

func TestContextExecutionSourceCannotBeOmittedOrTampered(t *testing.T) {
	for _, compact := range []bool{false, true} {
		c, _, _, env := sourceGuardFixture(t, "api_key")
		env.Command = contextCommand(compact)
		env.Command["accountId"] = env.Source.CredentialHandle
		env.Source.Model = ""
		if !sourceGuardExecutionType(str(env.Command, "type")) {
			t.Fatal("context bypassed source classification")
		}
		if err := c.validateExecutionSource(env); err != nil {
			t.Fatal(err)
		}
		env.Source = nil
		if err := c.validateExecutionSource(env); err == nil {
			t.Fatal("context accepted missing execution source")
		}
	}
}

func TestContextSourceGuardRejectsABAWhileNativePreflightWaits(t *testing.T) {
	for _, compact := range []bool{false, true} {
		c, store, _, env := sourceGuardFixture(t, "api_key")
		_, base, _ := aggregationFixture(t, "codex")
		a := &contextDispatchAgent{aggregationFixtureAgent: base}
		a.descriptor.Capabilities.Context, a.descriptor.Capabilities.Compact = true, true
		c.SetManager(&fakeManager{list: []agents.Agent{a}})
		env.Command = contextCommand(compact)
		env.Command["accountId"] = env.Source.CredentialHandle
		env.Source.Model = ""
		a.before = func() {
			for _, key := range []string{"key-b", "key-a"} {
				if err := store.Update(func(cfg *config.Config) error { cfg.LocalAccounts[0].Key = key; return nil }); err != nil {
					t.Fatal(err)
				}
			}
		}
		if value, err := c.Dispatch(withExecutionSource(context.Background(), env), env.Command); err == nil || !strings.Contains(err.Error(), "context_source_changed") {
			t.Fatalf("ABA guard result=%+v err=%v", value, err)
		}
	}
}
