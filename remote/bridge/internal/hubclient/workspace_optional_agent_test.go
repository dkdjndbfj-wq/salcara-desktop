package hubclient

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/protocol"
)

type optionalWorkspaceAgent struct {
	*workspaceListingAgent
	err error
}

func (a *optionalWorkspaceAgent) Sessions(context.Context) ([]protocol.SessionInfo, error) {
	return nil, a.err
}

func TestWorkspaceAdmissionSkipsOnlyMissingCodex(t *testing.T) {
	for _, tc := range []struct {
		name, id string
		err      error
		busy     bool
		code     string
	}{
		{"not-installed", "codex", agents.ErrCodexNotInstalled, false, ""},
		{"wrapped-not-installed", "codex", fmt.Errorf("inventory: %w", agents.ErrCodexNotInstalled), false, ""},
		{"installed-rpc-failed", "codex", errors.New("RPC failed"), false, "workspace_admission_unavailable"},
		{"untyped-missing-message", "codex", errors.New(agents.ErrCodexNotInstalled.Error()), false, "workspace_admission_unavailable"},
		{"other-adapter-error", "claude", agents.ErrCodexNotInstalled, false, "workspace_admission_unavailable"},
		{"missing-with-active-reservation", "codex", agents.ErrCodexNotInstalled, true, "workspace_busy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cwd := t.TempDir()
			c := &Client{}
			if tc.busy {
				c.rememberWorkspaceSession("codex:active", cwd, true)
			}
			a := &optionalWorkspaceAgent{workspaceListingAgent: &workspaceListingAgent{fakeAgent: &fakeAgent{id: tc.id}}, err: tc.err}
			err := c.checkWorkspaceAdmission(context.Background(), &fakeManager{list: []agents.Agent{a}}, cwd, "")
			if tc.code == "" {
				if err != nil || !c.workspaceScanned {
					t.Fatalf("absent optional adapter blocked admission: %v", err)
				}
				return
			}
			if protocol.ExecutionErrorCode(err) != tc.code || c.workspaceScanned {
				t.Fatalf("unsafe admission: err=%v scanned=%v", err, c.workspaceScanned)
			}
		})
	}
}
