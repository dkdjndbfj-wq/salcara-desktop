package agents

import (
	"context"
	"testing"

	"salcara/bridge/internal/protocol"
)

func TestCodexWatchdogSnapshotDoesNotStartIdleAppServer(t *testing.T) {
	a := newCodexAgent(func(protocol.Event) {}, func() Settings { return Settings{} }, "", "")
	defer a.Close()

	sessions, err := a.WatchdogSessions(context.Background())
	if err != nil || len(sessions) != 0 {
		t.Fatalf("idle watchdog snapshot = %+v, %v", sessions, err)
	}
	a.mu.Lock()
	conn, starting := a.conn, a.starting
	a.mu.Unlock()
	if conn != nil || starting != nil {
		t.Fatal("watchdog started Codex while no live turn existed")
	}
}

func TestClaudeWatchdogSnapshotIncludesOnlyOwnedActiveTurns(t *testing.T) {
	a := newClaudeAgent(func(protocol.Event) {}, func() Settings { return Settings{} }, "", t.TempDir())
	proc := &claudeProc{id: "owned", done: make(chan struct{}), stopCh: make(chan struct{}), turns: 1, turnIDs: []string{"turn-owned"}}
	a.mu.Lock()
	a.procs[proc.id] = proc
	a.live[proc.id] = protocol.SessionInfo{SessionKey: "claude:owned", Status: "running", UpdatedAt: 42}
	a.mu.Unlock()

	sessions, err := a.WatchdogSessions(context.Background())
	if err != nil || len(sessions) != 1 {
		t.Fatalf("owned watchdog snapshot = %+v, %v", sessions, err)
	}
	if sessions[0].SessionKey != "claude:owned" || sessions[0].ActiveTurnID != "turn-owned" || sessions[0].Status != "running" {
		t.Fatalf("watchdog snapshot lost exact worker/turn identity: %+v", sessions[0])
	}
	a.mu.Lock()
	delete(a.procs, proc.id)
	a.mu.Unlock()
	a.Close()
}
