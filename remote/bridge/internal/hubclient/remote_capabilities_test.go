package hubclient

import (
	"context"
	"encoding/base64"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/desktopcompanion"
	"salcara/bridge/internal/launcher"
	"salcara/bridge/internal/protocol"
)

func TestPublishedRemoteCapabilitiesRequireExecutableContracts(t *testing.T) {
	c, a, _ := aggregationFixture(t, "hermes")
	a.descriptor.Capabilities.PreciseInterrupt = true
	a.descriptor.Capabilities.Continue = true
	a.descriptor.Capabilities.Steer = true
	a.descriptor.Capabilities.PermissionModes = []string{"ask", "auto_review", "full_access"}
	for _, status := range c.agentStatus(context.Background())["agents"].([]agentStatus) {
		if status.ID != "hermes" {
			continue
		}
		caps := status.Capabilities
		if !caps.List || !caps.Read || !caps.Send || !caps.ImageOutput || !status.RemoteSendSupported {
			t.Fatal("mandatory implemented contracts were lost")
		}
		if caps.Interrupt || caps.PreciseInterrupt || caps.Continue || caps.Steer || caps.Approval || caps.Questions || caps.ModelOverride || caps.Attachments || caps.ImageInput || len(caps.PermissionModes) != 0 {
			t.Fatalf("descriptor invented an executable optional contract: %+v", caps)
		}
		return
	}
	t.Fatal("extension absent")
}

func TestDeclaredAttachmentCapabilityDoesNotGrantImageInput(t *testing.T) {
	_, base, _ := aggregationFixture(t, "claude")
	a := &permissionOptionAgent{aggregationFixtureAgent: base}
	if err := requireAgentCapability(a, "attachments"); err != nil {
		t.Fatal("declared attachment operation was omitted from capability switch", err)
	}
	a.descriptor.Capabilities.ImageInput = false
	if requireAgentCapability(a, "imageInput") == nil {
		t.Fatal("generic attachment flag granted image input")
	}
	a.descriptor.Capabilities.Attachments = false
	if requireAgentCapability(a, "attachments") == nil {
		t.Fatal("explicit false attachment flag ignored")
	}
}

type turnContractFixture struct{ *aggregationFixtureAgent }

func (*turnContractFixture) InterruptTurn(context.Context, string, string, string) (protocol.InterruptResult, error) {
	return protocol.InterruptResult{}, nil
}

func (*turnContractFixture) ContinueWithOptions(context.Context, string, string, string, string, agents.TurnOptions) (protocol.ContinueResult, error) {
	return protocol.ContinueResult{}, nil
}

func TestMissingTurnContractRefusedBeforeModelOrWorkerSideEffects(t *testing.T) {
	for _, typ := range []string{"session.continue", "session.interrupt", "session.steer"} {
		c, a, _ := aggregationFixture(t, "claude")
		a.descriptor.Capabilities.Continue, a.descriptor.Capabilities.PreciseInterrupt, a.descriptor.Capabilities.Steer = true, true, true
		cmd := map[string]any{"type": typ, "sessionKey": "claude:s", "text": "fixture",
			"expectedTurnId": "old-turn", "operationId": "11111111-2222-4333-8444-555555555555", "controlSurface": "cli"}
		if typ != "session.steer" {
			cmd["model"] = "choice"
		}
		assertCapabilityRefusal(t, c, cmd)
		if a.modelCalls != 0 || len(a.sent)+len(a.started)+len(a.stopped)+len(a.opened) != 0 {
			t.Fatal("missing optional contract was checked after a side effect")
		}
	}
}

func TestStartApprovalDefaultAndMalformedInputCannotBypassPhoneConsent(t *testing.T) {
	for _, tool := range []string{"codex", "claude"} {
		for _, variant := range []string{"omitted-denied", "explicit-denied", "empty", "wrong-type", "unknown", "omitted-allowed", "explicit-ask"} {
			t.Run(tool+"/"+variant, func(t *testing.T) {
				c, base, cwd := aggregationFixture(t, tool)
				if err := c.o.Store.Update(func(cfg *config.Config) error {
					cfg.Approval = config.ApprovalAutoAll
					cfg.AllowPhoneAutoAll = variant == "omitted-allowed"
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				cmd := map[string]any{"type": "session.start", "tool": tool, "cwd": cwd, "prompt": "synthetic"}
				switch variant {
				case "explicit-denied":
					cmd["approval"] = config.ApprovalAutoAll
				case "empty":
					cmd["approval"] = ""
				case "wrong-type":
					cmd["approval"] = true
				case "unknown":
					cmd["approval"] = "unknown"
				case "explicit-ask":
					cmd["approval"] = config.ApprovalAsk
				}
				_, err := c.Dispatch(context.Background(), cmd)
				allowed := variant == "omitted-allowed" || variant == "explicit-ask" || tool == "claude" && variant == "omitted-denied"
				if allowed != (err == nil) || allowed != (len(base.started) == 1) {
					t.Fatalf("approval selection: allowed=%v error=%v starts=%d", allowed, err, len(base.started))
				}
				if allowed {
					want := config.ApprovalAsk
					if variant == "omitted-allowed" && tool != "claude" {
						want = config.ApprovalAutoAll
					}
					if !strings.HasSuffix(base.started[0], "|"+want) {
						t.Fatal("chosen approval was silently replaced")
					}
				}
			})
		}
	}
}

// Only constructors and descriptors are used: all state/history roots point to
// temporary fixtures, executables are nonexistent, no app or provider is called.
func TestPublishedBuiltinCapabilitiesRetainRealCodexAndClaudeContracts(t *testing.T) {
	root := t.TempDir()
	m := agents.NewManagerWithOptions(nil, func() agents.Settings { return agents.Settings{} }, agents.Options{
		StateDir: root, ClaudeHome: root, CodexPath: filepath.Join(root, "no-codex"), ClaudePath: filepath.Join(root, "no-claude"),
	})
	t.Cleanup(m.Close)
	c := statusFixture(t, config.Config{})
	c.SetManager(m)
	c.o.DiscoverTools = func(context.Context, map[string]string) []launcher.Tool {
		return []launcher.Tool{{ID: "codex", Available: true}, {ID: "claude", Available: true}, {ID: "claude-desktop", Available: true}}
	}
	for _, status := range c.agentStatus(context.Background())["agents"].([]agentStatus) {
		if status.ID == "codex-desktop" {
			continue
		}
		caps := status.Capabilities
		if !caps.Send || !caps.Interrupt || !caps.PreciseInterrupt || !caps.Continue || !caps.Approval || !caps.Questions || !caps.ModelOverride || !caps.ImageInput || !caps.Attachments || caps.Steer != (status.ID == "codex") {
			t.Fatalf("real %s adapter contract changed: %+v", status.ID, caps)
		}
		if status.ID == "codex" {
			if !reflect.DeepEqual(caps.PermissionModes, []string{"ask", "auto_review", "full_access"}) {
				t.Fatal("Codex permission contract was lost")
			}
		} else if len(caps.PermissionModes) != 0 {
			t.Fatal("Codex permission modes leaked into Claude")
		}
		// Implementing an interface must not re-enable an explicit false flag.
		if zero := publishedRemoteCapabilities(m.Get(status.Tool), protocol.AgentCapabilities{}); !reflect.DeepEqual(zero, protocol.AgentCapabilities{}) {
			t.Fatalf("method presence granted undeclared capabilities: %+v", zero)
		}
		withoutApproval := caps
		withoutApproval.Approval = false
		if publishedRemoteCapabilities(m.Get(status.Tool), withoutApproval).Questions {
			t.Fatal("question control depended on an unavailable approval action")
		}
		if status.ID != "codex" {
			withoutApproval.PermissionModes = []string{"ask", "auto_review", "full_access"}
			if len(publishedRemoteCapabilities(m.Get(status.Tool), withoutApproval).PermissionModes) != 0 {
				t.Fatal("Codex-only permission selector escaped to another executor")
			}
		}
	}
}

type consentRevokingCatalogAgent struct {
	*effortMetadataAgent
	settings *config.Store
}

func (a *consentRevokingCatalogAgent) ModelCatalog(ctx context.Context) (protocol.ModelCatalog, error) {
	if err := a.settings.Update(func(cfg *config.Config) error { cfg.AllowPhoneAutoAll = false; return nil }); err != nil {
		return protocol.ModelCatalog{}, err
	}
	return a.effortMetadataAgent.ModelCatalog(ctx)
}

func TestStartInheritedAutoAllRechecksConsentAfterModelMetadataWait(t *testing.T) {
	c, base, cwd := effortClient(t)
	if err := c.o.Store.Update(func(cfg *config.Config) error {
		cfg.Approval, cfg.AllowPhoneAutoAll = config.ApprovalAutoAll, true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a := &consentRevokingCatalogAgent{effortMetadataAgent: base, settings: c.o.Store}
	c.SetManager(&fakeManager{list: []agents.Agent{a}})
	_, err := c.Dispatch(context.Background(), map[string]any{"type": "session.start", "tool": "codex", "cwd": cwd,
		"prompt": "synthetic", "model": "effort-fixture", "effort": "high"})
	if err == nil || !strings.Contains(err.Error(), "permission_mode_denied") || len(base.started)+len(base.startOptions) != 0 {
		t.Fatalf("revoked inherited auto-all started: %v", err)
	}
}

func TestStartLegacyAutoAllPassesLiveGuardToBothAdapters(t *testing.T) {
	for _, tool := range []string{"codex", "claude"} {
		c, base, cwd := aggregationFixture(t, tool)
		a := &modelOptionsAgent{fakeAgent: base.fakeAgent}
		c.SetManager(&fakeManager{list: []agents.Agent{a}})
		if err := c.o.Store.Update(func(cfg *config.Config) error {
			cfg.Approval, cfg.AllowPhoneAutoAll = config.ApprovalAutoAll, true
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		_, err := c.Dispatch(context.Background(), map[string]any{"type": "session.start", "tool": tool, "cwd": cwd, "prompt": "synthetic", "approval": "auto_all"})
		if err != nil || len(a.startOptions) != 1 || a.startOptions[0].ValidatePermission == nil {
			t.Fatal("legacy auto-all lost the live adapter fence", err)
		}
		guard := a.startOptions[0].ValidatePermission
		if guard() != nil {
			t.Fatal("active desktop consent was ignored")
		}
		if err = c.o.Store.Update(func(cfg *config.Config) error { cfg.AllowPhoneAutoAll = false; return nil }); err != nil {
			t.Fatal(err)
		}
		if guard() == nil {
			t.Fatal("legacy approval grant remained usable after revocation")
		}
	}
}

func TestNativeApprovalStatusRequiresResponderAndRejectsUnsupportedStopLease(t *testing.T) {
	for _, mode := range []string{"no-responder", "responder", "unsupported-stop"} {
		t.Run(mode, func(t *testing.T) {
			c := statusFixture(t, config.Config{})
			d := &routeDesktop{st: liveDesktop("codex:01a0ae56-e9f6-4933-9ad4-5e08cd7874e5")}
			d.st.Capabilities.Approval = true
			d.st.ApprovalTransport = desktopcompanion.NativeApprovalTransport
			c.o.Desktop = d
			if mode == "responder" {
				c.o.Desktop = &approvalDesktop{routeDesktop: *d}
			}
			if mode == "unsupported-stop" {
				d.st.Capabilities.Interrupt = true
			}
			for _, status := range dispatchStatus(t, c) {
				if status.ID == "codex-desktop" {
					if mode == "unsupported-stop" {
						if status.NativeConnection != nil || status.Capabilities.Send || status.Capabilities.Interrupt {
							t.Fatal("unsupported native lease became usable")
						}
						continue
					}
					if status.NativeConnection == nil || !status.Capabilities.Send || status.Capabilities.Approval != (mode == "responder") {
						t.Fatal("native approval was not projected from actual responder")
					}
					if status.NativeConnection.Capabilities.Approval != status.Capabilities.Approval || (status.NativeConnection.ApprovalTransport != "") != status.Capabilities.Approval {
						t.Fatal("raw native metadata bypassed projected capabilities")
					}
				}
				if status.ID == "codex" && status.DesktopLive != nil && status.DesktopLive.Capabilities.Approval != (mode == "responder") {
					t.Fatal("live metadata advertised a missing approval route")
				}
			}
		})
	}
}

func TestClaudeDesktopAttachmentUploadUsesOnlyItsDeclaredCodeWorker(t *testing.T) {
	for _, variant := range []string{"claude", "claude-desktop", "wrong-tool", "native-surface", "read-only", "malformed-surface", "malformed-agent", "codex-desktop", "shared-api-family", "no-image-methods"} {
		t.Run(variant, func(t *testing.T) {
			c, base, _ := aggregationFixture(t, "claude")
			worker := &modelOptionsAgent{fakeAgent: base.fakeAgent}
			c.SetManager(&fakeManager{list: []agents.Agent{worker}})
			previous := phoneAttachments
			phoneAttachments = testAttachmentStore(t)
			t.Cleanup(func() { phoneAttachments = previous })
			id := "att_" + strings.Repeat("ab", 16)
			image := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, []byte(strings.Repeat("x", 100))...)
			cmd := map[string]any{"type": "attachment.put", "agent": "claude-desktop", "tool": "claude", "controlSurface": "cli", "id": id,
				"mime": "image/jpeg", "index": float64(0), "total": float64(1), "data": base64.StdEncoding.EncodeToString(image)}
			switch variant {
			case "claude":
				cmd["agent"] = "claude"
			case "wrong-tool":
				cmd["tool"] = "codex"
			case "native-surface":
				cmd["controlSurface"] = "desktop"
			case "read-only":
				cmd["sessionKey"] = "claude-desktop:local_11111111-2222-4333-8444-555555555555"
			case "malformed-surface":
				cmd["controlSurface"] = map[string]any{}
			case "malformed-agent":
				cmd["agent"] = []any{"claude"}
			case "codex-desktop":
				cmd["agent"] = "codex-desktop"
			case "shared-api-family":
				base.descriptor.ID, base.descriptor.Tool = "hermes", "hermes"
				base.fakeAgent.id = "hermes"
				base.descriptor.APIFamily = "claude"
				c.SetManager(&fakeManager{list: []agents.Agent{base}})
			case "no-image-methods":
				c.SetManager(&fakeManager{list: []agents.Agent{base}})
			}
			result, err := c.Dispatch(context.Background(), cmd)
			if variant == "claude" || variant == "claude-desktop" {
				if err != nil || result.(map[string]any)["complete"] != true {
					t.Fatal(result, err)
				}
				if _, err = c.Dispatch(context.Background(), map[string]any{"type": "session.send", "sessionKey": "claude:s", "agent": "claude-desktop", "text": "fixture", "attachments": []string{id}}); err != nil {
					t.Fatal(err)
				}
				if len(worker.sendOptions) != 1 || len(worker.sendOptions[0].Images) != 1 {
					t.Fatal("uploaded image did not reach the selected Code session")
				}
			} else if err == nil || result != nil {
				t.Fatal("unsupported attachment route was accepted")
			}
		})
	}
}
