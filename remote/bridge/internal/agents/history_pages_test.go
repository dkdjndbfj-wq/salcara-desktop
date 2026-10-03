package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"salcara/bridge/internal/protocol"
	"strings"
	"testing"
	"time"
)

func TestHistoryPagesWalkAllEventsAndNewAppendDoesNotShiftCursor(t *testing.T) {
	all := []protocol.Event{}
	for i := 0; i < 1050; i++ {
		all = append(all, protocol.Event{SessionKey: "codex:original", Type: "message", ID: fmt.Sprint(i), Role: "assistant", Text: "fixture", TS: int64(i)})
	}
	page, cursor, err := paginateHistory("codex:original", all, "", 400)
	if err != nil || len(page) != 400 || cursor == "" {
		t.Fatal(len(page), cursor, err)
	}
	seen := map[string]bool{}
	for _, event := range page {
		seen[event.ID] = true
	}
	all = append(all, protocol.Event{Type: "message", ID: "new", TS: 1051})
	for cursor != "" {
		page, next, err := paginateHistory("codex:original", all, cursor, 400)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range page {
			if seen[event.ID] {
				t.Fatal("duplicate", event.ID)
			}
			seen[event.ID] = true
		}
		cursor = next
	}
	if len(seen) != 1050 || !seen["0"] {
		t.Fatal("older history omitted", len(seen))
	}
}
func TestHistoryPageIdentityStaleAndByteBudget(t *testing.T) {
	all := []protocol.Event{}
	for i := 0; i < 30; i++ {
		all = append(all, protocol.Event{Type: "message", ID: fmt.Sprint(i), Text: strings.Repeat("x", 60000)})
	}
	page, cursor, err := paginateHistory("codex:a", all, "", 400)
	if err != nil || len(page) >= 30 || cursor == "" {
		t.Fatal(len(page), cursor, err)
	}
	encoded, _ := json.Marshal(page)
	if len(encoded) > historyPageBytes {
		t.Fatal("byte budget", len(encoded))
	}
	if _, _, err := paginateHistory("codex:b", all, cursor, 400); err == nil {
		t.Fatal("cross thread cursor accepted")
	}
	if _, _, err := paginateHistory("codex:a", all[:2], cursor, 400); err == nil {
		t.Fatal("compacted boundary accepted")
	}
}
func TestClaudeDirectoryPagesBeyondOneHundredAndSeparateClients(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "projects", "project")
	os.MkdirAll(project, 0700)
	for i := 0; i < 205; i++ {
		id := fmt.Sprintf("fixture-%03d", i)
		entry := "cli"
		if i%2 == 0 {
			entry = "claude-desktop"
		}
		line := map[string]any{"type": "user", "sessionId": id, "cwd": root, "entrypoint": entry, "timestamp": "2026-10-03T00:00:00Z", "message": map[string]any{"role": "user", "content": "fixture"}}
		raw, _ := json.Marshal(line)
		if err := os.WriteFile(filepath.Join(project, id+".jsonl"), append(raw, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
	a := newClaudeAgent(func(protocol.Event) {}, func() Settings { return Settings{} }, "not-launched", root)
	defer a.Close()
	for _, client := range []string{"code", "desktop-code"} {
		cursor := ""
		seen := map[string]bool{}
		for {
			page, next, err := a.SessionsPage(context.Background(), cursor, 40, client)
			if err != nil {
				t.Fatal(err)
			}
			if len(page) > 40 {
				t.Fatal("page too large")
			}
			for _, info := range page {
				if seen[info.SessionKey] {
					t.Fatal("duplicate")
				}
				seen[info.SessionKey] = true
				if (info.Client == "Claude Desktop") != (client == "desktop-code") {
					t.Fatal("mixed client")
				}
			}
			if next == "" {
				break
			}
			cursor = next
		}
		want := 102
		if client == "desktop-code" {
			want = 103
		}
		if len(seen) != want {
			t.Fatal(client, len(seen), want)
		}
	}
}
func TestHistoryCursorStableWhenEarlierChildEventArrives(t *testing.T) {
	all := []protocol.Event{}
	for i := 0; i < 500; i++ {
		all = append(all, protocol.Event{Type: "message", ID: fmt.Sprint(i), TS: int64(i)})
	}
	_, cursor, err := paginateHistory("codex:original", all, "", 400)
	if err != nil {
		t.Fatal(err)
	}
	all = append([]protocol.Event{{Type: "message", ID: "late-child", ParentID: "real-task", TS: 1}}, all...)
	page, _, err := paginateHistory("codex:original", all, cursor, 400)
	if err != nil || len(page) != 101 {
		t.Fatal(len(page), err)
	}
}
func TestClaudeDirectoryUsesCanonicalNewestTranscriptBeforeClientFilter(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a-old", "z-new"} {
		os.MkdirAll(filepath.Join(root, "projects", name), 0700)
	}
	id := "fixture-same"
	write := func(folder, entry, title string, when time.Time) {
		raw, _ := json.Marshal(map[string]any{"type": "user", "sessionId": id, "cwd": root, "entrypoint": entry, "message": map[string]any{"role": "user", "content": title}})
		path := filepath.Join(root, "projects", folder, id+".jsonl")
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(path, when, when)
	}
	write("a-old", "cli", "old", time.Now().Add(-time.Hour))
	write("z-new", "claude-desktop", "new", time.Now())
	a := newClaudeAgent(func(protocol.Event) {}, func() Settings { return Settings{} }, "not-launched", root)
	defer a.Close()
	page, _, err := a.SessionsPage(context.Background(), "", 100, "code")
	if err != nil || len(page) != 0 {
		t.Fatal("old copy leaked to Code", page, err)
	}
	page, _, err = a.SessionsPage(context.Background(), "", 100, "desktop-code")
	if err != nil || len(page) != 1 || page[0].Title != "new" {
		t.Fatal(page, err)
	}
}
