package agents

import (
	"context"
	"testing"

	"salcara/bridge/internal/protocol"
)

func TestDeepSeekRestartKeepsUnconfirmedRuntimeFenceAndHistory(t *testing.T) {
	for _, status := range []string{"running", "waiting_approval", "unknown", "failed"} {
		t.Run(status, func(t *testing.T) {
			state := t.TempDir()
			settings := func() Settings { return Settings{} }
			a := newDeepSeekAgent(settings, state)
			id := deepSeekNewID()
			s := &deepSeekSession{Info: protocol.SessionInfo{SessionKey: deepSeekID + ":" + id, Tool: deepSeekID, Cwd: t.TempDir(), Status: status}, Started: true, Unconfirmed: status == "failed"}
			s.Events = []protocol.Event{{Tool: deepSeekID, SessionKey: s.Info.SessionKey, Type: "message", Role: "user", Text: "A conversation"}}
			if err := a.persistLocked(id, s); err != nil {
				t.Fatal(err)
			}
			b := newDeepSeekAgent(settings, state)
			info, events, err := b.Open(context.Background(), id)
			if err != nil || info.Status != "unknown" || info.Controllable || !b.ActiveRemoteTurns() || len(events) < 2 || events[0].Text != "A conversation" {
				t.Fatalf("unsafe restoration: %+v %+v %v", info, events, err)
			}
			if err := b.Send(context.Background(), id, "must not replay"); err == nil {
				t.Fatal("restarted an unconfirmed runtime")
			}
			b.Close()
			c := newDeepSeekAgent(settings, state)
			if !c.ActiveRemoteTurns() {
				t.Fatal("a second clean Bridge restart erased the fence")
			}
		})
	}
}
