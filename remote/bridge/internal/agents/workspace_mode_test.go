package agents

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"salcara/bridge/internal/protocol"
)

func TestWorkspaceModePersistsAcrossBridgeManagerRestart(t *testing.T) {
	stateDir := t.TempDir()
	makeManager := func() *manager {
		return NewManagerWithOptions(nil, func() Settings { return Settings{} }, Options{StateDir: stateDir}).(*manager)
	}
	first := makeManager()
	key := "codex:workspace-mode-fixture"
	if err := first.RememberWorkspaceMode(key, protocol.WorkspaceModeWork); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "session-workspace-modes.json")); err != nil {
		t.Fatal(err)
	}
	second := makeManager()
	info := protocol.SessionInfo{SessionKey: key}
	second.ApplyWorkspaceMode(&info)
	if info.WorkspaceMode != protocol.WorkspaceModeWork {
		t.Fatalf("mode did not survive restart: %+v", info)
	}

	otherAgent := protocol.SessionInfo{SessionKey: "claude:workspace-mode-fixture"}
	second.ApplyWorkspaceMode(&otherAgent)
	if otherAgent.WorkspaceMode != "" {
		t.Fatal("workspace mode crossed agent identity")
	}
}

func TestWorkspaceModeRejectsInvalidModeAndForeignSession(t *testing.T) {
	m := NewManagerWithOptions(nil, func() Settings { return Settings{} }, Options{StateDir: t.TempDir()}).(*manager)
	if err := m.RememberWorkspaceMode("codex:fixture", protocol.WorkspaceMode("work-ish")); err == nil {
		t.Fatal("invalid mode accepted")
	}
	if err := m.RememberWorkspaceMode("claude:fixture", protocol.WorkspaceModeCode); err != nil {
		t.Fatal(err)
	}
	info := protocol.SessionInfo{SessionKey: "codex:fixture"}
	m.ApplyWorkspaceMode(&info)
	if info.WorkspaceMode != "" {
		t.Fatal("another agent's mode leaked")
	}
}

func TestWorkspaceModeFileIsBoundedAndMalformedStateIsIgnored(t *testing.T) {
	stateDir := t.TempDir()
	path := filepath.Join(stateDir, "session-workspace-modes.json")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), maxWorkspaceModeFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewManagerWithOptions(nil, func() Settings { return Settings{} }, Options{StateDir: stateDir}).(*manager)
	info := protocol.SessionInfo{SessionKey: "codex:fixture"}
	m.ApplyWorkspaceMode(&info)
	if info.WorkspaceMode != "" {
		t.Fatal("oversized state file was applied")
	}
}
