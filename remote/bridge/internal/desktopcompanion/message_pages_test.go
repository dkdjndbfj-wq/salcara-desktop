package desktopcompanion

import (
	"context"
	"fmt"
	"testing"
)

func TestNativeMessagePagesPreserveUnreadPartOfTurnAndItsTools(t *testing.T) {
	f := gatewayFixture(t)
	items := []any{}
	for i := 0; i < 7; i++ {
		items = append(items, map[string]any{"id": fmt.Sprint("m", i), "type": "agentMessage", "text": fmt.Sprint("answer", i)}, map[string]any{"id": fmt.Sprint("t", i), "type": "commandExecution", "command": "fixture"})
	}
	f.result = func(cmd map[string]any) any {
		if cmd["cursor"] != nil {
			t.Error("advanced host cursor before exhausting one turn")
		}
		return nativeEnvelope(map[string]any{"schemaVersion": 1, "thread": map[string]any{"id": targetID, "kind": "codex", "hostId": "local"}, "turns": []any{map[string]any{"id": "turn-original", "items": items}}})
	}
	seen := map[string]bool{}
	cursor := ""
	for n := 0; n < 4; n++ {
		_, page, next, err := f.service.NativeOpenMessagesPage(context.Background(), "codex:"+targetID, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		messages := 0
		for _, event := range page {
			if event.Type == "message" {
				messages++
			}
			if seen[event.ID] {
				t.Fatal("duplicate native event", event.ID)
			}
			seen[event.ID] = true
		}
		if messages > 2 {
			t.Fatal("turn count was used as message count")
		}
		cursor = next
	}
	if cursor != "" || len(seen) != 15 {
		t.Fatal("unread history was skipped", len(seen))
	}
}

func TestNativeMessagePageCursorDoesNotCrossLeaseOrThread(t *testing.T) {
	f := gatewayFixture(t)
	f.result = func(map[string]any) any {
		return nativeEnvelope(map[string]any{"schemaVersion": 1, "thread": map[string]any{"id": targetID, "kind": "codex", "hostId": "local"}, "turns": []any{map[string]any{"id": "turn", "items": []any{map[string]any{"id": "m1", "type": "agentMessage", "text": "first"}, map[string]any{"id": "m2", "type": "agentMessage", "text": "second"}, map[string]any{"id": "m3", "type": "agentMessage", "text": "third"}}}}})
	}
	_, _, cursor, err := f.service.NativeOpenMessagesPage(context.Background(), "codex:"+targetID, "", 2)
	if err != nil || cursor == "" {
		t.Fatal(err)
	}
	if _, _, _, err := f.service.NativeOpenMessagesPage(context.Background(), "codex:"+otherID, cursor, 2); err == nil {
		t.Fatal("cross-thread cursor accepted")
	}
	f.descriptor.ExpiresAt++
	f.write(t)
	if _, _, _, err := f.service.NativeOpenMessagesPage(context.Background(), "codex:"+targetID, cursor, 2); err == nil {
		t.Fatal("cross-lease cursor accepted")
	}
}

func TestNativeLargeToolTurnHasContinuationInsteadOfSilentlyLosingItems(t *testing.T) {
	f := gatewayFixture(t)
	items := []any{}
	for i := 0; i < 650; i++ {
		items = append(items, map[string]any{"id": fmt.Sprint("t", i), "type": "commandExecution", "command": "fixture"})
	}
	f.result = func(map[string]any) any {
		return nativeEnvelope(map[string]any{"schemaVersion": 1, "thread": map[string]any{"id": targetID, "kind": "codex", "hostId": "local"}, "turns": []any{map[string]any{"id": "turn", "items": items}}})
	}
	_, first, cursor, err := f.service.NativeOpenMessagesPage(context.Background(), "codex:"+targetID, "", 2)
	if err != nil || len(first) != 400 || cursor == "" {
		t.Fatal(len(first), err)
	}
	_, older, next, err := f.service.NativeOpenMessagesPage(context.Background(), "codex:"+targetID, cursor, 10)
	if err != nil || len(older) != 251 || next != "" {
		t.Fatal(len(older), err)
	}
}
