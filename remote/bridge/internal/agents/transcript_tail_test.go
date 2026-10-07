package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"salcara/bridge/internal/protocol"
)

func tailFixture(t *testing.T, count int, oldBytes int64) (*os.File, protocol.SessionInfo) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(t.TempDir(), "history.jsonl"), os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if oldBytes > 0 {
		if _, err = f.WriteAt([]byte("\n"), oldBytes-1); err != nil {
			t.Fatal(err)
		}
	}
	var tail bytes.Buffer
	for i := 0; i < count; i++ {
		for _, role := range []string{"user", "assistant"} {
			line := map[string]any{"type": role, "sessionId": "original", "uuid": fmt.Sprintf("%s-%d", role, i), "timestamp": fmt.Sprintf("2026-10-07T01:%02d:00Z", i), "message": map[string]any{"id": fmt.Sprintf("%s-%d", role, i), "role": role, "content": fmt.Sprintf("%s %d", role, i)}}
			raw, _ := json.Marshal(line)
			tail.Write(raw)
			tail.WriteByte('\n')
		}
	}
	if _, err = f.WriteAt(tail.Bytes(), oldBytes); err != nil {
		t.Fatal(err)
	}
	return f, protocol.SessionInfo{SessionKey: "claude:original", Tool: "claude", Cwd: "fixture"}
}

func TestTranscriptTailCountsRealMessagesAndWalksWithoutLoss(t *testing.T) {
	f, info := tailFixture(t, 17, 0)
	st, _ := f.Stat()
	page, cursor, err := readClaudeTail(context.Background(), f, st, info.SessionKey, info, "", 2, "original", false)
	if err != nil || len(page) != 2 || page[0].Text != "user 16" || page[1].Text != "assistant 16" || cursor == "" {
		t.Fatal("wrong initial tail", page, err)
	}
	seen := map[string]bool{}
	for _, event := range page {
		seen[event.ID] = true
	}
	// Later appends must not move the existing older-history byte boundary.
	if _, err := f.WriteAt([]byte("{\"type\":\"user\",\"uuid\":\"new\",\"message\":{\"content\":\"new\"}}\n"), st.Size()); err != nil {
		t.Fatal(err)
	}
	for cursor != "" {
		st, _ = f.Stat()
		page, cursor, err = readClaudeTail(context.Background(), f, st, info.SessionKey, info, cursor, 10, "original", false)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range page {
			if seen[event.ID] {
				t.Fatal("duplicate", event.ID)
			}
			seen[event.ID] = true
		}
	}
	if len(seen) != 34 {
		t.Fatalf("history omitted messages: %d", len(seen))
	}
}

func TestTranscriptTailDoesNotLoadOrRejectAnEntireHugeOldFile(t *testing.T) {
	f, info := tailFixture(t, 15, 80<<20)
	st, _ := f.Stat()
	page, cursor, err := readClaudeTail(context.Background(), f, st, info.SessionKey, info, "", 2, "original", false)
	if err != nil || len(page) != 2 {
		t.Fatal("latest messages of a huge file failed", err)
	}
	var position transcriptTailCursor
	if readCursor(cursor, &position) != nil || position.Start < (79<<20) {
		t.Fatal("tail read included the huge older prefix")
	}
}

func TestTranscriptTailRejectsRotationScopeAndNamespaceChanges(t *testing.T) {
	f, info := tailFixture(t, 8, 0)
	st, _ := f.Stat()
	_, cursor, err := readClaudeTail(context.Background(), f, st, info.SessionKey, info, "", 2, "original", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := readClaudeTail(context.Background(), f, st, "another-scope", info, cursor, 10, "original", true); err == nil {
		t.Fatal("cross-scope cursor accepted")
	}
	if _, _, err := readClaudeTail(context.Background(), f, st, info.SessionKey, info, "", 2, "other-session", true); err == nil {
		t.Fatal("mixed transcript namespace accepted")
	}
	if _, err := f.WriteAt([]byte("changed"), 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readClaudeTail(context.Background(), f, st, info.SessionKey, info, cursor, 10, "original", true); err == nil {
		t.Fatal("rotated file accepted")
	}
}

func TestChildMessagesDoNotConsumeMainConversationMessageBudget(t *testing.T) {
	all := []protocol.Event{{Type: "message", ID: "user", Role: "user"}, {Type: "message", ID: "assistant", Role: "assistant"}, {Type: "message", ID: "child", Role: "assistant", ParentID: "task"}}
	page, next, err := paginateMessageHistory("parent", all, "", 2)
	if err != nil || next != "" || len(page) != 3 {
		t.Fatal("child consumed a main message slot")
	}
}

func TestTranscriptTailMappingBudgetFailsExplicitlyWithoutPartialSuccess(t *testing.T) {
	f, info := tailFixture(t, 1, 0)
	st, _ := f.Stat()
	var data bytes.Buffer
	for i := 0; i <= maxTailEvents; i++ {
		raw, _ := json.Marshal(map[string]any{"type": "assistant", "uuid": fmt.Sprint("bulk", i), "message": map[string]any{"id": fmt.Sprint("bulk", i), "role": "assistant", "content": "fixture"}})
		data.Write(raw)
		data.WriteByte('\n')
	}
	if _, err := f.WriteAt(data.Bytes(), st.Size()); err != nil {
		t.Fatal(err)
	}
	st, _ = f.Stat()
	page, next, err := readClaudeTail(context.Background(), f, st, info.SessionKey, info, "", 2, "original", false)
	if err == nil || page != nil || next != "" {
		t.Fatal("oversized mapped tail was silently truncated")
	}
}
