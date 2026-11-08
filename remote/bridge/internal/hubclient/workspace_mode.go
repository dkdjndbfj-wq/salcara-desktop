package hubclient

import (
	"errors"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/protocol"
)

func commandWorkspaceMode(cmd map[string]any) (protocol.WorkspaceMode, error) {
	raw, present := cmd["workspaceMode"]
	if !present {
		return protocol.WorkspaceModeCode, nil // compatibility with older phone versions
	}
	value, ok := raw.(string)
	if !ok {
		return "", errors.New("workspace_mode_invalid: 工作区必须是 Code 或 Work")
	}
	mode := protocol.WorkspaceMode(value)
	if mode != protocol.WorkspaceModeCode && mode != protocol.WorkspaceModeWork {
		return "", errors.New("workspace_mode_invalid: 工作区必须是 Code 或 Work")
	}
	return mode, nil
}

func applyWorkspaceMode(m agents.Manager, session *protocol.SessionInfo) {
	agents.ApplyRememberedWorkspaceMode(m, session)
}
