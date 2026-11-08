package agents

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"salcara/bridge/internal/protocol"
)

func TestLegacyInterruptCannotStopAnUnspecifiedTurn(t *testing.T) {
	a, _, _ := newResumeSafetyAgent(t, cxThread{}, false)
	tr := a.threadLocked("target")
	tr.turnID, tr.loaded = "actual-current", true
	s := &conversationInterruptTransport{conn:a.conn}
	a.conn.stdin = s
	if err := a.Interrupt(context.Background(), "target"); !errors.Is(err, ErrExpectedTurnRequired) || s.call != nil {
		t.Fatalf("legacy stop operated on a turn the caller never observed: %v",err)
	}
}

func TestClaudePublishesStableTurnIdentityFromAdmissionThroughTerminal(t *testing.T) {
	rec := &recorder{}
	a := newClaudeAgent(rec.sink, func() Settings { return Settings{} }, "not-launched", t.TempDir())
	t.Cleanup(a.Close)
	p := claudeSyntheticPermissionWorker(t,a,"controls-fixture")
	p.stdin = &claudeDeliveryFixtureWriter{write:func(b []byte)(int,error){ return len(b),nil }}
	if err := a.writeTurn(p,"synthetic",nil,nil); err != nil { t.Fatal(err) }
	var started protocol.Event
	for _,e := range rec.all(){ if e.Type=="turn" && e.Status=="started" { started=e } }
	if started.TurnID == "" { t.Fatal("admitted turn has no stable target") }
	a.mu.Lock(); info := a.live[p.id]; a.mu.Unlock()
	if info.ActiveTurnID != started.TurnID { t.Fatal("live session did not expose actual target") }
	a.handleLine(p, json.RawMessage(`{"type":"result","subtype":"success"}`))
	a.mu.Lock(); info=a.live[p.id]; a.mu.Unlock()
	if info.ActiveTurnID!="" || info.LastTerminalTurnID!=started.TurnID { t.Fatal("terminal target lost or active target not retired") }
}
