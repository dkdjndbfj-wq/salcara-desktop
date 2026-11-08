package hubclient

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

const sideToolsChain = "11111111-2222-4333-8444-555555555551"
const sideToolsOperation = "11111111-2222-4333-8444-555555555552"

type sideToolsFixture struct {
	*sideQuestionFixture
	metadata                protocol.SessionInfo
	toolsCalls              int
	owner                   string
	snapshot                agents.SideQuestionSnapshot
	request                 protocol.SideQuestionToolRequest
	liveValidate            func() error
	beforeTools, afterTools func()
	mutateResult            func(*protocol.SideQuestionToolState)
	reservations            []protocol.SessionInfo
	opened                  map[string]string
}

func (a *sideToolsFixture) SessionMetadata(context.Context, string) (protocol.SessionInfo, error) {
	return a.metadata, nil
}
func (a *sideToolsFixture) SideQuestionToolWorkspaces() []protocol.SessionInfo { return a.reservations }
func (a *sideToolsFixture) SideQuestionTools(_ context.Context, id, owner string, snapshot agents.SideQuestionSnapshot, request protocol.SideQuestionToolRequest, validate func() error) (protocol.SideQuestionToolState, error) {
	a.toolsCalls++
	a.owner, a.snapshot, a.liveValidate = owner, snapshot, validate
	a.request = request
	if a.beforeTools != nil {
		a.beforeTools()
	}
	if err := validate(); err != nil {
		return protocol.SideQuestionToolState{}, err
	}
	key := owner + ":" + request.ChainID
	if request.Action == "open" {
		if a.opened == nil {
			a.opened = map[string]string{}
		}
		a.opened[key] = snapshot.Revision
	} else if a.opened[key] != request.ExpectedRevision {
		return protocol.SideQuestionToolState{}, errors.New("unknown private owner or revision")
	}
	result := protocol.SideQuestionToolState{SessionKey: "codex:" + id, ChainID: request.ChainID, Revision: request.ExpectedRevision, Permission: "workspace-write", State: "idle", CanSend: true, Sequence: 1, ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}
	if request.Action == "send" {
		result.State, result.CanSend, result.TurnID, result.LastOperationID = "running", false, "private-turn", request.OperationID
	}
	if a.mutateResult != nil {
		a.mutateResult(&result)
	}
	if a.afterTools != nil {
		a.afterTools()
	}
	return result, nil
}

func newSideToolsFixture(t *testing.T) (*Client, *sideToolsFixture) {
	t.Helper()
	c, base := sideFixture(t)
	a := &sideToolsFixture{sideQuestionFixture: base, metadata: protocol.SessionInfo{SessionKey: "codex:s", Tool: "codex", ControlSurface: "cli", Cwd: c.o.Store.Get().Projects[0].Path}}
	a.options = &protocol.SideQuestionInputOptions{Version: 1, ReadOnly: true, DefaultModel: "m1", Tools: &protocol.SideQuestionToolOptions{Version: 1, Permissions: []string{"workspace-write"}}}
	c.SetManager(&fakeManager{list: []agents.Agent{a}})
	return c, a
}
func sideToolsCatalog(t *testing.T, c *Client, ctx context.Context) protocol.SideQuestionCatalog {
	t.Helper()
	result := map[string]any{"session": protocol.SessionInfo{SessionKey: "codex:s", Tool: "codex", ControlSurface: "cli"}, "events": []protocol.Event{{SessionKey: "codex:s", ID: "context-message", Type: "message", Role: "user", Text: "server snapshot"}}}
	c.rememberSideQuestionHistory(ctx, executionConfig(ctx, c.o.Store.Get()), map[string]any{"sessionKey": "codex:s"}, result, c.beginSideQuestionHistory("codex:s"))
	value, err := c.Dispatch(ctx, sideCommand(""))
	if err != nil {
		t.Fatal(err)
	}
	return value.(protocol.SideQuestionCatalog)
}
func sideToolsCommand(action, revision string) map[string]any {
	cmd := map[string]any{"type": "session.side-question.tools", "tool": "codex", "sessionKey": "codex:s", "controlSurface": "cli", "action": action, "chainId": sideToolsChain, "expectedRevision": revision}
	if action != "read" {
		cmd["operationId"] = sideToolsOperation
	}
	if action == "open" {
		cmd["permission"] = "workspace-write"
	}
	if action == "send" {
		cmd["question"] = "inspect the project"
	}
	if action == "stop" || action == "approve" {
		cmd["expectedTurnId"] = "private-turn"
	}
	if action == "approve" {
		cmd["approvalId"], cmd["decision"] = "private-approval", "allow"
	}
	return cmd
}
func openSideTools(t *testing.T, c *Client, ctx context.Context, revision string) {
	t.Helper()
	if _, err := c.Dispatch(ctx, sideToolsCommand("open", revision)); err != nil {
		t.Fatal(err)
	}
}

func TestSideQuestionToolsFeaturesAndNativeTurnOptions(t *testing.T) {
	c, a := newSideToolsFixture(t)
	a.options.Tools.Features = true
	catalog := sideToolsCatalog(t, c, context.Background())
	if catalog.InputOptions == nil || catalog.InputOptions.Tools == nil || !catalog.InputOptions.Tools.Features {
		t.Fatal("private feature capability was lost")
	}
	openSideTools(t, c, context.Background(), catalog.Revision)
	a.mutateResult = func(state *protocol.SideQuestionToolState) {
		state.Features = &protocol.AgentFeatures{CollaborationModes: []string{"default", "plan"}, SkillsSupported: true, Skills: []protocol.Skill{{Name: "inspect", Path: "C:/project/.agents/skills/inspect/SKILL.md", Description: "Inspect the project"}}}
	}
	result, err := c.Dispatch(context.Background(), sideToolsCommand("features", catalog.Revision))
	if err != nil || result.(protocol.SideQuestionToolState).Features == nil || a.request.Action != "features" {
		t.Fatalf("private features lookup failed: %+v %v", result, err)
	}
	cmd := sideToolsCommand("send", catalog.Revision)
	cmd["collaborationMode"] = "plan"
	cmd["skills"] = []any{map[string]any{"name": "inspect", "path": "C:/project/.agents/skills/inspect/SKILL.md"}}
	if _, err := c.Dispatch(context.Background(), cmd); err != nil || a.request.CollaborationMode != "plan" || len(a.request.Skills) != 1 || a.request.Skills[0].Name != "inspect" {
		t.Fatalf("native options lost before private worker: %+v %v", a.request, err)
	}
	calls := a.toolsCalls
	for _, invalid := range []any{[]any{map[string]any{"name": "inspect", "extra": true}}, "inspect", nil} {
		cmd["skills"] = invalid
		if _, err := c.Dispatch(context.Background(), cmd); err == nil || a.toolsCalls != calls {
			t.Fatal("malformed skill input reached private worker")
		}
	}
}

func TestSideQuestionToolsStrictAdmissionAndServerSnapshot(t *testing.T) {
	for _, variant := range []string{"valid", "no-capability", "no-tools-option", "owner", "snapshot", "desktop", "archived", "bad-chain", "unadvertised-revision"} {
		t.Run(variant, func(t *testing.T) {
			c, a := newSideToolsFixture(t)
			if variant == "no-tools-option" {
				a.options.Tools = nil
			}
			catalog := sideToolsCatalog(t, c, context.Background())
			cmd := sideToolsCommand("open", catalog.Revision)
			switch variant {
			case "no-capability":
				a.descriptor.Capabilities.SideQuestion = false
			case "owner":
				cmd["owner"] = "caller-owned"
			case "snapshot":
				cmd["snapshot"] = "caller-injected"
			case "desktop":
				a.metadata.ControlSurface = "desktop"
			case "archived":
				a.metadata.Archived = true
			case "bad-chain":
				cmd["chainId"] = "guessed-chain"
			case "unadvertised-revision":
				cmd["expectedRevision"] = "unadvertised"
			}
			value, err := c.Dispatch(context.Background(), cmd)
			if variant != "valid" {
				if err == nil || a.toolsCalls != 0 {
					t.Fatalf("invalid command reached provider: value=%+v err=%v calls=%d", value, err, a.toolsCalls)
				}
				return
			}
			if err != nil || a.toolsCalls != 1 || len(a.snapshot.Messages) != 1 || a.snapshot.Messages[0].Text != "server snapshot" || a.owner == "" {
				t.Fatalf("trusted snapshot missing: value=%+v err=%v", value, err)
			}
			if len(a.sent)+len(a.started)+len(a.aggregationFixtureAgent.opened) != 0 {
				t.Fatal("tools side chat touched parent worker")
			}
		})
	}
}

func TestSideQuestionToolsDirectBindingAndCancellationLifetime(t *testing.T) {
	c, a := newSideToolsFixture(t)
	paired := directFixtureContext(t, c)
	rev := sideToolsCatalog(t, c, paired).Revision
	if _, err := c.Dispatch(WithDirectExecution(context.Background()), sideToolsCommand("open", rev)); err == nil || a.toolsCalls != 0 {
		t.Fatal("Direct without binding validator reached tools provider")
	}
	revoked := false
	ctx, cancel := context.WithCancel(WithDirectExecutionValidator(context.Background(), strings.Repeat("a", 64), func() error {
		if revoked {
			return errors.New("revoked")
		}
		return nil
	}))
	rev = sideToolsCatalog(t, c, ctx).Revision
	openSideTools(t, c, ctx, rev)
	check := a.liveValidate
	cancel()
	if err := check(); err != nil {
		t.Fatalf("request cancellation killed binding lease: %v", err)
	}
	other := WithDirectExecutionValidator(context.Background(), strings.Repeat("b", 64), func() error { return nil })
	if _, err := c.Dispatch(other, sideToolsCommand("read", rev)); err == nil {
		t.Fatal("different Direct binding read another owner's private chain")
	}
	revoked = true
	if check() == nil {
		t.Fatal("revoked binding retained execution authority")
	}
	before := a.toolsCalls
	if _, err := c.Dispatch(WithDirectExecutionValidator(context.Background(), strings.Repeat("a", 64), func() error { return errors.New("revoked") }), sideToolsCommand("read", rev)); err == nil || a.toolsCalls != before {
		t.Fatal("revoked Direct read reached provider")
	}
}

func TestSideQuestionToolsRechecksSourceProjectAndParentAfterProvider(t *testing.T) {
	for _, variant := range []string{"cwd-before", "projects-before", "api-after", "manager-after"} {
		t.Run(variant, func(t *testing.T) {
			c, a := newSideToolsFixture(t)
			rev := sideToolsCatalog(t, c, context.Background()).Revision
			change := func() {
				switch variant {
				case "cwd-before":
					a.metadata.Cwd = t.TempDir()
				case "projects-before":
					if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.Projects = nil; return nil }); err != nil {
						t.Fatal(err)
					}
				case "api-after":
					if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.CodexModel = "changed"; return nil }); err != nil {
						t.Fatal(err)
					}
				case "manager-after":
					c.SetManager(&fakeManager{list: []agents.Agent{a.aggregationFixtureAgent}})
				}
			}
			if strings.HasSuffix(variant, "before") {
				a.beforeTools = change
			} else {
				a.afterTools = change
			}
			if _, err := c.Dispatch(context.Background(), sideToolsCommand("open", rev)); err == nil {
				t.Fatal("changed source or project returned private state")
			}
		})
	}
}

func TestSideQuestionToolsWorkspaceAdmissionAndSafetyActions(t *testing.T) {
	c, a := newSideToolsFixture(t)
	ctx := context.Background()
	rev := sideToolsCatalog(t, c, ctx).Revision
	openSideTools(t, c, ctx, rev)
	c.rememberWorkspaceSession("codex:s", a.metadata.Cwd, true)
	before := a.toolsCalls
	if _, err := c.Dispatch(ctx, sideToolsCommand("send", rev)); !errors.Is(err, errWorkspaceBusy) || a.toolsCalls != before {
		t.Fatalf("busy parent accepted private write: %v", err)
	}
	for _, action := range []string{"read", "stop", "close"} {
		if _, err := c.Dispatch(ctx, sideToolsCommand(action, rev)); err != nil {
			t.Fatalf("busy project blocked %s: %v", action, err)
		}
	}
	deny := sideToolsCommand("approve", rev)
	deny["decision"] = "deny"
	if _, err := c.Dispatch(ctx, deny); err != nil {
		t.Fatalf("busy project blocked denial: %v", err)
	}
	c.rememberWorkspaceSession("codex:s", a.metadata.Cwd, false)
	a.reservations = []protocol.SessionInfo{{SessionKey: "side-tools:" + a.owner + ":" + sideToolsChain, Cwd: a.metadata.Cwd, Status: "running"}}
	if _, err := c.Dispatch(ctx, sideToolsCommand("send", rev)); err != nil {
		t.Fatalf("private reservation blocked its own owner: %v", err)
	}
	if _, err := c.Dispatch(ctx, map[string]any{"type": "session.send", "sessionKey": "codex:s", "controlSurface": "cli", "text": "ordinary main task"}); !errors.Is(err, errWorkspaceBusy) {
		t.Fatalf("main task ignored temporary reservation: %v", err)
	}
	if len(a.sent) != 0 {
		t.Fatal("blocked main task reached parent worker")
	}
}

func TestSideQuestionToolsRejectsForeignAndUnboundedState(t *testing.T) {
	for _, variant := range []string{"session", "chain", "revision", "permission", "answer", "events", "sequence", "turn", "can-send", "event-owner", "event-tool", "json-budget"} {
		t.Run(variant, func(t *testing.T) {
			c, a := newSideToolsFixture(t)
			rev := sideToolsCatalog(t, c, context.Background()).Revision
			a.mutateResult = func(s *protocol.SideQuestionToolState) {
				switch variant {
				case "session":
					s.SessionKey = "codex:other"
				case "chain":
					s.ChainID = sideToolsOperation
				case "revision":
					s.Revision = "other"
				case "permission":
					s.Permission = "full-access"
				case "answer":
					s.Answer = strings.Repeat("a", (64<<10)+1)
				case "events":
					s.Events = make([]protocol.Event, 129)
				case "sequence":
					s.Sequence = 9007199254740992
				case "turn":
					s.TurnID = "bad turn"
				case "can-send":
					s.State = "running"
				case "event-owner":
					s.Events = []protocol.Event{{SessionKey: "codex:other", Tool: "codex"}}
				case "event-tool":
					s.Events = []protocol.Event{{SessionKey: "codex:s", Tool: "claude"}}
				case "json-budget":
					s.Events = []protocol.Event{{SessionKey: "codex:s", Tool: "codex", Text: strings.Repeat("a", 241<<10)}}
				}
			}
			if _, err := c.Dispatch(context.Background(), sideToolsCommand("open", rev)); !errors.Is(err, errSideToolsInvalid) {
				t.Fatalf("invalid private state escaped: %v", err)
			}
		})
	}
}

func TestSideQuestionToolsStripsParentAttachmentAndChildHandles(t *testing.T) {
	c, a := newSideToolsFixture(t)
	rev := sideToolsCatalog(t, c, context.Background()).Revision
	a.mutateResult = func(s *protocol.SideQuestionToolState) {
		s.Events = []protocol.Event{{SessionKey: "codex:s", Tool: "codex", Type: "message", ImageData: []string{"private-image"}, Session: &protocol.SessionInfo{SessionKey: "codex:private"}, ChildSessionKeys: []string{"codex:private-child"}}}
	}
	value, err := c.Dispatch(context.Background(), sideToolsCommand("open", rev))
	if err != nil {
		t.Fatal(err)
	}
	e := value.(protocol.SideQuestionToolState).Events[0]
	if len(e.ImageData) != 0 || e.Session != nil || len(e.ChildSessionKeys) != 0 {
		t.Fatal("private image or child-session handles escaped into parent response")
	}
}
