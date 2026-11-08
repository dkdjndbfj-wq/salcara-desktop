package hubclient

import (
	"encoding/json"
	"strings"
	"testing"

	"salcara/bridge/internal/protocol"
)

func TestConversationMetadataAndAttachmentsAreIncludedInQueueBudget(t *testing.T) {
	e := protocol.Event{Type: "message", SessionKey: "codex:fixture", Role: "user", Text: "fixture"}
	baseline := eventWeight(e)
	e.ClientID = strings.Repeat("\x00", 256)
	e.MessagePhase, e.ErrorCode = "commentary", "model_capacity"
	e.MessageOrigin = &protocol.MessageOrigin{Kind: "delegation"}
	e.ApprovalOptions = &protocol.ApprovalOptions{Version: 1, Decisions: []string{"allow", "allow_session", "deny"}}
	e.Session = &protocol.SessionInfo{AgentNickname: strings.Repeat("名", 160), AgentRole: strings.Repeat("role", 40), ForkedFromSessionKey: "codex:fixture-parent"}
	for i := 0; i < 16; i++ {
		e.Attachments = append(e.Attachments, protocol.AttachmentRef{ID: "att_" + strings.Repeat("x", 128), Mime: "image/png", Name: strings.Repeat("\x00", 160), SHA256: strings.Repeat("f", 64)})
	}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if eventWeight(e) <= baseline || eventWeight(e) < len(data) {
		t.Fatalf("metadata wire size exceeds retained budget: weight=%d wire=%d baseline=%d", eventWeight(e), len(data), baseline)
	}
}
