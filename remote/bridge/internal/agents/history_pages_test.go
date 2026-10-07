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

func TestCodexPageDoesNotAppendEveryPreviouslyLoadedThread(t *testing.T) {
	f := newFixture(t, "ask")
	a := f.m.Get("codex").(*codexAgent)
	a.mu.Lock()
	for i := 0; i < 40; i++ {
		id := fmt.Sprintf("cached-%d", i)
		a.threads[id] = &codexThread{id: id, loaded: true, info: protocol.SessionInfo{SessionKey: "codex:" + id, Tool: "codex"}}
	}
	a.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	page, _, err := a.SessionsPage(ctx, "", 2, "")
	if err != nil || len(page) != 2 {
		t.Fatal("a bounded page was expanded by remembered threads", len(page), err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.threads["cached-0"] == nil {
		t.Fatal("paged read removed a live thread from memory")
	}
}

func TestMessageHistoryCountsMessagesAndRetainsTheirToolsAndStatuses(t *testing.T) {
	all := []protocol.Event{}
	for i := 0; i < 12; i++ {
		all = append(all, protocol.Event{Type: "message", ID: fmt.Sprintf("m%d", i), Role: "assistant", Text: "fixture"},
			protocol.Event{Type: "tool", ID: fmt.Sprintf("tool%d", i)}, protocol.Event{Type: "turn", ID: fmt.Sprintf("turn%d", i), Status: "completed"})
	}
	page, cursor, err := paginateMessageHistory("codex:original", all, "", 2)
	if err != nil || len(page) != 6 || page[0].ID != "m10" || page[5].ID != "turn11" || cursor == "" {
		t.Fatal("first page counted raw events instead of messages")
	}
	all = append(all, protocol.Event{Type: "message", ID: "appended", Role: "assistant"})
	older, next, err := paginateMessageHistory("codex:original", all, cursor, 10)
	if err != nil || len(older) != 30 || older[0].ID != "m0" || older[29].ID != "turn9" || next != "" {
		t.Fatal("message history cursor skipped an older message")
	}
	if _, _, err := paginateMessageHistory("codex:other", all, cursor, 10); err == nil {
		t.Fatal("cross-session cursor accepted")
	}
}

func TestMessageHistoryStillHasEventAndByteBudgets(t *testing.T) {
	all := []protocol.Event{{Type: "message", ID: "prompt", Role: "user"}}
	for i := 0; i < 1000; i++ {
		all = append(all, protocol.Event{Type: "tool", ID: fmt.Sprint(i), Text: "fixture"})
	}
	all = append(all, protocol.Event{Type: "message", ID: "reply", Role: "assistant"})
	page, cursor, err := paginateMessageHistory("codex:original", all, "", 2)
	if err != nil || len(page) > maxHistoryEvents || cursor == "" {
		t.Fatal("message limit disabled event safety cap")
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

func TestClaudeDirectoryReadsOnlyOnePageOfContentSummaries(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "projects", "fixture")
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 35; i++ {
		id := fmt.Sprintf("fixture-%03d", i)
		raw, _ := json.Marshal(map[string]any{"type": "user", "sessionId": id, "cwd": root, "entrypoint": "cli", "message": map[string]any{"role": "user", "content": "fixture"}})
		path := filepath.Join(project, id+".jsonl")
		if err := os.WriteFile(path, append(raw, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
		when := time.Unix(1700000000-int64(i), 0)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	a := newClaudeAgent(func(protocol.Event) {}, func() Settings { return Settings{} }, "not-launched", root)
	defer a.Close()
	page, cursor, err := a.SessionsPage(context.Background(), "", 10, "code")
	if err != nil || len(page) != 10 || cursor == "" || len(a.hist.cache) != 11 || a.hist.cache[filepath.Join(project, "fixture-034.jsonl")] != nil {
		t.Fatal("the first page scanned older unrequested content", len(page), len(a.hist.cache), err)
	}
	page, _, err = a.SessionsPage(context.Background(), cursor, 10, "code")
	if err != nil || len(page) != 10 || page[0].SessionKey != "claude:fixture-010" || len(a.hist.cache) != 21 {
		t.Fatal("next page rescanned or skipped its boundary", len(page), len(a.hist.cache), err)
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
