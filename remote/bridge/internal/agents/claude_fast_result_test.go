package agents

import (
	"context"
	"testing"

	"salcara/bridge/internal/protocol"
)

func TestClaudeFastResultCannotPrecedeLocalStartOrLeavePhantomBusy(t *testing.T) {
	rec := &recorder{}
	a := newClaudeAgent(rec.sink, func() Settings { return Settings{ClaudeRoot: "http://127.0.0.1:1", ClaudeKey: "synthetic-api-key"} }, "not-launched", t.TempDir())
	t.Cleanup(a.Close)
	a.sink = func(e protocol.Event) {
		// Admission events must not hold a.mu/p.mu while calling a sink. A
		// real sink can inspect remote busy state or publish into another loop.
		if e.Type == "message" && e.Role == "user" || e.Type == "turn" && e.Status == "started" {
			if !a.ActiveRemoteTurns() {
				t.Error("send admission did not reserve busy state before publishing")
			}
		}
		rec.sink(e)
	}
	p := claudeSyntheticPermissionWorker(t, a, "fixture-session")
	p.mapper = newClaudeMapper("claude:"+p.id, p.cwd)
	p.workerSettingsSig = claudeSettingsSig(a.settings(), a.exeOverride)
	p.stdin = &claudeDeliveryFixtureWriter{write: func(data []byte) (int, error) {
		// A real stdout reader can receive a result before an owned pipe Write
		// returns. Make that ordering deterministic without launching a tool.
		a.handleLine(p, []byte(`{"type":"assistant","message":{"id":"synthetic-message","content":[{"type":"text","text":"synthetic result"}]}}`))
		a.handleLine(p, []byte(`{"type":"result","subtype":"success","result":"synthetic result"}`))
		return len(data), nil
	}}
	if err := a.Send(context.Background(), p.id, "synthetic input"); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	pending := p.turns
	p.mu.Unlock()
	if pending != 0 || a.ActiveRemoteTurns() {
		t.Fatal("completed fast result left a phantom active turn")
	}
	user, started, assistant, completed := -1, -1, -1, -1
	for i, e := range rec.all() {
		switch {
		case e.Type == "message" && e.Role == "user":
			user = i
		case e.Type == "turn" && e.Status == "started":
			started = i
		case e.Type == "message" && e.Role == "assistant":
			assistant = i
		case e.Type == "turn" && e.Status == "completed":
			completed = i
		}
	}
	if user < 0 || started <= user || assistant <= started || completed <= assistant {
		t.Fatal("fast result reordered local user/start after assistant/completion")
	}
	a.mu.Lock()
	status := a.live[p.id].Status
	a.mu.Unlock()
	if status != "idle" {
		t.Fatal("local send overwrote an authoritative completed result with running")
	}
}
