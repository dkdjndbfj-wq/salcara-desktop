package desktopcompanion

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func imageHistoryItem(t *testing.T, values map[string]any) map[string]json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	var item map[string]json.RawMessage
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Fatal(err)
	}
	return item
}

func TestNativeImageHistoryKeepsOperationOrderIdentityAndReportedState(t *testing.T) {
	fixture := gatewayFixture(t)
	path := filepath.Join(t.TempDir(), "actual-native-output.png")
	input := []map[string]any{
		{"id": "comment", "type": "agentMessage", "text": "Checking the image", "phase": "commentary"},
		{"id": "view-schema", "type": "imageView", "path": path},
		{"id": "view-started", "type": "imageView", "status": "inProgress", "path": path},
		{"id": "view-done", "type": "imageView", "status": "completed", "path": path},
		{"id": "view-failed", "type": "imageView", "status": "failed", "path": path},
		{"id": "generation-running", "type": "imageGeneration", "status": "inProgress", "savedPath": path},
		{"id": "generation-done", "type": "imageGeneration", "status": "completed", "savedPath": path},
		{"id": "generation-failed", "type": "imageGeneration", "status": "failed", "result": "image provider rejected the request", "savedPath": path},
		{"id": "answer", "type": "agentMessage", "text": "Done", "phase": "final_answer"},
	}
	items := make([]map[string]json.RawMessage, 0, len(input))
	for _, value := range input {
		items = append(items, imageHistoryItem(t, value))
	}
	fixture.result = func(command map[string]any) any {
		if command["type"] != "read" || command["sessionKey"] != "codex:"+targetID {
			t.Error("image history requested an unrelated operation", command)
		}
		return nativeEnvelope(map[string]any{
			"schemaVersion": 1, "thread": map[string]any{"id": targetID, "kind": "codex", "hostId": "local"},
			"turns": []any{map[string]any{"id": "image-turn", "status": "completed", "items": items}},
		})
	}
	_, events, next, err := fixture.service.NativeOpenPage(context.Background(), "codex:"+targetID, "")
	if err != nil || next != "" || len(events) != len(input)+1 {
		t.Fatalf("image history failed: %v next=%q events=%+v", err, next, events)
	}
	wantStates := []string{"", "", "running", "done", "failed", "running", "done", "failed", ""}
	for i, event := range events[:len(input)] {
		if event.ID != input[i]["id"] || event.TurnID != "image-turn" || event.SessionKey != "codex:"+targetID || event.Status != wantStates[i] {
			t.Fatalf("image operation order, identity or state changed: %+v", event)
		}
		if (event.Type == "notice" && i != 1) || len(event.Attachments) != 0 {
			t.Fatalf("known image became unsupported or invented an attachment: %+v", event)
		}
		if i > 1 && i < 8 {
			kind, title := "image_view", "查看图片"
			if i >= 5 {
				kind, title = "image_generation", "生成图片"
			}
			if event.Type != "tool" || event.Kind != kind || event.Title != title {
				t.Fatalf("image kind was lost: %+v", event)
			}
		}
	}
	if events[1].Type != "notice" || events[1].Level != "info" || events[1].Text != "图片查看记录 · 电脑未提供执行状态" || events[1].Detail != "" {
		t.Fatalf("status-less native image is not a protocol-compatible neutral notice: %+v", events[1])
	}
	if events[2].Detail != path || events[3].Detail != path || events[6].Detail != path || events[4].Detail != "" || events[5].Detail != "" || events[7].Detail != "" || events[7].Output != "image provider rejected the request" {
		t.Fatalf("image media or failure payload was inferred: %+v", events)
	}
	if events[0].MessagePhase != "commentary" || events[8].MessagePhase != "final_answer" {
		t.Fatal("image operation merged separate assistant phases")
	}
	_, messages, cursor, err := fixture.service.NativeOpenMessagesPage(context.Background(), "codex:"+targetID, "", 10)
	if err != nil || cursor != "" || !reflect.DeepEqual(messages, events) {
		t.Fatalf("message-limited image history differs: %v", err)
	}
}

func TestNativeImageHistorySerializedToolsAlwaysHaveSupportedStatus(t *testing.T) {
	var items []map[string]json.RawMessage
	for _, kind := range []string{"imageView", "imageGeneration"} {
		for _, status := range []any{nil, "", "unknown", 42, "inProgress", "running", "completed", "done", "success", "failed", "error", "declined"} {
			items = append(items, imageHistoryItem(t, map[string]any{"type": kind, "id": "image", "status": status}))
		}
		items = append(items, imageHistoryItem(t, map[string]any{"type": kind, "id": "image"}))
	}
	events := nativeEvents("codex:fixture", nativeRead{Turns: []nativeTurn{{ID: "turn", Items: items}}})
	encoded, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	var wire []map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire) != len(items) {
		t.Fatalf("unknown image records disappeared: got %d want %d", len(wire), len(items))
	}
	for i, event := range wire {
		if event["id"] != "image" || event["turnId"] != "turn" || event["sessionKey"] != "codex:fixture" {
			t.Fatalf("serialized image lost native identity: %+v", event)
		}
		status := rawString(items[i], "status")
		known := status == "inProgress" || status == "running" || status == "completed" || status == "done" || status == "success" || status == "failed" || status == "error" || status == "declined"
		if known {
			wireStatus, ok := event["status"].(string)
			if event["type"] != "tool" || !ok || (wireStatus != "running" && wireStatus != "done" && wireStatus != "failed" && wireStatus != "interrupted") {
				t.Fatalf("tool would be rejected by mobile event parser: %+v", event)
			}
		} else if event["type"] != "notice" || event["status"] != nil || event["text"] == "" || event["level"] == nil {
			t.Fatalf("unknown image emitted invalid tool status: %+v", event)
		}
	}
}

func TestNativeImageHistoryDoesNotGuessMissingOrFutureGenerationState(t *testing.T) {
	for _, kind := range []string{"imageView", "imageGeneration"} {
		for _, status := range []any{nil, "", "future-state", 123} {
			item := imageHistoryItem(t, map[string]any{"id": "image", "type": kind, "status": status, "path": "private-native-path", "savedPath": "private-native-path", "result": "private-native-result"})
			events := nativeEvents("codex:fixture", nativeRead{Turns: []nativeTurn{{ID: "turn", Status: "completed", Items: []map[string]json.RawMessage{item}}}})
			encoded, _ := json.Marshal(events)
			if len(events) != 2 || events[0].Type != "notice" || events[0].Status != "" || strings.Contains(string(encoded), "private-native") {
				t.Fatalf("unknown image state became success or exposed payload: %+v", events)
			}
			if kind == "imageView" && (events[0].Level != "info" || events[0].Text != "图片查看记录 · 电脑未提供执行状态") || kind == "imageGeneration" && events[0].ErrorCode != "unsupported_item" {
				t.Fatalf("unknown image did not use its neutral fallback: %+v", events[0])
			}
		}
	}
	item := imageHistoryItem(t, map[string]any{"id": "image", "type": "imageGeneration", "result": "private-native-result"})
	events := nativeEvents("codex:fixture", nativeRead{Turns: []nativeTurn{{ID: "turn", Status: "completed", Items: []map[string]json.RawMessage{item}}}})
	if events[0].Type != "notice" || events[0].Status != "" {
		t.Fatal("parent completion supplied missing image generation state")
	}
}

func TestNativeImageHistoryOnlyPassesActualOutputToLocalMediaImporter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "native-image.webp")
	data := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAAB"
	for _, tc := range []struct {
		name, saved, result, wantPath, wantData string
	}{
		{"saved-path", path, "", path, ""},
		{"legacy-result-path", "", path, path, ""},
		{"generated-base64", "", data, "", data},
		{"generated-data-url", "", "data:image/png;base64," + data, "", "data:image/png;base64," + data},
		{"prefer-saved-output", path, data, path, ""},
		{"no-output", "", "", "", ""},
		{"remote-url", "", "https://example.test/image.png", "", ""},
		{"relative-path", "relative.png", "relative.png", "", ""},
		{"ordinary-result", "", "provider description", "", ""},
		{"non-image-path", path + ".txt", "", "", ""},
		{"malformed-path", path + "\n", "", "", ""},
		{"oversize-result", "", data + strings.Repeat("A", 12<<20), "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := imageHistoryItem(t, map[string]any{"id": "image", "type": "imageGeneration", "status": "completed", "savedPath": tc.saved, "result": tc.result})
			events := nativeEvents("codex:fixture", nativeRead{Turns: []nativeTurn{{ID: "turn", Items: []map[string]json.RawMessage{item}}}})
			if len(events) != 1 || events[0].Detail != tc.wantPath || len(events[0].Attachments) != 0 {
				t.Fatalf("image output fabricated: %+v", events)
			}
			wantData := []string(nil)
			if tc.wantData != "" {
				wantData = []string{tc.wantData}
			}
			if !reflect.DeepEqual(events[0].ImageData, wantData) {
				t.Fatal("media handoff did not preserve only actual bounded image data")
			}
			encoded, _ := json.Marshal(events)
			if strings.Contains(string(encoded), data) {
				t.Fatal("private base64 payload crossed the serialization boundary")
			}
		})
	}
}
