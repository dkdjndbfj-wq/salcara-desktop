package hubclient

import (
	"context"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/protocol"
)

type workspaceModeFixtureManager struct {
	*fakeManager
	modes map[string]protocol.WorkspaceMode
}

func (m *workspaceModeFixtureManager) RememberWorkspaceMode(key string, mode protocol.WorkspaceMode) error {
	m.modes[key] = mode
	return nil
}

func (m *workspaceModeFixtureManager) ApplyWorkspaceMode(info *protocol.SessionInfo) {
	if mode := m.modes[info.SessionKey]; mode == protocol.WorkspaceModeCode || mode == protocol.WorkspaceModeWork {
		info.WorkspaceMode = mode
	}
}

func TestCommandWorkspaceModeDefaultsAndValidates(t *testing.T) {
	for _, test := range []struct {
		name string
		cmd  map[string]any
		want protocol.WorkspaceMode
		bad  bool
	}{
		{name: "legacy default", cmd: map[string]any{}, want: protocol.WorkspaceModeCode},
		{name: "code", cmd: map[string]any{"workspaceMode": "code"}, want: protocol.WorkspaceModeCode},
		{name: "work", cmd: map[string]any{"workspaceMode": "work"}, want: protocol.WorkspaceModeWork},
		{name: "unknown", cmd: map[string]any{"workspaceMode": "other"}, bad: true},
		{name: "wrong type", cmd: map[string]any{"workspaceMode": false}, bad: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := commandWorkspaceMode(test.cmd)
			if test.bad {
				if err == nil {
					t.Fatal("invalid workspace mode accepted")
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("mode=%q err=%v", got, err)
			}
		})
	}
}

func TestWorkspaceModeSurvivesSessionStartDispatch(t *testing.T) {
	for _, mode := range []string{"code", "work"} {
		t.Run(mode, func(t *testing.T) {
			c, a, cwd := turnFeatureFixture(t)
			_, err := c.Dispatch(context.Background(), map[string]any{
				"type": "session.start", "tool": "hermes", "cwd": cwd, "prompt": "task", "workspaceMode": mode,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(a.options) != 1 || string(a.options[0].WorkspaceMode) != mode {
				t.Fatalf("workspace mode was dropped: %+v", a.options)
			}
		})
	}
}

func TestWorkspaceModeRoundTripsThroughListAndOpen(t *testing.T) {
	for _, mode := range []string{"code", "work"} {
		t.Run(mode, func(t *testing.T) {
			c, a, cwd := turnFeatureFixture(t)
			m := &workspaceModeFixtureManager{fakeManager: &fakeManager{list: []agents.Agent{a}}, modes: map[string]protocol.WorkspaceMode{}}
			c.SetManager(m)
			if _, err := c.Dispatch(context.Background(), map[string]any{
				"type": "session.start", "tool": "hermes", "cwd": cwd, "prompt": "task", "workspaceMode": mode,
			}); err != nil {
				t.Fatal(err)
			}
			a.sessions = []protocol.SessionInfo{{SessionKey: "hermes:created", Tool: "hermes", Cwd: cwd}}

			listed, err := c.Dispatch(context.Background(), map[string]any{"type": "sessions.list", "tool": "hermes"})
			if err != nil {
				t.Fatal(err)
			}
			rows := listed.(map[string]any)["sessions"].([]protocol.SessionInfo)
			if len(rows) != 1 || string(rows[0].WorkspaceMode) != mode {
				t.Fatalf("list lost workspace mode: %+v", rows)
			}

			opened, err := c.Dispatch(context.Background(), map[string]any{"type": "session.open", "sessionKey": "hermes:created"})
			if err != nil {
				t.Fatal(err)
			}
			info := opened.(map[string]any)["session"].(protocol.SessionInfo)
			if string(info.WorkspaceMode) != mode {
				t.Fatalf("open lost workspace mode: %+v", info)
			}
		})
	}
}

func TestInvalidWorkspaceModeDoesNotStartSession(t *testing.T) {
	c, a, cwd := turnFeatureFixture(t)
	if _, err := c.Dispatch(context.Background(), map[string]any{
		"type": "session.start", "tool": "hermes", "cwd": cwd, "prompt": "task", "workspaceMode": "work-ish",
	}); err == nil {
		t.Fatal("invalid workspace mode accepted")
	}
	if len(a.options) != 0 || len(a.started) != 0 {
		t.Fatal("invalid workspace mode reached the Agent")
	}
}
