package hubclient

import (
	"context"
	"errors"
	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/protocol"
	"testing"
)

type failingHistoryAgent struct{ *fakeAgent }

func (a *failingHistoryAgent) Sessions(context.Context) ([]protocol.SessionInfo, error) {
	return nil, errors.New("fixture scan failed")
}
func TestSessionListReportsPartialFailureAndCancellation(t *testing.T) {
	good := &fakeAgent{id: "codex", sessions: []protocol.SessionInfo{{SessionKey: "codex:local"}}}
	bad := &failingHistoryAgent{&fakeAgent{id: "claude"}}
	m := &fakeManager{list: []agents.Agent{good, bad}}
	r, err := ListSessions(context.Background(), m, "")
	if err != nil || len(r["sessions"].([]protocol.SessionInfo)) != 1 || len(r["errors"].([]string)) != 1 {
		t.Fatalf("partial scan was hidden: %v", err)
	}
	failed, ok := r["failedTools"].([]string)
	if !ok || len(failed) != 1 || failed[0] != "claude" {
		t.Fatalf("failed history source was not identified: %v", r["failedTools"])
	}
	if _, err := ListSessions(context.Background(), m, "claude"); err == nil {
		t.Fatal("failed scan returned empty success")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ListSessions(ctx, m, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled scan continued: %v", err)
	}
}
