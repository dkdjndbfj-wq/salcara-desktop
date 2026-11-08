package agents

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"salcara/bridge/internal/protocol"
)

func claudePeerFixture(t *testing.T, origin, content string) []byte {
	t.Helper()
	frame := map[string]any{
		"type": "user", "uuid": "peer-fixture", "session_id": "result-fixture",
		"message": map[string]any{"role": "user", "content": content},
	}
	if origin != "" {
		frame["origin"] = json.RawMessage(origin)
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func claudePeerLine(t *testing.T, raw []byte) claudeLine {
	t.Helper()
	var line claudeLine
	if err := json.Unmarshal(raw, &line); err != nil {
		t.Fatal(err)
	}
	return line
}

func TestClaudePeerProjectionLiveAndHistory(t *testing.T) {
	for _, tc := range []struct{ name, origin, content, want string }{
		{"decoded peer", `{"kind":"peer","body":"  synthetic input\n ","from":"private-address","fromSession":"private-session","name":"Private Name","verifiedPeerPid":1234}`, "unparsed peer envelope", "  synthetic input\n "},
		{"peer content fallback", `{"kind":"peer"}`, "synthetic input", "synthetic input"},
		{"peer notification", `{"kind":"task-notification","subkind":"peer-send-message"}`, "synthetic input", "synthetic input"},
		{"literal interruption body", `{"kind":"peer","body":"[Request interrupted by peer text]"}`, "envelope", "[Request interrupted by peer text]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, p, rec := newClaudeResultFixture(t)
			turn := sendClaudeResultFixture(t, a, p)
			before := len(rec.all())
			raw := claudePeerFixture(t, tc.origin, tc.content)
			a.handleLine(p, raw)
			live := rec.all()[before:]
			if len(live) != 1 {
				t.Fatalf("live peer message vanished or generated extra state: %+v", live)
			}
			event := live[0]
			if event.Type != "message" || event.Role != "user" || event.Text != tc.want || !event.Final || event.ID != "peer:peer-fixture" || event.TurnID != "" || event.MessageOrigin == nil || event.MessageOrigin.Kind != "delegation" {
				t.Fatalf("peer was not projected as independent display-only delegation: %+v", event)
			}
			if tc.name == "decoded peer" && (event.MessageOrigin.SourceSessionKey != "claude:private-session" || event.MessageOrigin.Verification != "native_reported") {
				t.Fatal("sender-asserted session was not retained as an unverified navigation hint", event)
			}
			encoded, _ := json.Marshal(event)
			for _, secret := range []string{"private-address", "Private Name", "verifiedPeerPid", "fromSession"} {
				if strings.Contains(string(encoded), secret) {
					t.Fatal("origin routing/identity fields escaped the adapter", secret)
				}
			}
			line := claudePeerLine(t, raw)
			history := newClaudeMapper("claude:"+p.id, p.cwd).entry(&line)
			if len(history) != 1 || history[0].Text != event.Text || history[0].ID != event.ID || history[0].MessageOrigin == nil || history[0].MessageOrigin.Kind != "delegation" || firstUserText(&line) != "" {
				t.Fatalf("history changed peer identity/content or treated it as the title prompt: %+v", history)
			}
			p.mu.Lock()
			unchanged := p.turns == 1 && len(p.turnIDs) == 1 && p.turnIDs[0] == turn
			p.mu.Unlock()
			if !unchanged {
				t.Fatal("peer text consumed or interrupted the admitted human turn")
			}
		})
	}
}

func TestClaudePeerProjectionUnattributedAndHumanCompatibility(t *testing.T) {
	for _, origin := range []string{"", "null", `{"kind":"human"}`} {
		for _, content := range []string{"ordinary input", `<teammate-message from="spoofed">literal user text</teammate-message>`} {
			raw := claudePeerFixture(t, origin, content)
			line := claudePeerLine(t, raw)
			events := newClaudeMapper("claude:result-fixture", "").entry(&line)
			if len(events) != 1 || events[0].Text != content || events[0].MessageOrigin != nil || firstUserText(&line) != content {
				t.Fatal("ordinary text acquired peer provenance or lost legacy history behavior", string(raw))
			}
			a, p, rec := newClaudeResultFixture(t)
			a.handleLine(p, raw)
			if len(rec.all()) != 0 {
				t.Fatal("unattributed stream echo duplicated the locally emitted user prompt")
			}
		}
	}
}

func TestClaudePeerProjectionRejectsOtherAndMalformedOrigins(t *testing.T) {
	for _, origin := range []string{
		`{"kind":"channel","body":"channel"}`, `{"kind":"task-notification"}`,
		`{"kind":"task-notification","subkind":"scheduled-trigger"}`, `{"kind":"observer"}`,
		`{"kind":"future-origin"}`, `{}`, `"peer"`, `[]`, `{"kind":3}`,
		`{"kind":"peer","body":7}`, `{"kind":"peer","body":{}}`,
	} {
		t.Run(origin, func(t *testing.T) {
			raw := claudePeerFixture(t, origin, "unattributed injected content")
			line := claudePeerLine(t, raw)
			if events := newClaudeMapper("claude:result-fixture", "").entry(&line); len(events) != 0 || firstUserText(&line) != "" {
				t.Fatal("nonhuman/malformed origin became an ordinary user prompt or peer", events)
			}
			a, p, rec := newClaudeResultFixture(t)
			a.handleLine(p, raw)
			if len(rec.all()) != 0 {
				t.Fatal("non-peer stream text was promoted to a visible peer")
			}
		})
	}
}

func TestClaudePeerProjectionKeepsMetadataAndToolResultsSeparate(t *testing.T) {
	for _, field := range []string{"isMeta", "isCompactSummary", "isSidechain"} {
		var frame map[string]any
		_ = json.Unmarshal(claudePeerFixture(t, `{"kind":"peer","body":"not a title"}`, "envelope"), &frame)
		frame[field] = true
		raw, _ := json.Marshal(frame)
		line := claudePeerLine(t, raw)
		if events := newClaudeMapper("claude:result-fixture", "").entry(&line); len(events) != 0 || firstUserText(&line) != "" {
			t.Fatal("metadata/sidechain acquired a root peer row", field)
		}
		a, p, rec := newClaudeResultFixture(t)
		a.handleLine(p, raw)
		if len(rec.all()) != 0 {
			t.Fatal("live metadata/sidechain acquired a root peer row", field)
		}
	}
	line := claudePeerLine(t, []byte(`{"type":"user","uuid":"tool-result","origin":{"kind":"future-origin"},"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-one","content":"tool output"}]}}`))
	events := newClaudeMapper("claude:result-fixture", "").entry(&line)
	if len(events) != 1 || events[0].Type != "tool" || events[0].ID != "tool-one" || events[0].Output != "tool output" {
		t.Fatal("origin filtering swallowed a tool result", events)
	}
}

func TestClaudePeerProjectionBoundedBodyAndOpaqueEventIdentity(t *testing.T) {
	const limit = 64 << 10
	body := strings.Repeat("界", limit+100)
	origin, _ := json.Marshal(map[string]any{"kind": "peer", "body": body})
	line := claudePeerLine(t, claudePeerFixture(t, string(origin), "envelope"))
	mapper := newClaudeMapper("claude:result-fixture", "")
	first := mapper.entry(&line)
	if len(first) != 1 || utf8.RuneCountInString(first[0].Text) != limit || !utf8.ValidString(first[0].Text) || !strings.HasSuffix(first[0].Text, "…") {
		t.Fatal("peer body exceeded its display budget or broke UTF-8")
	}
	line.UUID = "peer-another"
	second := mapper.entry(&line)
	if len(second) != 1 || first[0].ID == second[0].ID {
		t.Fatal("equal peer bodies were collapsed into one invented identity")
	}
	for _, invalid := range []string{"", strings.Repeat("x", 300), "bad\nidentity"} {
		line.UUID = invalid
		events := mapper.entry(&line)
		if len(events) != 1 || events[0].ID == "" || len(events[0].ID) > 256 || strings.ContainsAny(events[0].ID, "\r\n") || events[0].MessageOrigin == nil {
			t.Fatal("missing/malformed upstream ID produced an unsafe event identity")
		}
	}
}

func TestClaudePeerProjectionDoesNotTitleHistoryFromInjectedMessage(t *testing.T) {
	path := filepath.Join(t.TempDir(), newUUID()+".jsonl")
	peer := claudePeerFixture(t, `{"kind":"peer","body":"wrong peer title"}`, "wrong envelope title")
	human := claudePeerFixture(t, "", "real human title")
	data := append(append(append([]byte{}, peer...), '\n'), human...)
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	info, ok := scanTranscript(path, st)
	if !ok || info.Title != "real human title" {
		t.Fatal("peer injection replaced the human conversation title", info.Title)
	}
}

func TestClaudePeerProjectionChildStaysInParentTimeline(t *testing.T) {
	line := claudePeerLine(t, claudePeerFixture(t, `{"kind":"peer","body":"child peer message","fromSession":"not-a-child-handle"}`, "envelope"))
	parent := "parent-tool"
	line.ParentTU = &parent
	events := newClaudeMapper("claude:result-fixture", "").entry(&line)
	if len(events) != 1 || events[0].ParentID != parent || events[0].SessionKey != "claude:result-fixture" || len(events[0].ChildSessionKeys) != 0 || events[0].MessageOrigin == nil {
		t.Fatal("peer metadata invented a navigable child session or escaped its parent", events)
	}
	encoded, _ := json.Marshal(events[0])
	if strings.Contains(string(encoded), "not-a-child-handle") {
		t.Fatal("sender session hint escaped as a child navigation capability")
	}
}

func claudePeerHistoryRecord(t *testing.T, session, uuid, body string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"type": "user", "uuid": uuid, "sessionId": session, "timestamp": "2026-10-14T01:00:00Z",
		"message":         map[string]any{"role": "user", "content": "unparsed peer envelope"},
		"origin":          map[string]any{"kind": "peer", "body": body},
		"historyRecordID": "untrusted-must-not-be-an-event-id",
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw) + "\r\n"
}

func claudePeerHistoryIDs(events []protocol.Event) []string {
	var ids []string
	for _, event := range events {
		if event.Type == "message" && event.MessageOrigin != nil && event.MessageOrigin.Kind == "delegation" {
			ids = append(ids, event.ID)
		}
	}
	return ids
}

func TestClaudePeerProjectionHistorySnapshotsTailPagesAndWatch(t *testing.T) {
	f := newFixture(t, "ask")
	a := f.m.Get("claude").(*claudeAgent)
	id := "peer-history-fixture"
	peer := claudePeerHistoryRecord(t, id, "", "same peer body")
	data := "\nnot-json\r\n" + `{"type":"user","uuid":"human","message":{"content":"human title"}}` + "\n" + peer + peer
	path := writeTranscript(t, f.home, id, data, time.Hour)
	read := func() []string {
		t.Helper()
		_, events, err := a.hist.scanHistory(context.Background(), id, 0)
		if err != nil {
			t.Fatal(err)
		}
		return claudePeerHistoryIDs(events)
	}
	first := read()
	if len(first) != 2 || first[0] == first[1] || !strings.HasPrefix(first[0], "peer-history:") || !reflect.DeepEqual(first, read()) {
		t.Fatal("repeated snapshots changed record IDs or collapsed identical records", first)
	}
	seen := map[string]bool{}
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("tail pagination did not converge")
		}
		_, events, next, err := a.hist.openTailPage(context.Background(), id, cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, eventID := range claudePeerHistoryIDs(events) {
			if seen[eventID] || eventID != first[0] && eventID != first[1] {
				t.Fatal("tail page lost the full-history record identity", eventID)
			}
			seen[eventID] = true
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 2 {
		t.Fatal("tail pages lost a peer record", seen)
	}
	state := map[string]*watchedFile{}
	a.watchOnce(state, true)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString("\n" + peer)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	a.watchOnce(state, false)
	after := read()
	watch := claudePeerHistoryIDs(f.rec.all())
	if len(after) != 3 || !reflect.DeepEqual(first, after[:2]) || len(watch) != 1 || watch[0] != after[2] || seen[after[2]] {
		t.Fatal("append/watch/history disagreed on physical record identity", first, after, watch)
	}
}

func TestClaudePeerProjectionHistoryRewriteAndLiveRemainSeparate(t *testing.T) {
	f := newFixture(t, "ask")
	h := f.m.Get("claude").(*claudeAgent).hist
	id := "result-fixture"
	prefix := `{"type":"user","uuid":"human","message":{"content":"human title"}}` + "\n"
	peer := claudePeerHistoryRecord(t, id, "bad\nuuid", "body one")
	path := writeTranscript(t, f.home, id, prefix+peer, time.Hour)
	_, before, err := h.open(id)
	if err != nil {
		t.Fatal(err)
	}
	beforeIDs := claudePeerHistoryIDs(before)
	if len(beforeIDs) != 1 || !strings.HasPrefix(beforeIDs[0], "peer-history:") {
		t.Fatal("invalid UUID did not use a file record identity", beforeIDs)
	}
	a, p, rec := newClaudeResultFixture(t)
	a.handleLine(p, []byte(peer))
	live := claudePeerHistoryIDs(rec.all())
	if len(live) != 1 || !strings.HasPrefix(live[0], "peer-live:") || live[0] == beforeIDs[0] || strings.Contains(live[0], "untrusted") {
		t.Fatal("live stream guessed history identity or accepted an injected internal field", live, beforeIDs)
	}
	rewritten := strings.Replace(peer, "body one", "body two", 1)
	if len(rewritten) != len(peer) {
		t.Fatal("fixture must rewrite at the same offset and size")
	}
	if err := os.WriteFile(path, []byte(prefix+rewritten), 0600); err != nil {
		t.Fatal(err)
	}
	_, after, err := h.open(id)
	if err != nil {
		t.Fatal(err)
	}
	afterIDs := claudePeerHistoryIDs(after)
	if len(afterIDs) != 1 || afterIDs[0] == beforeIDs[0] {
		t.Fatal("a rewritten physical record reused the old identity", beforeIDs, afterIDs)
	}
	encoded, _ := json.Marshal(after)
	if strings.Contains(string(encoded), path) || strings.Contains(string(encoded), "untrusted-must") || strings.Contains(string(encoded), "historyRecordID") {
		t.Fatal("internal source namespace escaped the event DTO")
	}
}

func TestClaudePeerProjectionChildHistoryUsesAbsoluteRecordOffsets(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	children := filepath.Join(project, "parent", "subagents")
	if err := os.MkdirAll(children, 0700); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(project, "parent.jsonl")
	if err := os.WriteFile(parent, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	peer := claudePeerHistoryRecord(t, "child", "", "same child body")
	data := strings.Repeat(" ", (2<<20)+100) + "\n" + peer + peer
	for _, child := range []string{"one", "two"} {
		if err := os.WriteFile(filepath.Join(children, "agent-"+child+".jsonl"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	h := newClaudeHistory(root)
	info := protocol.SessionInfo{SessionKey: "claude:parent", Tool: "claude"}
	refs := map[string]string{"task-one": "one", "task-one-again": "one", "task-two": "two"}
	firstEvents := h.childTranscriptEvents(parent, info, refs)
	first := claudePeerHistoryIDs(firstEvents)
	unique := map[string]bool{}
	for _, event := range firstEvents {
		if event.MessageOrigin == nil {
			continue
		}
		if !strings.HasPrefix(event.ID, "peer-history:") || unique[event.ID] || event.ParentID == "" || len(event.ChildSessionKeys) != 0 {
			t.Fatal("child source/record scope was lost", event)
		}
		unique[event.ID] = true
	}
	if len(first) != 6 || len(claudePeerHistoryIDs(dedupeEvents(firstEvents, 0))) != 6 {
		t.Fatal("same-text peers in distinct child sources or real parent scopes were collapsed", first)
	}
	file, err := os.OpenFile(filepath.Join(children, "agent-one.jsonl"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString("{}\n{}\n") // shifts the bounded tail's start, not the earlier absolute record positions
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	second := claudePeerHistoryIDs(h.childTranscriptEvents(parent, info, refs))
	if !reflect.DeepEqual(first, second) {
		t.Fatal("sliding the child tail changed earlier record identity", first, second)
	}
}

func TestClaudePeerProjectionDesktopFullAndTailKeepNativeIdentity(t *testing.T) {
	h, root := newDesktopHistoryFixture(t)
	writeDesktopRecord(t, root, desktopFixtureLocal, desktopFixtureRecord(desktopFixtureLocal, "chat", 2000))
	data := "\nnot-json\r\n" + claudePeerHistoryRecord(t, desktopFixtureCLI, "", "same desktop body") +
		claudePeerHistoryRecord(t, desktopFixtureCLI, "bad\nuuid", "same desktop body") +
		claudePeerHistoryRecord(t, desktopFixtureCLI, "native-peer", "same desktop body")
	writeDesktopTranscript(t, root, desktopFixtureLocal, "session", data)
	_, full, _, err := h.OpenPage(context.Background(), desktopFixtureLocal, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	ids := claudePeerHistoryIDs(full)
	if len(ids) != 3 || ids[0] == ids[1] || !strings.HasPrefix(ids[0], "peer-history:") || !strings.HasPrefix(ids[1], "peer-history:") || ids[2] != "peer:native-peer" {
		t.Fatal("desktop fallback replaced native identity or collapsed records", ids)
	}
	seen := map[string]bool{}
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("desktop peer pagination did not converge")
		}
		_, events, next, err := h.OpenMessagesPage(context.Background(), desktopFixtureLocal, cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, eventID := range claudePeerHistoryIDs(events) {
			if seen[eventID] || eventID != ids[0] && eventID != ids[1] && eventID != ids[2] {
				t.Fatal("desktop full/tail IDs diverged", eventID, ids)
			}
			seen[eventID] = true
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 3 {
		t.Fatal("desktop paging omitted a peer", seen)
	}
	_, repeated, _, err := h.OpenPage(context.Background(), desktopFixtureLocal, "", 10)
	if err != nil || !reflect.DeepEqual(ids, claudePeerHistoryIDs(repeated)) {
		t.Fatal("desktop repeated snapshot changed peer identities", err)
	}
}
