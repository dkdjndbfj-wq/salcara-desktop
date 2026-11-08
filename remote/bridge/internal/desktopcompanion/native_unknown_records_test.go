package desktopcompanion

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNativeFutureItemsAreSafePlaceholdersNotSilentSuccess(t *testing.T) {
	var read nativeRead
	if err := json.Unmarshal([]byte(`{"turns":[{"id":"turn","status":"completed","items":[{"id":"future","type":"futureTool","status":"completed","arguments":{"secret":"private-fixture"},"output":"private-fixture","path":"private-fixture","receiverThreadIds":["private-fixture"]}]}]}`), &read); err != nil {
		t.Fatal(err)
	}
	events := nativeEvents("codex:fixture", read)
	encoded, _ := json.Marshal(events)
	if len(events) != 2 || events[0].Type != "notice" || events[0].ErrorCode != "unsupported_item" || events[0].ID != "future" || events[0].TurnID != "turn" || events[0].Status != "" || strings.Contains(string(encoded), "private-fixture") {
		t.Fatalf("future item dropped or unsafe payload projected: %+v", events)
	}
}

func TestNativeFutureTurnStateDoesNotFabricateCompletion(t *testing.T) {
	for _, status := range []string{"futureTerminal", "", "inProgress"} {
		events := nativeEvents("codex:fixture", nativeRead{Turns: []nativeTurn{{ID: "turn", Status: status, DurationMS: 1200}}})
		if status == "" {
			if len(events) != 0 {
				t.Fatalf("missing state was guessed: %+v", events)
			}
		} else if status == "inProgress" {
			if len(events) != 1 || events[0].Status != "started" {
				t.Fatalf("duration turned busy into completed: %+v", events)
			}
		} else if len(events) != 1 || events[0].Type != "notice" || events[0].ErrorCode != "unsupported_turn_state" {
			t.Fatalf("future turn state became completed: %+v", events)
		}
	}
}
