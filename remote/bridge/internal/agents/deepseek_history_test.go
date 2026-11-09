package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"salcara/bridge/internal/protocol"
)

func deepSeekHistoryFixture(t *testing.T, count int) (*deepSeekAgent, string, string, []byte) {
	t.Helper()
	a := newDeepSeekAgent(func() Settings { t.Error("history read consulted launch settings"); return Settings{} }, t.TempDir())
	id, cwd := deepSeekNewID(), t.TempDir()
	a.sessions[id] = &deepSeekSession{Info: protocol.SessionInfo{Tool: deepSeekID, SessionKey: deepSeekID + ":" + id, Cwd: cwd, Status: "idle", Controllable: true}, Started: true, approvals: map[string]deepSeekApproval{}}
	header, err := json.Marshal(map[string]any{"type": "session", "version": 4, "id": id, "cwd": cwd, "createdAt": 1, "isSeeded": false, "delegationDepth": 0})
	if err != nil {
		t.Fatal(err)
	}
	data := append(header, '\n')
	for seq := 0; seq < count; seq++ {
		data = append(data, deepSeekFixtureMessage(t, seq)...)
	}
	path := filepath.Join(a.stateDir, id, "native", "sessions", deepSeekProjectKey(cwd), id, "session.v4.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return a, id, path, data
}

func deepSeekFixtureMessage(t *testing.T, seq int) []byte {
	t.Helper()
	role, kind := "user", "user/message"
	message := map[string]any{"id": fmt.Sprintf("m-%06d", seq), "role": role, "content": []any{map[string]string{"type": "text", "text": fmt.Sprintf("record-%06d", seq)}}, "source": map[string]string{"kind": "user"}}
	var data any = message
	if seq%2 != 0 {
		kind, message["role"] = "assistant/message", "assistant"
		delete(message, "source")
		data = map[string]any{"turn": seq/2 + 1, "step": 1, "message": message, "stream": []any{}}
	}
	row, err := json.Marshal(map[string]any{"type": kind, "seq": seq, "time": 1000 + seq, "data": data})
	if err != nil {
		t.Fatal(err)
	}
	return append(row, '\n')
}

func TestDeepSeekHistoryReadsBeyondCacheAfterRestart(t *testing.T) {
	a, id, path, original := deepSeekHistoryFixture(t, 710)
	s := a.sessions[id]
	s.Events = []protocol.Event{{Tool: deepSeekID, SessionKey: s.Info.SessionKey, Type: "message", ID: "m-000709", Role: "assistant", Text: "truncated cache", Final: true}}
	if err := a.persistLocked(id, s); err != nil {
		t.Fatal(err)
	}
	b := newDeepSeekAgent(func() Settings { t.Error("history started a runtime"); return Settings{} }, filepath.Dir(a.stateDir))
	seen := map[string]bool{}
	cursor, pages := "", 0
	for {
		info, events, next, err := b.OpenMessagesPage(context.Background(), id, cursor, 10)
		if err != nil || info.Status != "idle" {
			t.Fatalf("history: %+v %v", info, err)
		}
		messages := 0
		for _, event := range events {
			if event.Type != "message" {
				continue
			}
			messages++
			if seen[event.ID] {
				t.Fatalf("duplicate message %s", event.ID)
			}
			seen[event.ID] = true
			if event.ID == "m-000709" && event.Text != "record-000709" {
				t.Fatal("cached truncation replaced native content")
			}
		}
		if messages > 10 {
			t.Fatal("message page exceeded bound")
		}
		pages++
		if next == "" {
			break
		}
		cursor = next
		if pages > 100 {
			t.Fatal("pagination did not end")
		}
	}
	if len(seen) != 710 || !seen["m-000000"] || pages != 71 {
		t.Fatalf("lost older records: %d messages / %d pages", len(seen), pages)
	}
	_, tail, err := b.Open(context.Background(), id)
	if err != nil || len(tail) != 400 || tail[0].Text != "record-000310" {
		t.Fatalf("bounded Open tail: %d %v", len(tail), err)
	}
	if exists, err := b.deepSeekNativeSessionExists(context.Background(), id); !exists || err != nil {
		t.Fatal("native existence", exists, err)
	}
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(current, original) {
		t.Fatal("read modified native storage", err)
	}
}

func TestDeepSeekHistoryCursorSurvivesAppendAndRejectsForeignScope(t *testing.T) {
	a, id, path, _ := deepSeekHistoryFixture(t, 30)
	_, _, cursor, err := a.OpenMessagesPage(context.Background(), id, "", 10)
	if err != nil || cursor == "" {
		t.Fatal(cursor, err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.Write(deepSeekFixtureMessage(t, 30))
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	_, older, next, err := a.OpenMessagesPage(context.Background(), id, cursor, 10)
	if err != nil || next == "" || len(older) != 10 || older[0].Text != "record-000010" || older[9].Text != "record-000019" {
		t.Fatalf("append shifted cursor: %+v %s %v", older, next, err)
	}
	b, other, _, _ := deepSeekHistoryFixture(t, 30)
	if _, _, _, err := b.OpenMessagesPage(context.Background(), other, cursor, 10); err == nil {
		t.Fatal("accepted another session's cursor")
	}
}

func TestDeepSeekHistoryPreservesFenceApprovalsAndUnflushedTail(t *testing.T) {
	a, id, _, _ := deepSeekHistoryFixture(t, 5)
	s := a.sessions[id]
	s.Info.Status, s.Info.Controllable, s.Unconfirmed = "unknown", false, true
	s.Events = []protocol.Event{
		{Tool: deepSeekID, SessionKey: s.Info.SessionKey, Type: "message", ID: "live-not-flushed", Role: "assistant", Text: "live tail"},
		{Tool: deepSeekID, SessionKey: s.Info.SessionKey, Type: "notice", ID: "deepseek-runtime-recovered", Text: "results unknown"},
		{Tool: deepSeekID, SessionKey: s.Info.SessionKey, Type: "approval.request", ApprovalID: "pending", Title: "live approval"},
		{Tool: deepSeekID, SessionKey: s.Info.SessionKey, Type: "approval.request", ApprovalID: "stale", Title: "retired approval"},
	}
	s.approvals["pending"] = deepSeekApproval{}
	info, events, _, err := a.OpenMessagesPage(context.Background(), id, "", 10)
	if err != nil || info.Status != "unknown" || info.Controllable || !a.ActiveRemoteTurns() || !s.Unconfirmed {
		t.Fatal("history read changed runtime fence", info, err)
	}
	ids := map[string]bool{}
	for _, event := range events {
		ids[event.ID] = true
		if event.ApprovalID == "stale" {
			t.Fatal("stale approval revived")
		}
	}
	if !ids["live-not-flushed"] || !ids["deepseek-runtime-recovered"] || !ids["deepseek-runtime-unconfirmed"] {
		t.Fatalf("lost live events/notices: %+v", events)
	}
	if events[len(events)-2].ApprovalID != "pending" {
		t.Fatalf("lost active approval: %+v", events)
	}
	if s.conn != nil {
		t.Fatal("read created a native connection")
	}
}

func TestDeepSeekHistoryMalformedDurableRecordNeverFallsBackSilently(t *testing.T) {
	for _, mutation := range []string{"invalid-json", "seq-gap", "message-shape", "wrong-id", "wrong-cwd", "future-generation", "symlink"} {
		t.Run(mutation, func(t *testing.T) {
			a, id, path, data := deepSeekHistoryFixture(t, 3)
			s := a.sessions[id]
			s.Events = []protocol.Event{{Tool: deepSeekID, SessionKey: s.Info.SessionKey, Type: "message", ID: "cache", Role: "user", Text: "must not disguise corrupt native"}}
			switch mutation {
			case "invalid-json":
				data = append(data, []byte("{invalid}\n")...)
			case "seq-gap":
				data = append(data, deepSeekFixtureMessage(t, 7)...)
			case "message-shape":
				data = append(data, []byte(`{"type":"assistant/message","seq":3,"time":2,"data":{"message":{"id":"broken","role":"assistant","content":"wrong"}}}`+"\n")...)
			case "wrong-id":
				data = bytes.Replace(data, []byte(id), []byte(strings.Repeat("f", 32)), 1)
			case "wrong-cwd":
				data = bytes.Replace(data, []byte(`"cwd":`), []byte(`"otherCwd":`), 1)
			case "future-generation":
				if err := os.WriteFile(filepath.Join(filepath.Dir(path), "session.v5.jsonl"), data, 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				outside := filepath.Join(t.TempDir(), "external.jsonl")
				if err := os.WriteFile(outside, data, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Skip("symlink unavailable:", err)
				}
			}
			if mutation != "symlink" {
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := a.Open(context.Background(), id); err == nil {
				t.Fatal("silently returned cache for invalid native history")
			}
			if _, err := a.deepSeekNativeSessionExists(context.Background(), id); err == nil {
				t.Fatal("invalid native file accepted as resume evidence")
			}
		})
	}
}

func TestDeepSeekHistoryMissingAndTornTailAreExplicitReadOnlyStates(t *testing.T) {
	a, id, path, data := deepSeekHistoryFixture(t, 2)
	if err := os.WriteFile(path, append(data, []byte(`{"type":"assistant/message"`)...), 0o600); err != nil {
		t.Fatal(err)
	}
	_, events, err := a.Open(context.Background(), id)
	if err != nil || len(events) != 3 || events[2].ID != "deepseek-history-incomplete-tail" {
		t.Fatalf("torn tail: %+v %v", events, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if exists, err := a.deepSeekNativeSessionExists(context.Background(), id); err != nil || exists {
		t.Fatal("missing log claimed as existing", exists, err)
	}
	_, events, err = a.Open(context.Background(), id)
	if err != nil || len(events) != 1 || events[0].ID != "deepseek-history-unavailable" {
		t.Fatalf("missing history was silent: %+v %v", events, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := a.Open(ctx, id); err != context.Canceled {
		t.Fatal("cancellation ignored", err)
	}
}

func TestDeepSeekHistoryReadsCompressedV4AndPreservesToolDescription(t *testing.T) {
	a, id, path, data := deepSeekHistoryFixture(t, 2)
	data = append(data, []byte(`{"type":"tool/call","seq":2,"time":1002,"data":{"callId":"call-1","name":"read","arguments":"{}"}}`+"\n")...)
	data = append(data, []byte(`{"type":"tool/result","seq":3,"time":1003,"data":{"message":{"role":"tool","toolCallId":"call-1","content":[{"type":"text","text":"result"}]}}}`+"\n")...)
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	compressed := encoder.EncodeAll(data, nil)
	encoder.Close()
	if err := os.WriteFile(path+".zstd", compressed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	_, events, err := a.Open(context.Background(), id)
	if err != nil || len(events) != 3 {
		t.Fatalf("zstd read: %+v %v", events, err)
	}
	tool := events[2]
	if tool.Type != "tool" || tool.Status != "completed" || tool.Title != "read" || tool.Detail != "{}" || tool.Output != "result" {
		t.Fatal("lost completed tool context", tool)
	}
}

func TestDeepSeekHistoryProjectKeyMatchesNativeUTF16Encoding(t *testing.T) {
	for _, tc := range []struct{ cwd, want string }{
		{`C:\Work\hello`, "--C-Work-hello--"},
		{"/tmp/项目/😀", "--tmp-~9879~76EE-~D83D~DE00--"},
		{"/", "--root--"},
	} {
		if got := deepSeekProjectKey(tc.cwd); got != tc.want {
			t.Fatalf("%q: %q != %q", tc.cwd, got, tc.want)
		}
	}
}

func TestDeepSeekHistoryRejectsDuplicateProjectAndLegacyArtifacts(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint(legacy), func(t *testing.T) {
			a, id, path, data := deepSeekHistoryFixture(t, 2)
			project := filepath.Join(a.stateDir, id, "native", "sessions", "--other-project--")
			destination := filepath.Join(project, id, "session.v4.jsonl")
			if legacy {
				destination = filepath.Join(filepath.Dir(filepath.Dir(path)), id+".jsonl")
			}
			if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(destination, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := a.deepSeekNativeSessionExists(context.Background(), id); err == nil {
				t.Fatal("ambiguous/legacy storage admitted as a new or resumable session")
			}
		})
	}
}

func TestDeepSeekHistoryRejectsOversizedRecord(t *testing.T) {
	a, id, path, data := deepSeekHistoryFixture(t, 2)
	data = append(data, bytes.Repeat([]byte("x"), deepSeekHistoryLineBytes+1)...)
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Open(context.Background(), id); err == nil {
		t.Fatal("oversized native row accepted")
	}
}

func TestDeepSeekHistoryMatchesSDKNormalizedWorkingDirectory(t *testing.T) {
	a, id, _, _ := deepSeekHistoryFixture(t, 2)
	cwd := a.sessions[id].Info.Cwd
	separator := string(filepath.Separator)
	variants := []string{cwd + separator, cwd + separator + ".", cwd + separator + "child" + separator + ".."}
	if runtime.GOOS == "windows" {
		variants = append(variants, filepath.ToSlash(cwd), filepath.ToSlash(cwd)+"/child/../.")
	}
	for _, spelling := range variants {
		t.Run(spelling, func(t *testing.T) {
			a.sessions[id].Info.Cwd = spelling
			info, events, err := a.Open(context.Background(), id)
			if err != nil || len(events) != 2 || events[0].Text != "record-000000" || info.Cwd != spelling {
				t.Fatalf("equivalent cwd refused or mutated: %+v %+v %v", info, events, err)
			}
			if exists, err := a.deepSeekNativeSessionExists(context.Background(), id); err != nil || !exists {
				t.Fatal("normalized native session not found", exists, err)
			}
		})
	}
	a.sessions[id].Info.Cwd = filepath.Join(cwd, "genuinely-different")
	if _, err := a.deepSeekNativeSessionExists(context.Background(), id); err == nil {
		t.Fatal("different native working directory was admitted")
	}
}
