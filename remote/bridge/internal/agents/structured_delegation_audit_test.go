package agents

import (
	"encoding/json"
	"strings"
	"testing"

	"salcara/bridge/internal/protocol"
)

// The same explicitly observed native item must keep its message category when
// read through app-server history. Client IDs/text are not sender identity proof.
func TestAuditCodexHistoryPreservesExplicitDelegationInputAndCategory(t *testing.T) {
	content := json.RawMessage(`{"type":"userMessage","id":"delegated-message","clientId":"synthetic-client","content":[{"type":"text","text":"native wrapper","codexDelegation":{"input":"synthetic delegated input","sourceThreadId":"private-source-must-not-be-inferred"}}]}`)
	thread := cxThread{ID: "receiver", Turns: []cxTurn{{ID: "delegated-turn", Status: "completed", Items: []json.RawMessage{content}}}}
	events := codexHistory("codex:receiver", thread)
	var message *protocol.Event
	for i := range events {
		if events[i].Type == "message" {
			message = &events[i]
			break
		}
	}
	if message == nil || message.Text != "synthetic delegated input" || message.MessageOrigin == nil || message.MessageOrigin.Kind != "delegation" {
		t.Fatalf("structured delegated message lost its input/category on the app-server history path: %+v", message)
	}
	raw, err := json.Marshal(message)
	if err != nil || strings.Contains(string(raw), "private-source") {
		t.Fatal("unverified source-thread field became public sender metadata")
	}
}
