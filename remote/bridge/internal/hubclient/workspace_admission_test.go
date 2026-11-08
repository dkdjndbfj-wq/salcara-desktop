package hubclient

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/protocol"
)

type workspaceContinuationAgent struct {
	*aggregationFixtureAgent
	cwd       string
	continued int
	readErr   error
}

func (a *workspaceContinuationAgent) SessionMetadata(_ context.Context, id string) (protocol.SessionInfo, error) {
	return protocol.SessionInfo{SessionKey: a.ID() + ":" + id, Cwd: a.cwd}, a.readErr
}

func (a *workspaceContinuationAgent) ContinueWithOptions(context.Context, string, string, string, string, agents.TurnOptions) (protocol.ContinueResult, error) {
	a.continued++
	return protocol.ContinueResult{Accepted: true}, nil
}

func TestWorkspaceContinueUsesSameAdmissionAsSend(t *testing.T) {
	for _, family := range []string{"codex", "claude"} {
		for _, state := range []string{"overlapping", "independent", "self", "metadata-unavailable"} {
			t.Run(family+"/"+state, func(t *testing.T) {
				c, base, cwd := aggregationFixture(t, family)
				base.descriptor.Capabilities.Continue = true
				a := &workspaceContinuationAgent{aggregationFixtureAgent: base, cwd: filepath.Join(cwd, "nested")}
				c.SetManager(&fakeManager{list: []agents.Agent{a}})
				c.workspaceScanned = true
				key, busyCwd := family+":other", cwd
				switch state {
				case "independent":
					busyCwd = t.TempDir()
				case "self":
					key = family + ":target"
				case "metadata-unavailable":
					a.readErr = errors.New("synthetic metadata failure")
				}
				c.rememberWorkspaceSession(key, busyCwd, true)
				_, err := c.Dispatch(context.Background(), map[string]any{
					"type": "session.continue", "sessionKey": family + ":target", "controlSurface": "cli",
					"text": "Continue the original task.", "expectedTurnId": "completed-turn", "operationId": "11111111-2222-4333-8444-555555555555",
				})
				blocked := state == "overlapping" || state == "metadata-unavailable"
				if blocked {
					if err == nil || a.continued != 0 {
						t.Fatalf("workspace refusal did not precede native continuation: calls=%d err=%v", a.continued, err)
					}
					var failure workspaceAdmissionError
					if !errors.As(err, &failure) {
						t.Fatalf("unexpected refusal: %v", err)
					}
				} else if err != nil || a.continued != 1 {
					t.Fatalf("independent continuation blocked: calls=%d err=%v", a.continued, err)
				}
			})
		}
	}
}

type workspaceListingAgent struct{ *fakeAgent }

func (a *workspaceListingAgent) Descriptor() protocol.AgentDescriptor {
	return protocol.AgentDescriptor{ID: a.id, Name: a.Name(), Tool: a.id, ControlSurface: "cli",
		Capabilities: protocol.AgentCapabilities{List: true, Read: true}}
}

func TestWorkspacePathsOverlapIncludesNestedProjects(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	nested := filepath.Join(root, "service")
	if !workspacePathsOverlap(root, nested) {
		t.Fatal("nested project was not treated as overlapping")
	}
	if workspacePathsOverlap(filepath.Join(base, "repo-one"), filepath.Join(base, "repo-two")) {
		t.Fatal("sibling projects were treated as overlapping")
	}
}

func TestWorkspaceAdmissionBlocksActiveRegistryLease(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo")
	nested := filepath.Join(root, "service")
	c := &Client{workspaceSessions: map[string]workspaceLease{
		"codex:active": {cwd: root, active: true},
	}}
	c.workspaceScanned = true
	m := &fakeManager{}
	err := c.checkWorkspaceAdmission(context.Background(), m, nested, "")
	if err == nil || !strings.Contains(err.Error(), "工作目录正在被其他任务使用") {
		t.Fatalf("overlapping active task was admitted: %v", err)
	}
	if err := c.checkWorkspaceAdmission(context.Background(), m, filepath.Join(t.TempDir(), "other"), ""); err != nil {
		t.Fatalf("independent project was blocked: %v", err)
	}
	if err := c.checkWorkspaceAdmission(context.Background(), m, nested, "codex:active"); err != nil {
		t.Fatalf("excluded target session was blocked: %v", err)
	}
}

func TestWorkspaceAdmissionReconcilesRunningSessionsAfterRestart(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo")
	nested := filepath.Join(root, "nested")
	a := &workspaceListingAgent{fakeAgent: &fakeAgent{id: "codex", sessions: []protocol.SessionInfo{{
		SessionKey: "codex:old", Cwd: root, Status: "running", Controllable: true,
	}}}}
	c := &Client{workspaceSessions: map[string]workspaceLease{}}
	m := &fakeManager{list: []agents.Agent{a}}
	err := c.checkWorkspaceAdmission(context.Background(), m, nested, "")
	if err == nil || !strings.Contains(err.Error(), "工作目录正在被其他任务使用") {
		t.Fatalf("running session from before restart was not found: %v", err)
	}
	if c.workspaceScanned {
		t.Fatal("failed admission scan was marked complete")
	}
	// Once the old task is idle, a successful scan allows the new task and
	// avoids re-reading every history directory on subsequent admissions.
	a.sessions[0].Status = "idle"
	if err := c.checkWorkspaceAdmission(context.Background(), m, nested, ""); err != nil {
		t.Fatalf("idle project remained blocked: %v", err)
	}
	if !c.workspaceScanned {
		t.Fatal("successful admission scan was not remembered")
	}
}
