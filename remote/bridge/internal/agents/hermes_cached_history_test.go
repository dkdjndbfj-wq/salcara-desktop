package agents

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"salcara/bridge/internal/protocol"
)

func TestHermesUnavailableRuntimeKeepsCachedHistoryReadOnly(t *testing.T) {
	a, _, settings, snapshot := hermesFixture(t)
	id := hermesStart(t, a, snapshot)
	a.emit(id, protocol.Event{Type: "message", Role: "user", Text: "original API A prompt"})
	a.emit(id, protocol.Event{Type: "message", Role: "assistant", Text: "original answer", Final: true})
	a.emit(id, protocol.Event{Type: "approval.request", ApprovalID: "stale-approval"})
	a.Close()
	restored := newHermesAgent(func() Settings { return *settings }, filepath.Dir(a.stateDir))
	defer restored.Close()
	restored.dial = func(context.Context, func(string, json.RawMessage)) (hermesGateway, error) {
		return nil, errors.New("synthetic unavailable runtime with private diagnostic")
	}
	info, events, err := restored.Open(context.Background(), id)
	if err != nil || info.Controllable || info.Status != "unknown" || info.ActiveTurnID != "" || !restored.ActiveRemoteTurns() {
		t.Fatalf("offline history implied live ownership: %+v %v", info, err)
	}
	var text strings.Builder
	for _, event := range events {
		if event.Type == "approval.request" {
			t.Fatal("cached history revived a stale approval")
		}
		text.WriteString(event.Text)
	}
	if !strings.Contains(text.String(), "original API A prompt") || !strings.Contains(text.String(), "original answer") || !strings.Contains(text.String(), "上次保存") || strings.Contains(text.String(), "private diagnostic") {
		t.Fatal("offline transcript lost history or exposed runtime diagnostics")
	}
	if err := restored.Send(context.Background(), id, "must not replay"); err == nil {
		t.Fatal("cached history allowed execution")
	}
}
