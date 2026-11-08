package hubclient

import (
	"context"
	"strings"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/launcher"
	"salcara/bridge/internal/protocol"
)

type aggregationFixtureAgent struct {
	*fakeAgent
	descriptor                           protocol.AgentDescriptor
	listCalls, modelCalls, responseCalls int
	startModels                          []string
}

func aggregationDescriptor(id string) protocol.AgentDescriptor {
	return protocol.AgentDescriptor{ID: id, Tool: id, Name: "Fixture Agent", APIFamily: id, ControlSurface: "cli",
		Capabilities: protocol.AgentCapabilities{List: true, Read: true, Send: true, Interrupt: true, Approval: true,
			Questions: true, Attachments: true, ImageInput: true, ImageOutput: true, ModelOverride: true}}
}
func (a *aggregationFixtureAgent) Descriptor() protocol.AgentDescriptor { return a.descriptor }
func (a *aggregationFixtureAgent) Sessions(ctx context.Context) ([]protocol.SessionInfo, error) {
	a.listCalls++
	return a.fakeAgent.Sessions(ctx)
}
func (a *aggregationFixtureAgent) Models(ctx context.Context) []string {
	a.modelCalls++
	return a.fakeAgent.Models(ctx)
}
func (a *aggregationFixtureAgent) Start(ctx context.Context, cwd, prompt, model, approval string) (string, error) {
	a.startModels = append(a.startModels, model)
	return a.fakeAgent.Start(ctx, cwd, prompt, model, approval)
}
func (a *aggregationFixtureAgent) RespondWithAnswers(id string, _ agents.ApprovalResponse) bool {
	a.responseCalls++
	return id == "fixture"
}
func aggregationFixture(t *testing.T, id string) (*Client, *aggregationFixtureAgent, string) {
	t.Helper()
	cwd := t.TempDir()
	c := statusFixture(t, config.Config{Projects: []protocol.Project{{Path: cwd}}, ClaudeModel: "claude-only-default"})
	a := &aggregationFixtureAgent{fakeAgent: &fakeAgent{id: id}, descriptor: aggregationDescriptor(id)}
	c.SetManager(&fakeManager{list: []agents.Agent{a}})
	return c, a, cwd
}
func assertCapabilityRefusal(t *testing.T, c *Client, cmd map[string]any) {
	t.Helper()
	result, err := c.Dispatch(context.Background(), cmd)
	if err == nil || !strings.Contains(err.Error(), "agent_capability_unavailable") {
		t.Fatalf("result=%v error=%v", result, err)
	}
}

func TestAggregationExplicitCapabilityFalseIsEnforcedBeforeAdapter(t *testing.T) {
	for _, id := range []string{"hermes", "codex", "claude"} {
		for _, operation := range []string{"list", "read", "send", "start", "interrupt", "approval", "questions", "models"} {
			t.Run(id+"/"+operation, func(t *testing.T) {
				c, a, cwd := aggregationFixture(t, id)
				a.descriptor.Capabilities = protocol.AgentCapabilities{}
				cmd := map[string]any{"sessionKey": id + ":s", "tool": id, "text": "message", "prompt": "task", "cwd": cwd, "approvalId": "fixture", "decision": "allow"}
				switch operation {
				case "list":
					cmd["type"] = "sessions.list"
				case "read":
					cmd["type"] = "session.open"
				case "send":
					cmd["type"] = "session.send"
				case "start":
					cmd["type"] = "session.start"
				case "interrupt":
					cmd["type"] = "session.interrupt"
				case "approval":
					cmd["type"] = "approval.respond"
				case "questions":
					cmd["type"], cmd["answers"] = "approval.respond", map[string][]string{"answer": {"yes"}}
				case "models":
					cmd["type"] = "models.list"
				}
				assertCapabilityRefusal(t, c, cmd)
				if a.listCalls+a.modelCalls+a.responseCalls+len(a.started)+len(a.opened)+len(a.sent)+len(a.stopped) != 0 {
					t.Fatal("unsupported operation reached adapter")
				}
			})
		}
	}
}

func TestAggregationLegacyExtensionDoesNotGainMandatoryMethodCapabilities(t *testing.T) {
	c, _, cwd := aggregationFixture(t, "hermes")
	a := &fakeAgent{id: "hermes"}
	c.SetManager(&fakeManager{list: []agents.Agent{a}})
	for _, cmd := range []map[string]any{
		{"type": "sessions.list", "tool": "hermes"}, {"type": "session.open", "sessionKey": "hermes:s"},
		{"type": "session.start", "tool": "hermes", "cwd": cwd, "prompt": "task"},
		{"type": "session.send", "sessionKey": "hermes:s", "text": "message"}, {"type": "session.interrupt", "sessionKey": "hermes:s"},
	} {
		assertCapabilityRefusal(t, c, cmd)
	}
	if len(a.started)+len(a.opened)+len(a.sent)+len(a.stopped) != 0 {
		t.Fatal("legacy extension executed")
	}
}

func TestAggregationRejectsMisleadingIdentityAndNonCLISurface(t *testing.T) {
	for _, mutation := range []string{"foreign-id", "foreign-tool", "desktop", "read-only"} {
		t.Run(mutation, func(t *testing.T) {
			c, a, _ := aggregationFixture(t, "hermes")
			switch mutation {
			case "foreign-id":
				a.descriptor.ID = "codex"
			case "foreign-tool":
				a.descriptor.Tool = "codex"
			default:
				a.descriptor.ControlSurface = mutation
			}
			assertCapabilityRefusal(t, c, map[string]any{"type": "session.send", "sessionKey": "hermes:s", "text": "message"})
			if _, err := c.routeForAgent("hermes"); err == nil {
				t.Fatal("invalid adapter obtained API route")
			}
			result := c.agentStatus(context.Background())
			for _, status := range result["agents"].([]agentStatus) {
				if status.ID == "hermes" {
					t.Fatal("invalid adapter advertised")
				}
			}
		})
	}
}

func TestAggregationOptionsNeverSilentlyDiscardedAndDefaultsStayIsolated(t *testing.T) {
	c, a, cwd := aggregationFixture(t, "hermes")
	if _, err := c.Dispatch(context.Background(), map[string]any{"type": "session.start", "tool": "hermes", "cwd": cwd, "prompt": "task"}); err != nil {
		t.Fatal(err)
	}
	if len(a.startModels) != 1 || a.startModels[0] != "" {
		t.Fatalf("extension inherited model: %v", a.startModels)
	}
	if _, err := c.Dispatch(context.Background(), map[string]any{"type": "session.start", "tool": "hermes", "cwd": cwd, "prompt": "task", "model": "native-choice"}); err != nil {
		t.Fatal(err)
	}
	if a.startModels[1] != "native-choice" {
		t.Fatal("mandatory Start model was lost")
	}
	for _, cmd := range []map[string]any{
		{"type": "session.send", "sessionKey": "hermes:s", "text": "message", "model": "native-choice"},
		{"type": "session.start", "tool": "hermes", "cwd": cwd, "prompt": "task", "effort": "high"},
		{"type": "session.start", "tool": "hermes", "cwd": cwd, "prompt": "task", "attachments": []string{"att_" + strings.Repeat("a", 32)}},
	} {
		assertCapabilityRefusal(t, c, cmd)
	}
	if len(a.started) != 2 || len(a.sent) != 0 {
		t.Fatal("unsupported options reached worker")
	}
	a.descriptor.Capabilities.ModelOverride = false
	assertCapabilityRefusal(t, c, map[string]any{"type": "session.start", "tool": "hermes", "cwd": cwd, "prompt": "task", "model": "native-choice"})
}

func TestAggregationMalformedTurnFieldsDoNotBecomeEmptyOptions(t *testing.T) {
	for _, typ := range []string{"session.start", "session.send"} {
		for _, field := range []string{"model", "effort", "attachments"} {
			t.Run(typ+"/"+field, func(t *testing.T) {
				c, a, cwd := aggregationFixture(t, "hermes")
				cmd := map[string]any{"type": typ, "tool": "hermes", "sessionKey": "hermes:s", "cwd": cwd, "prompt": "task", "text": "message", field: 123}
				if _, err := c.Dispatch(context.Background(), cmd); err == nil {
					t.Fatal("malformed field accepted")
				}
				if len(a.started)+len(a.sent) != 0 {
					t.Fatal("malformed field ran")
				}
			})
		}
	}
}

type aggregationPagedAgent struct{ *aggregationFixtureAgent }

func (a *aggregationPagedAgent) SessionsPage(context.Context, string, int, string) ([]protocol.SessionInfo, string, error) {
	a.listCalls++
	return a.sessions, "", nil
}
func TestAggregationDirectoryAndHistoryBindKeyAndTool(t *testing.T) {
	for _, paged := range []bool{false, true} {
		for _, foreign := range []protocol.SessionInfo{{SessionKey: "codex:s"}, {SessionKey: "hermes:s", Tool: "codex"}, {SessionKey: "hermes:\x00"}} {
			c, a, _ := aggregationFixture(t, "hermes")
			a.sessions = []protocol.SessionInfo{foreign}
			if paged {
				c.SetManager(&fakeManager{list: []agents.Agent{&aggregationPagedAgent{a}}})
			}
			if _, err := c.Dispatch(context.Background(), map[string]any{"type": "sessions.list", "tool": "hermes"}); err == nil {
				t.Fatalf("foreign directory accepted: %+v paged=%v", foreign, paged)
			}
		}
	}
	c, a, _ := aggregationFixture(t, "hermes")
	a.openEvents = []protocol.Event{{Type: "message", Tool: "codex", SessionKey: "hermes:s", Text: "wrong tool"}, {Type: "message", SessionKey: "hermes:s", Text: "own"}}
	result, err := c.Dispatch(context.Background(), map[string]any{"type": "session.open", "sessionKey": "hermes:s"})
	if err != nil {
		t.Fatal(err)
	}
	events := result.(map[string]any)["events"].([]protocol.Event)
	if len(events) != 1 || events[0].Text != "own" || events[0].Tool != "hermes" {
		t.Fatalf("wrong ownership: %+v", events)
	}
	if result.(map[string]any)["session"].(protocol.SessionInfo).Tool != "hermes" {
		t.Fatal("history Tool not bound")
	}
}

func TestAggregationSharedAPIFamilyDoesNotChangeSessionNamespace(t *testing.T) {
	c, a, _ := aggregationFixture(t, "hermes")
	a.descriptor.APIFamily = "codex"
	route, err := c.routeForAgent("hermes")
	if err != nil || route.Family != "codex" || route.ID != "hermes" {
		t.Fatalf("route=%+v err=%v", route, err)
	}
	if _, err = c.Dispatch(context.Background(), map[string]any{"type": "session.send", "sessionKey": "hermes:s", "text": "message"}); err != nil {
		t.Fatal(err)
	}
	if len(a.sent) != 1 || a.sent[0] != "s|message" {
		t.Fatal("session was cross routed")
	}
	if _, err = c.Dispatch(context.Background(), map[string]any{"type": "session.send", "sessionKey": "hermes:s", "tool": "codex", "text": "wrong executor"}); err == nil {
		t.Fatal("shared credential family became an executor alias")
	}
	if len(a.sent) != 1 {
		t.Fatal("mismatched executor reached adapter")
	}
}

type aggregationClaimsAgent struct{ *aggregationFixtureAgent }

func (a *aggregationClaimsAgent) Open(_ context.Context, id string) (protocol.SessionInfo, []protocol.Event, error) {
	a.opened = append(a.opened, id)
	info := protocol.SessionInfo{SessionKey: a.id + ":" + id, Tool: a.id, Controllable: true, ControlSurface: "desktop", ControlExpiresAt: 9999999999999}
	return info, []protocol.Event{
		{Type: "session.updated", SessionKey: info.SessionKey, Session: &info},
		{Type: "message", SessionKey: info.SessionKey, Attachments: []protocol.AttachmentRef{{ID: "synthetic"}}, ImageData: []string{"synthetic-image"}},
		{Type: "tool", SessionKey: info.SessionKey, Kind: "image_generation", Detail: "image-base64:synthetic"},
		{Type: "approval.request", SessionKey: info.SessionKey, Kind: "question", ApprovalID: "unsupported-question"},
	}, nil
}
func TestAggregationReadOnlyCapabilitiesProjectSnapshotsAndLegacyDirectory(t *testing.T) {
	c, a, _ := aggregationFixture(t, "hermes")
	a.descriptor.Capabilities.Send, a.descriptor.Capabilities.ImageOutput, a.descriptor.Capabilities.Questions = false, false, false
	a.sessions = []protocol.SessionInfo{{SessionKey: "hermes:s", Controllable: true, ControlSurface: "desktop", ControlExpiresAt: 9999999999999}}
	c.SetManager(&fakeManager{list: []agents.Agent{&aggregationClaimsAgent{a}}})
	result, err := c.Dispatch(context.Background(), map[string]any{"type": "sessions.list", "tool": "hermes"})
	if err != nil {
		t.Fatal(err)
	}
	listed := result.(map[string]any)["sessions"].([]protocol.SessionInfo)[0]
	if listed.Controllable || listed.ControlSurface != "cli" || listed.ControlExpiresAt != 0 {
		t.Fatal("directory projected false executor permissions")
	}
	if a.sessions[0].ControlSurface != "desktop" || !a.sessions[0].Controllable {
		t.Fatal("adapter-owned directory was mutated")
	}
	result, err = c.Dispatch(context.Background(), map[string]any{"type": "session.open", "sessionKey": "hermes:s"})
	if err != nil {
		t.Fatal(err)
	}
	info := result.(map[string]any)["session"].(protocol.SessionInfo)
	if info.Controllable || info.ControlSurface != "cli" || info.ControlExpiresAt != 0 {
		t.Fatal("history retained native/controllable claim")
	}
	events := result.(map[string]any)["events"].([]protocol.Event)
	if len(events) != 3 || events[0].Session.Controllable || events[0].Session.ControlExpiresAt != 0 || len(events[1].Attachments) != 0 || len(events[1].ImageData) != 0 || events[2].Detail != "" {
		t.Fatalf("snapshot capabilities not projected: %+v", events)
	}
}

func TestAggregationClaudeDesktopDistinguishesCodeWorkerAndReadOnlyHistory(t *testing.T) {
	for _, mode := range []string{"code-only", "read-only-only", "explicit-no-read"} {
		t.Run(mode, func(t *testing.T) {
			c, a, _ := aggregationFixture(t, "claude")
			workerAvailable := mode != "read-only-only"
			c.o.DiscoverTools = func(context.Context, map[string]string) []launcher.Tool {
				return []launcher.Tool{{ID: "claude-desktop", Available: true}, {ID: "claude", Available: workerAvailable}}
			}
			if mode == "explicit-no-read" {
				a.descriptor.Capabilities.List, a.descriptor.Capabilities.Read = false, false
			}
			if mode == "read-only-only" {
				f := &readonlyHistoryFixture{identity: strings.Repeat("a", 64)}
				c.o.ClaudeDesktopHistory = func(context.Context) (ReadOnlyDesktopHistory, error) { return f, nil }
			}
			var desktop agentStatus
			for _, s := range c.agentStatus(context.Background())["agents"].([]agentStatus) {
				if s.ID == "claude-desktop" {
					desktop = s
				}
			}
			if mode == "explicit-no-read" {
				if desktop.Capabilities.List || desktop.Capabilities.Read {
					t.Fatal("explicit false worker read ignored")
				}
			} else if !desktop.Capabilities.List || !desktop.Capabilities.Read {
				t.Fatal("Code/native readonly history capabilities conflated")
			}
			if desktop.RemoteSendSupported != workerAvailable || desktop.Capabilities.Send != workerAvailable {
				t.Fatal("readonly history falsely authorized execution")
			}
		})
	}
}
