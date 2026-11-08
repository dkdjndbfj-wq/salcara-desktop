package hubclient

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/protocol"
)

type cwdHistoryAgent struct {
	fakeAgent
	historyReads int
}

func (a *cwdHistoryAgent) Sessions(context.Context) ([]protocol.SessionInfo, error) {
	a.historyReads++
	return a.sessions, nil
}

func TestConfiguredProjectStartsWithoutLoadingAgentHistories(t *testing.T) {
	root := t.TempDir()
	c, _ := newTestClient(t, "http://127.0.0.1", "synthetic", []protocol.Project{{Path: root}})
	a := &cwdHistoryAgent{fakeAgent: fakeAgent{id: "codex"}}
	b := &cwdHistoryAgent{fakeAgent: fakeAgent{id: "claude"}}
	c.SetManager(&fakeManager{list: []agents.Agent{a, b}})
	if _, err := c.Dispatch(context.Background(), map[string]any{"type": "session.start", "tool": "codex", "cwd": root, "prompt": "fixture"}); err != nil {
		t.Fatal(err)
	}
	if a.historyReads+b.historyReads != 0 || len(a.started) != 1 {
		t.Fatal("saved project loaded histories before launch")
	}
}

func TestCwdAncestorUsesFilesystemIdentity(t *testing.T) {
	root, other := t.TempDir(), t.TempDir()
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	if !sameCwdAncestor(root, child) || !sameCwdAncestor(root, root) || sameCwdAncestor(root, other) {
		t.Fatal("ancestor identity mismatch")
	}
}
