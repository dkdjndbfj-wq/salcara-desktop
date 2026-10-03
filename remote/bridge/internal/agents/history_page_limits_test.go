package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/protocol"
)

func newHistoryLimitAgent(t *testing.T, id string) (*claudeAgent, string) {
	t.Helper()
	root := t.TempDir()
	a := newClaudeAgent(func(protocol.Event) {}, func() Settings { return Settings{} }, "not-launched", root)
	t.Cleanup(a.Close)
	a.live[id] = protocol.SessionInfo{SessionKey: "claude:" + id, Tool: "claude", Title: "Live fixture"}
	dir := filepath.Join(root, "projects", "fixture")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return a, filepath.Join(dir, id+".jsonl")
}

func writeHistoryLimitHeader(t *testing.T, file *os.File, id string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"type": "user", "uuid": "first-prompt", "sessionId": id,
		"entrypoint": "cli", "cwd": "/fixture", "timestamp": "2026-10-03T00:00:00Z",
		"message": map[string]any{"role": "user", "content": "History fixture"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(append(raw, '\n')); err != nil {
		t.Fatal(err)
	}
}

// Cancel only after OpenPage's initial check. This deterministically exercises
// errors from its history reader instead of only the preflight cancellation.
type cancelAfterHistoryPreflight struct {
	context.Context
	checks atomic.Int32
}

func (ctx *cancelAfterHistoryPreflight) Err() error {
	if ctx.checks.Add(1) > 1 {
		return context.Canceled
	}
	return nil
}

func TestClaudeOpenPageLiveReadErrorsAreNotEmptySuccess(t *testing.T) {
	t.Run("input limit", func(t *testing.T) {
		id := "fixture-input-limit"
		a, path := newHistoryLimitAgent(t, id)
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		writeHistoryLimitHeader(t, file, id)
		if err := file.Truncate((64 << 20) + 1); err != nil {
			file.Close()
			t.Fatal(err)
		}
		file.Close()
		_, events, next, err := a.OpenPage(context.Background(), id, "", 400)
		if err == nil || !strings.Contains(err.Error(), "历史文件过大") || len(events) != 0 || next != "" {
			t.Fatalf("limit became successful empty history: %v %d %q", err, len(events), next)
		}
	})
	t.Run("mapped limit", func(t *testing.T) {
		id := "fixture-mapped-limit"
		a, path := newHistoryLimitAgent(t, id)
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		writeHistoryLimitHeader(t, file, id)
		text := strings.Repeat("x", 50000)
		encoder := json.NewEncoder(file)
		for i := 0; i < 700; i++ {
			if err := encoder.Encode(map[string]any{"type": "user", "uuid": fmt.Sprintf("user-%d", i), "message": map[string]any{"role": "user", "content": text}}); err != nil {
				file.Close()
				t.Fatal(err)
			}
		}
		file.Close()
		_, events, _, err := a.OpenPage(context.Background(), id, "", 400)
		if err == nil || !strings.Contains(err.Error(), "历史内容过大") || len(events) != 0 {
			t.Fatalf("mapped limit was suppressed: %v %d", err, len(events))
		}
	})
	t.Run("record limit", func(t *testing.T) {
		id := "fixture-record-limit"
		a, path := newHistoryLimitAgent(t, id)
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		writeHistoryLimitHeader(t, file, id)
		if _, err := io.Copy(file, strings.NewReader(strings.Repeat("x", (16<<20)+1)+"\n")); err != nil {
			file.Close()
			t.Fatal(err)
		}
		file.Close()
		_, events, _, err := a.OpenPage(context.Background(), id, "", 400)
		if !errors.Is(err, ErrLineTooLarge) || len(events) != 0 {
			t.Fatalf("record limit was suppressed: %v %d", err, len(events))
		}
	})
	t.Run("cancel after preflight", func(t *testing.T) {
		id := "fixture-history-cancel"
		a, path := newHistoryLimitAgent(t, id)
		if err := os.WriteFile(path, []byte(transcript), 0600); err != nil {
			t.Fatal(err)
		}
		ctx := &cancelAfterHistoryPreflight{Context: context.Background()}
		_, events, next, err := a.OpenPage(ctx, id, "", 400)
		if !errors.Is(err, context.Canceled) || len(events) != 0 || next != "" || ctx.checks.Load() < 2 {
			t.Fatalf("reader cancellation became successful empty history: %v %d %q", err, len(events), next)
		}
	})
}

func TestClaudeOpenPageOnlyAbsentLegalLiveSessionCanBeEmpty(t *testing.T) {
	for _, mode := range []string{"legal", "absent live", "different identity", "different tool", "invalid ID"} {
		t.Run(mode, func(t *testing.T) {
			id := "fixture-new-live"
			if mode == "invalid ID" {
				id = "fixture*"
			}
			a, _ := newHistoryLimitAgent(t, id)
			switch mode {
			case "absent live":
				delete(a.live, id)
			case "different identity":
				info := a.live[id]
				info.SessionKey = "claude:another-session"
				a.live[id] = info
			case "different tool":
				info := a.live[id]
				info.Tool = "codex"
				a.live[id] = info
			}
			info, events, next, err := a.OpenPage(context.Background(), id, "", 400)
			if mode == "legal" {
				if err != nil || info.SessionKey != "claude:"+id || len(events) != 0 || next != "" {
					t.Fatalf("legitimate not-yet-written live session: %+v %v %d %q", info, err, len(events), next)
				}
			} else if err == nil {
				t.Fatalf("illegal empty fallback accepted: %+v", info)
			}
		})
	}
}

func TestClaudeOpenPageWalksOneThousandRealTranscriptMessages(t *testing.T) {
	id := "11111111-2222-4333-8444-555555555555"
	a, path := newHistoryLimitAgent(t, id)
	delete(a.live, id)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	start := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 1000; i++ {
		role := "user"
		message := map[string]any{"role": role, "content": fmt.Sprintf("History message %d", i)}
		if i%2 != 0 {
			role = "assistant"
			message = map[string]any{"id": fmt.Sprintf("assistant-%d", i), "role": role, "model": "fixture-model", "content": []any{map[string]any{"type": "text", "text": fmt.Sprintf("History message %d", i)}}}
		}
		line := map[string]any{"type": role, "uuid": fmt.Sprintf("line-%d", i), "sessionId": id, "entrypoint": "cli", "cwd": "/fixture", "timestamp": start.Add(time.Duration(i) * time.Second).Format(time.RFC3339), "message": message}
		if err := encoder.Encode(line); err != nil {
			file.Close()
			t.Fatal(err)
		}
	}
	file.Close()
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		info, events, next, err := a.OpenPage(context.Background(), id, cursor, 400)
		if err != nil || info.SessionKey != "claude:"+id || len(events) > 400 {
			t.Fatalf("page %d identity/limit: %+v %v %d", pages, info, err, len(events))
		}
		for _, event := range events {
			if event.Type != "message" || event.SessionKey != info.SessionKey || seen[event.Text] {
				t.Fatalf("duplicate, foreign or unexpected record: %+v", event)
			}
			seen[event.Text] = true
		}
		pages++
		if next == "" {
			break
		}
		if next == cursor || pages > 3 {
			t.Fatal("cursor did not advance", pages)
		}
		cursor = next
	}
	if pages != 3 || len(seen) != 1000 || !seen["History message 0"] || !seen["History message 999"] {
		t.Fatalf("real transcript omitted historical messages: %d pages, %d records", pages, len(seen))
	}
}

func TestClaudeHistoryGlobIDsDoNotOpenOtherTranscript(t *testing.T) {
	a, path := newHistoryLimitAgent(t, "real-session")
	if err := os.WriteFile(path, []byte(transcript), 0600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"*", "real?session", "[r]eal-session", "../real-session", `..\real-session`} {
		if path, err := a.hist.path(id); err == nil || path != "" {
			t.Fatalf("unsafe glob/path selected a transcript: %q %q %v", id, path, err)
		}
		if _, events, _, err := a.OpenPage(context.Background(), id, "", 400); err == nil || len(events) != 0 {
			t.Fatalf("unsafe ID exposed another conversation: %q %v %d", id, err, len(events))
		}
	}
}

func TestClaudeHistoryExternalLinkedDirectoryIsNotExposed(t *testing.T) {
	id := "external-session"
	a, _ := newHistoryLimitAgent(t, id)
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, id+".jsonl")
	if err := os.WriteFile(outsideFile, []byte(transcript), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(a.hist.dir, "external")
	if runtime.GOOS == "windows" {
		// A junction needs no privileged symlink permission. Both absolute paths
		// are freshly-created test directories, never user transcript locations.
		command := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", `New-Item -ItemType Junction -Path $env:SALCARA_TEST_LINK -Target $env:SALCARA_TEST_TARGET -ErrorAction Stop | Out-Null`)
		command.Env = append(os.Environ(), "SALCARA_TEST_LINK="+link, "SALCARA_TEST_TARGET="+outside)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("create fixture junction: %v %s", err, output)
		}
	} else if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Error("remove fixture link", err)
		}
	})
	if files := a.hist.files(); len(files) != 0 {
		t.Fatalf("external transcript appeared in discovery: %v", files)
	}
	if path, err := a.hist.path(id); err == nil || path != "" {
		t.Fatalf("external transcript accepted: %q %v", path, err)
	}
	if _, events, _, err := a.OpenPage(context.Background(), id, "", 400); err == nil || len(events) != 0 {
		t.Fatalf("outside file was exposed or hidden behind empty live success: %v %d", err, len(events))
	}
	if sessions, _, err := a.SessionsPage(context.Background(), "", 100, "code"); err != nil || len(sessions) != 1 || sessions[0].Title != "Live fixture" {
		t.Fatalf("external file contaminated the valid live metadata: %v %+v", err, sessions)
	}
	if _, err := os.Stat(outsideFile); err != nil {
		t.Fatal("fixture boundary check changed target", err)
	}
}
