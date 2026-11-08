package agents

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"salcara/bridge/internal/protocol"
	"testing"
)

func TestCooperationCodexNativeSendKeepsUserInputAndOwnReceiptSeparate(t *testing.T) {
	a, transport, _ := newCodexFeatureFixture(t)
	a.stateFile = filepath.Join(t.TempDir(), "state.json")
	op := "11111111-2222-4333-8444-555555555555"
	origin := ownCrossTaskOrigin("codex", "feature-thread", op)
	if err := a.SendWithOptions(context.Background(), "feature-thread", "explicit user input", TurnOptions{ClientUserMessageID: op, CrossTaskOrigin: origin}); err != nil {
		t.Fatal(err)
	}
	calls := featureCalls(transport, "turn/start")
	if len(calls) != 1 || calls[0]["clientUserMessageId"] != op {
		t.Fatal("native exact correlation missing", calls)
	}
	input := calls[0]["input"].([]any)
	if len(input) != 1 || input[0].(map[string]any)["text"] != "explicit user input" {
		t.Fatal("cross-task route changed the user input", input)
	}
	receipt, ok := a.readInputReceipt("feature-thread", "client", op)
	if !ok || !reflect.DeepEqual(receipt.Origin, origin) {
		t.Fatal("native send omitted own durable receipt", receipt)
	}
}

func TestCooperationClaudeNativeSendUsesExactUUIDAndOwnReceipt(t *testing.T) {
	a, p, rec := newClaudeResultFixture(t)
	op := "11111111-2222-4333-8444-555555555555"
	origin := ownCrossTaskOrigin("claude", p.id, op)
	var input map[string]any
	p.stdin = &claudeDeliveryFixtureWriter{write: func(raw []byte) (int, error) {
		if err := json.Unmarshal(raw, &input); err != nil {
			t.Fatal(err)
		}
		return len(raw), nil
	}}
	if err := a.SendWithOptions(context.Background(), p.id, "explicit user input", TurnOptions{ClientUserMessageID: op, CrossTaskOrigin: origin}); err != nil {
		t.Fatal(err)
	}
	var user protocol.Event
	for _, event := range rec.all() {
		if event.Type == "message" && event.Role == "user" {
			user = event
		}
	}
	if !skillReceiptUUID(user.ID) || input["uuid"] != user.ID || user.ClientID != op || !reflect.DeepEqual(user.MessageOrigin, origin) {
		t.Fatal("live receipt identity missing", user, input)
	}
	receipt, ok := a.hist.readSkillReceipt(p.id, user.ID)
	if !ok || !reflect.DeepEqual(receipt.Origin, origin) {
		t.Fatal("native send omitted durable receipt", receipt)
	}
}

func TestCooperationNativeAdmissionRejectsTargetThatBecameBusy(t *testing.T) {
	t.Run("codex", func(t *testing.T) {
		a, transport, _ := newCodexFeatureFixture(t)
		a.stateFile = filepath.Join(t.TempDir(), "state.json")
		op := "11111111-2222-4333-8444-555555555555"
		err := a.SendWithOptions(context.Background(), "feature-thread", "do not queue", TurnOptions{ClientUserMessageID: op, CrossTaskOrigin: ownCrossTaskOrigin("codex", "feature-thread", op), ValidatePermission: func() error {
			a.mu.Lock()
			a.threads["feature-thread"].info.Status = "running"
			a.mu.Unlock()
			return nil
		}})
		if err == nil || len(featureCalls(transport, "turn/start")) != 0 {
			t.Fatal("busy target accepted", err)
		}
	})
	t.Run("claude", func(t *testing.T) {
		a, p, _ := newClaudeResultFixture(t)
		writes := 0
		p.stdin = &claudeDeliveryFixtureWriter{write: func(raw []byte) (int, error) { writes++; return len(raw), nil }}
		op := "11111111-2222-4333-8444-555555555555"
		err := a.SendWithOptions(context.Background(), p.id, "do not queue", TurnOptions{ClientUserMessageID: op, CrossTaskOrigin: ownCrossTaskOrigin("claude", p.id, op), ValidatePermission: func() error { p.mu.Lock(); p.turns = 1; p.mu.Unlock(); return nil }})
		if err == nil || writes != 0 {
			t.Fatal("busy target accepted", err, writes)
		}
	})
}

func ownCrossTaskOrigin(tool, target, operation string) *protocol.MessageOrigin {
	return &protocol.MessageOrigin{Kind: "user_cross_task", Verification: "bridge_receipt", SourceSessionKey: tool + ":source", TargetSessionKey: tool + ":" + target, SourceTitle: "Source task", ReceiptID: operation}
}
func TestCooperationCodexReceiptExactIdentitySurvivesRestart(t *testing.T) {
	op := "11111111-2222-4333-8444-555555555555"
	path := filepath.Join(t.TempDir(), "state.json")
	a := &codexAgent{stateFile: path}
	r := codexInputReceipt{Version: 1, SessionID: "target", ClientID: op, Text: "same text", Origin: ownCrossTaskOrigin("codex", "target", op)}
	if err := a.saveInputReceipt(r, "client", op); err != nil {
		t.Fatal(err)
	}
	e := protocol.Event{Type: "message", Role: "user", SessionKey: "codex:target", ID: "native-exact", ClientID: op, Text: "same text"}
	got := a.projectInputReceipt(e)
	if !reflect.DeepEqual(got.MessageOrigin, r.Origin) {
		t.Fatal("live receipt lost", got)
	}
	restarted := &codexAgent{stateFile: path}
	e.ClientID = ""
	got = restarted.projectInputReceipt(e)
	if !reflect.DeepEqual(got.MessageOrigin, r.Origin) {
		t.Fatal("restart receipt lost", got)
	}
	for _, variant := range []string{"other-id", "other-session", "peer", "assistant", "child"} {
		copy := e
		switch variant {
		case "other-id":
			copy.ID = "other"
		case "other-session":
			copy.SessionKey = "codex:other"
		case "peer":
			copy.MessageOrigin = &protocol.MessageOrigin{Kind: "delegation"}
		case "assistant":
			copy.Role = "assistant"
		case "child":
			copy.ParentID = "child"
		}
		if projected := restarted.projectInputReceipt(copy); projected.MessageOrigin != nil && projected.MessageOrigin.Kind == "user_cross_task" {
			t.Fatal("receipt crossed identity", variant)
		}
	}
}
func TestCooperationClaudeReceiptExactIdentitySurvivesRestart(t *testing.T) {
	op := "11111111-2222-4333-8444-555555555555"
	uuid := "22222222-2222-4333-8444-555555555555"
	dir := filepath.Join(t.TempDir(), "projects")
	h := newClaudeHistory(dir)
	r := claudeSkillReceipt{Version: 1, SessionID: "target", UUID: uuid, ClientID: op, Text: "same text", Origin: ownCrossTaskOrigin("claude", "target", op)}
	if err := h.saveInputReceipt(r); err != nil {
		t.Fatal(err)
	}
	restarted := newClaudeHistory(dir)
	l := claudeLine{Type: "user", UUID: uuid, SessionID: "target", Message: json.RawMessage(`{"role":"user","content":"same text"}`)}
	events := restarted.projectSkillReceipt("target", &l, nil)
	if len(events) != 1 || !reflect.DeepEqual(events[0].MessageOrigin, r.Origin) {
		t.Fatal("receipt not restored", events)
	}
	for _, variant := range []string{"other-id", "peer", "other-session", "child"} {
		copy := l
		switch variant {
		case "other-id":
			copy.UUID = op
		case "peer":
			copy.Origin = json.RawMessage(`{"kind":"peer"}`)
		case "other-session":
			copy.SessionID = "other"
		case "child":
			copy.IsSidechain = true
		}
		if len(restarted.projectSkillReceipt("target", &copy, nil)) != 0 {
			t.Fatal("receipt crossed identity", variant)
		}
	}
}
