package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

const desktopFixtureLocal = "local_11111111-2222-4333-8444-555555555555"
const desktopFixtureCLI = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

func newDesktopHistoryFixture(t *testing.T) (*ClaudeDesktopHistory, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "local-agent-mode-sessions", "01234567-89ab-4cde-8fab-0123456789ab", "23456789-abcd-4ef0-8abc-0123456789ab")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	h, err := NewClaudeDesktopHistory(root, ClaudeDesktopHistorySchemaVersion)
	if err != nil {
		t.Fatal(err)
	}
	return h, root
}

func desktopFixtureRecord(id string, kind any, updated int64) map[string]any {
	r := map[string]any{"sessionId": id, "processName": "fixture-process", "cwd": "/fixture", "createdAt": 1000, "lastActivityAt": updated, "cliSessionId": desktopFixtureCLI, "title": "Desktop fixture", "model": "fixture-model"}
	if kind != nil {
		r["sessionType"] = kind
	}
	return r
}

func writeDesktopRecord(t *testing.T, dir string, id string, record map[string]any) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+".json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeDesktopTranscript(t *testing.T, root, folder, project, data string) string {
	t.Helper()
	dir := filepath.Join(root, folder, ".claude", "projects", project)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, desktopFixtureCLI+".jsonl")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func desktopFixtureMessages(count int) string {
	var data strings.Builder
	for i := 0; i < count; i++ {
		// Missing UUID is intentional: stable fallback identities must preserve
		// repeated identical messages across independently loaded history pages.
		raw, _ := json.Marshal(map[string]any{"type": "user", "sessionId": desktopFixtureCLI, "message": map[string]any{"role": "user", "content": "Repeated desktop message"}})
		data.Write(raw)
		data.WriteByte('\n')
	}
	return data.String()
}

func TestClaudeDesktopHistoryVersionAndExplicitRoot(t *testing.T) {
	h, root := newDesktopHistoryFixture(t)
	if _, err := NewClaudeDesktopHistory(root, "2.16120.0.0"); err != nil {
		t.Fatal(err)
	}
	// Later builds of the same line are read in compatibility mode (see
	// claude_desktop_history_compat_test.go); malformed versions never are.
	for _, version := range []string{"", "2.16119.0", "2.16120.0-extra"} {
		if _, err := NewClaudeDesktopHistory(root, version); err == nil {
			t.Fatalf("unsupported version accepted: %q", version)
		}
	}
	for _, path := range []string{".", filepath.Dir(root), t.TempDir(), filepath.Dir(filepath.Dir(root))} {
		if _, err := NewClaudeDesktopHistory(path, ClaudeDesktopHistorySchemaVersion); err == nil {
			t.Fatalf("unscoped root accepted: %q", path)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := h.SessionsPage(ctx, "", 10, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if _, _, err := h.SessionsPage(context.Background(), "", 10, "desktop-code"); err == nil {
		t.Fatal("Code/Chat scope conflation")
	}
}

func TestClaudeDesktopHistoryNativeDirectoryKindsAreReadOnly(t *testing.T) {
	h, root := newDesktopHistoryFixture(t)
	ids := []string{
		desktopFixtureLocal,
		"local_22222222-2222-4333-8444-555555555555",
		"local_33333333-2222-4333-8444-555555555555",
		"local_44444444-2222-4333-8444-555555555555",
		"local_55555555-2222-4333-8444-555555555555",
		"local_66666666-2222-4333-8444-555555555555",
	}
	kinds := []any{"chat", nil, "scheduled", "radar", "agent", "chat"}
	for i, id := range ids {
		r := desktopFixtureRecord(id, kinds[i], int64(2000+i))
		r["apiKey"] = "fixture-secret-never-exported"
		dir := root
		if kinds[i] == "agent" {
			dir = filepath.Join(root, "agent")
		}
		if i == 5 {
			r["isArchived"] = true
		}
		writeDesktopRecord(t, dir, id, r)
	}
	for scope, want := range map[string]int{"": 3, "desktop-chat": 1, "desktop-cowork": 2} {
		list, next, err := h.SessionsPage(context.Background(), "", 100, scope)
		if err != nil || len(list) != want || next != "" {
			t.Fatalf("%q: %v %d %q", scope, err, len(list), next)
		}
		for _, info := range list {
			if info.Controllable || info.ControlSurface != "read-only" || info.Client != "Claude Desktop" || !strings.HasPrefix(info.SessionKey, "claude-desktop:local_") || info.Status != "idle" {
				t.Fatalf("unsafe native identity/control: %+v", info)
			}
			raw, _ := json.Marshal(info)
			if strings.Contains(string(raw), "fixture-secret") || strings.Contains(string(raw), desktopFixtureCLI) {
				t.Fatal("configuration or CLI identity leaked")
			}
		}
	}
	if _, _, _, err := h.OpenPage(context.Background(), ids[4], "", 10); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hidden agent exposed: %v", err)
	}
}

func TestClaudeDesktopHistoryDescribeUsesMetadataOnlyAndNativeCategory(t *testing.T) {
	h, root := newDesktopHistoryFixture(t)
	for index, kind := range []any{"chat", nil, "scheduled"} {
		id := fmt.Sprintf("local_%08d-2222-4333-8444-555555555555", index+1)
		record := desktopFixtureRecord(id, kind, int64(2000+index))
		delete(record, "cliSessionId") // No resumable CLI ID or transcript exists.
		record["apiKey"] = "fixture-secret-never-exported"
		path := writeDesktopRecord(t, root, id, record)
		before, _ := os.ReadFile(path)
		info, err := h.Describe(context.Background(), id)
		want := "desktop-cowork"
		if kind == "chat" {
			want = "desktop-chat"
		}
		if err != nil || info.SessionKey != "claude-desktop:"+id || info.SessionScope != want || info.Controllable || info.ControlSurface != "read-only" || info.ControlExpiresAt != 0 {
			t.Fatalf("native metadata descriptor: %+v %v", info, err)
		}
		encoded, _ := json.Marshal(info)
		if strings.Contains(string(encoded), "fixture-secret") || strings.Contains(string(encoded), desktopFixtureCLI) {
			t.Fatal("metadata leaked credentials or CLI identity")
		}
		if _, _, _, err := h.OpenPage(context.Background(), id, "", 10); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing transcript became history: %v", err)
		}
		after, _ := os.ReadFile(path)
		if string(before) != string(after) {
			t.Fatal("Describe mutated native metadata")
		}
	}
	for _, id := range []string{desktopFixtureCLI, "claude:" + desktopFixtureCLI, "claude-desktop:" + desktopFixtureLocal, "../" + desktopFixtureLocal} {
		if _, err := h.Describe(context.Background(), id); err == nil {
			t.Fatalf("invalid/non-native descriptor identity: %q", id)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.Describe(ctx, "local_00000001-2222-4333-8444-555555555555"); !errors.Is(err, context.Canceled) {
		t.Fatalf("descriptor cancellation: %v", err)
	}
}

func TestClaudeDesktopHistoryDescribeRejectsHiddenAndArchivedRecords(t *testing.T) {
	h, root := newDesktopHistoryFixture(t)
	for index, kind := range []any{"radar", "agent", "dispatch_child", "chat"} {
		id := fmt.Sprintf("local_%08d-2222-4333-8444-555555555555", index+1)
		record := desktopFixtureRecord(id, kind, int64(2000+index))
		dir := root
		if kind == "agent" {
			dir = filepath.Join(root, "agent")
		}
		if kind == "chat" {
			record["isArchived"] = true
		}
		writeDesktopRecord(t, dir, id, record)
	}
	for index := 0; index < 5; index++ {
		id := fmt.Sprintf("local_%08d-2222-4333-8444-555555555555", index+1)
		if _, err := h.Describe(context.Background(), id); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("hidden/archived/missing record described: %s %v", id, err)
		}
	}
}

func TestClaudeDesktopHistoryOpenCannotBypassArchivedDirectoryFilter(t *testing.T) {
	h, root := newDesktopHistoryFixture(t)
	record := desktopFixtureRecord(desktopFixtureLocal, "chat", 2000)
	record["isArchived"] = true
	writeDesktopRecord(t, root, desktopFixtureLocal, record)
	writeDesktopTranscript(t, root, desktopFixtureLocal, "fixture-project", desktopFixtureMessages(1))
	info, events, next, err := h.OpenPage(context.Background(), desktopFixtureLocal, "", 10)
	if !errors.Is(err, os.ErrNotExist) || info.SessionKey != "" || len(events) != 0 || next != "" {
		t.Fatalf("archived record read through explicit ID: %+v %v %q %v", info, events, next, err)
	}
}

func TestClaudeDesktopHistoryFullAndShortLayoutsAndPaging(t *testing.T) {
	for _, folder := range []string{desktopFixtureLocal, "11111111"} {
		t.Run(folder, func(t *testing.T) {
			h, root := newDesktopHistoryFixture(t)
			writeDesktopRecord(t, root, desktopFixtureLocal, desktopFixtureRecord(desktopFixtureLocal, "chat", 2000))
			path := writeDesktopTranscript(t, root, folder, "session", desktopFixtureMessages(7))
			before, _ := os.ReadFile(path)
			seen := map[string]bool{}
			cursor := ""
			for pages := 0; ; pages++ {
				info, events, next, err := h.OpenPage(context.Background(), desktopFixtureLocal, cursor, 2)
				if err != nil || len(events) > 2 || info.Controllable || info.ControlSurface != "read-only" {
					t.Fatalf("page %d: %+v %v %d", pages, info, err, len(events))
				}
				for _, event := range events {
					if event.SessionKey != "claude-desktop:"+desktopFixtureLocal || event.Type != "message" || seen[event.ID] {
						t.Fatalf("lost/duplicate desktop identity: %+v", event)
					}
					seen[event.ID] = true
				}
				if next == "" {
					break
				}
				cursor = next
				if pages > 7 {
					t.Fatal("pagination never completed")
				}
			}
			if len(seen) != 7 {
				t.Fatalf("repeated messages collapsed: %d", len(seen))
			}
			after, _ := os.ReadFile(path)
			if string(before) != string(after) {
				t.Fatal("read-only adapter modified transcript")
			}
			for _, id := range []string{desktopFixtureCLI, "claude:" + desktopFixtureCLI, "../" + desktopFixtureLocal, "local_*", "claude-desktop:" + desktopFixtureLocal} {
				if _, events, _, err := h.OpenPage(context.Background(), id, "", 10); err == nil || len(events) > 0 {
					t.Fatalf("unsafe/native versus CLI ID accepted: %q %v", id, err)
				}
			}
		})
	}
}

func TestClaudeDesktopHistoryCursorsCannotCrossScopeOrAccount(t *testing.T) {
	h, root := newDesktopHistoryFixture(t)
	other, otherRoot := newDesktopHistoryFixture(t)
	for _, r := range []string{root, otherRoot} {
		for i, id := range []string{desktopFixtureLocal, "local_22222222-2222-4333-8444-555555555555"} {
			writeDesktopRecord(t, r, id, desktopFixtureRecord(id, "chat", int64(2000+i)))
		}
		writeDesktopTranscript(t, r, desktopFixtureLocal, "session", desktopFixtureMessages(3))
	}
	_, listCursor, err := h.SessionsPage(context.Background(), "", 1, "desktop-chat")
	if err != nil || listCursor == "" {
		t.Fatalf("list cursor: %v %q", err, listCursor)
	}
	if _, _, err := h.SessionsPage(context.Background(), listCursor, 1, "desktop-cowork"); err == nil {
		t.Fatal("cross-kind cursor accepted")
	}
	if _, _, err := other.SessionsPage(context.Background(), listCursor, 1, "desktop-chat"); err == nil {
		t.Fatal("cross-account list cursor accepted")
	}
	_, _, historyCursor, err := h.OpenPage(context.Background(), desktopFixtureLocal, "", 1)
	if err != nil || historyCursor == "" {
		t.Fatalf("history cursor: %v %q", err, historyCursor)
	}
	if _, _, _, err := other.OpenPage(context.Background(), desktopFixtureLocal, historyCursor, 1); err == nil {
		t.Fatal("cross-account history cursor accepted")
	}
}

func TestClaudeDesktopHistoryRejectsAmbiguousDirectories(t *testing.T) {
	for _, mode := range []string{"full and short", "short collision", "duplicate transcript", "duplicate record"} {
		t.Run(mode, func(t *testing.T) {
			h, root := newDesktopHistoryFixture(t)
			writeDesktopRecord(t, root, desktopFixtureLocal, desktopFixtureRecord(desktopFixtureLocal, "chat", 2000))
			switch mode {
			case "full and short":
				writeDesktopTranscript(t, root, desktopFixtureLocal, "session", desktopFixtureMessages(1))
				writeDesktopTranscript(t, root, "11111111", "session", desktopFixtureMessages(1))
			case "short collision":
				id := "local_11111111-ffff-4333-8444-555555555555"
				writeDesktopRecord(t, root, id, desktopFixtureRecord(id, "chat", 2000))
				writeDesktopTranscript(t, root, "11111111", "session", desktopFixtureMessages(1))
			case "duplicate transcript":
				writeDesktopTranscript(t, root, desktopFixtureLocal, "session", desktopFixtureMessages(1))
				writeDesktopTranscript(t, root, desktopFixtureLocal, "other-project", desktopFixtureMessages(1))
			case "duplicate record":
				writeDesktopRecord(t, filepath.Join(root, "agent"), desktopFixtureLocal, desktopFixtureRecord(desktopFixtureLocal, "agent", 2000))
			}
			if _, events, _, err := h.OpenPage(context.Background(), desktopFixtureLocal, "", 10); err == nil || len(events) > 0 {
				t.Fatalf("ambiguity accepted: %v %d", err, len(events))
			}
		})
	}
}

func TestClaudeDesktopHistoryRejectsUnverifiedRecordTypes(t *testing.T) {
	for _, mode := range []string{"missing required", "optional null", "wrong bool", "identity mismatch", "timestamp string", "negative time", "invalid CLI UUID", "duplicate field", "array"} {
		t.Run(mode, func(t *testing.T) {
			h, root := newDesktopHistoryFixture(t)
			r := desktopFixtureRecord(desktopFixtureLocal, "chat", 2000)
			switch mode {
			case "missing required":
				delete(r, "processName")
			case "optional null":
				r["model"] = nil
			case "wrong bool":
				r["isArchived"] = "false"
			case "identity mismatch":
				r["sessionId"] = "local_22222222-2222-4333-8444-555555555555"
			case "timestamp string":
				r["createdAt"] = "2000"
			case "negative time":
				r["lastActivityAt"] = -1
			case "invalid CLI UUID":
				r["cliSessionId"] = "../../other"
			}
			path := writeDesktopRecord(t, root, desktopFixtureLocal, r)
			if mode == "duplicate field" {
				data, _ := os.ReadFile(path)
				data = append([]byte(`{"sessionId":"another",`), data[1:]...)
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "array" {
				if err := os.WriteFile(path, []byte("[]"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := h.SessionsPage(context.Background(), "", 10, ""); err == nil {
				t.Fatal("unverified metadata accepted")
			}
		})
	}
}

func TestClaudeDesktopHistoryReadFailuresRemainErrors(t *testing.T) {
	for _, mode := range []string{"missing transcript", "missing CLI ID", "wrong transcript identity", "metadata budget", "transcript budget", "line budget"} {
		t.Run(mode, func(t *testing.T) {
			h, root := newDesktopHistoryFixture(t)
			r := desktopFixtureRecord(desktopFixtureLocal, "chat", 2000)
			if mode == "missing CLI ID" {
				delete(r, "cliSessionId")
			}
			record := writeDesktopRecord(t, root, desktopFixtureLocal, r)
			data := desktopFixtureMessages(1)
			if mode == "wrong transcript identity" {
				data = strings.ReplaceAll(data, desktopFixtureCLI, "bbbbbbbb-bbbb-4ccc-8ddd-eeeeeeeeeeee")
			}
			path := writeDesktopTranscript(t, root, desktopFixtureLocal, "session", data)
			switch mode {
			case "missing transcript":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "metadata budget", "transcript budget":
				size := int64(claudeDesktopTranscriptBytes + 1)
				target := path
				if mode == "metadata budget" {
					size = claudeDesktopRecordBytes + 1
					target = record
				}
				file, err := os.OpenFile(target, os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				err = file.Truncate(size)
				file.Close()
				if err != nil {
					t.Fatal(err)
				}
			case "line budget":
				if err := os.WriteFile(path, []byte(strings.Repeat("x", (16<<20)+1)+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, events, next, err := h.OpenPage(context.Background(), desktopFixtureLocal, "", 10)
			if err == nil || len(events) > 0 || next != "" {
				t.Fatalf("failure became empty/successful history: %v %d %q", err, len(events), next)
			}
			if mode == "line budget" && !errors.Is(err, ErrLineTooLarge) {
				t.Fatalf("line bound: %v", err)
			}
		})
	}
}

func TestClaudeDesktopHistoryLinkedTranscriptTreeIsRejected(t *testing.T) {
	h, root := newDesktopHistoryFixture(t)
	writeDesktopRecord(t, root, desktopFixtureLocal, desktopFixtureRecord(desktopFixtureLocal, "chat", 2000))
	outside := t.TempDir()
	file := writeDesktopTranscript(t, filepath.Dir(outside), filepath.Base(outside), "session", desktopFixtureMessages(1))
	link := filepath.Join(root, desktopFixtureLocal)
	if runtime.GOOS == "windows" {
		cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", `New-Item -ItemType Junction -Path $env:SALCARA_DESKTOP_HISTORY_TEST_LINK -Target $env:SALCARA_DESKTOP_HISTORY_TEST_TARGET -ErrorAction Stop | Out-Null`)
		cmd.Env = append(os.Environ(), "SALCARA_DESKTOP_HISTORY_TEST_LINK="+link, "SALCARA_DESKTOP_HISTORY_TEST_TARGET="+outside)
		if data, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fixture junction: %v %s", err, data)
		}
	} else if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Error(err)
		}
	})
	if _, events, _, err := h.OpenPage(context.Background(), desktopFixtureLocal, "", 10); err == nil || len(events) > 0 {
		t.Fatalf("linked transcript exposed: %v %d", err, len(events))
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("read-only check changed target", err)
	}
}

func TestClaudeDesktopHistoryDirectoryEntryBudget(t *testing.T) {
	h, root := newDesktopHistoryFixture(t)
	// Use a small direct bound to exercise the same bounded enumerator without
	// constructing thousands of files during every bridge test run.
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("fixture-%d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.directory(context.Background(), root, 2); err == nil {
		t.Fatal("directory entry bound ignored")
	}
}

type desktopHistoryCancelDuringRead struct {
	context.Context
	checks atomic.Int32
}

func (ctx *desktopHistoryCancelDuringRead) Err() error {
	if ctx.checks.Add(1) > 10 {
		return context.Canceled
	}
	return nil
}

func TestClaudeDesktopHistoryMappingBudgetAndCancellation(t *testing.T) {
	t.Run("cancellation drops partial page", func(t *testing.T) {
		h, root := newDesktopHistoryFixture(t)
		writeDesktopRecord(t, root, desktopFixtureLocal, desktopFixtureRecord(desktopFixtureLocal, "chat", 2000))
		writeDesktopTranscript(t, root, desktopFixtureLocal, "session", desktopFixtureMessages(100))
		ctx := &desktopHistoryCancelDuringRead{Context: context.Background()}
		_, events, next, err := h.OpenPage(ctx, desktopFixtureLocal, "", 10)
		if !errors.Is(err, context.Canceled) || len(events) > 0 || next != "" || ctx.checks.Load() <= 10 {
			t.Fatalf("partial/canceled success: %v %d %q", err, len(events), next)
		}
	})
	t.Run("mapped bytes", func(t *testing.T) {
		h, root := newDesktopHistoryFixture(t)
		writeDesktopRecord(t, root, desktopFixtureLocal, desktopFixtureRecord(desktopFixtureLocal, "chat", 2000))
		path := writeDesktopTranscript(t, root, desktopFixtureLocal, "session", "")
		f, err := os.OpenFile(path, os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		encoder := json.NewEncoder(f)
		text := strings.Repeat("x", 50000)
		for i := 0; i < 700; i++ {
			if err := encoder.Encode(map[string]any{"type": "user", "uuid": fmt.Sprintf("user-%d", i), "sessionId": desktopFixtureCLI, "message": map[string]any{"role": "user", "content": text}}); err != nil {
				f.Close()
				t.Fatal(err)
			}
		}
		f.Close()
		_, events, next, err := h.OpenPage(context.Background(), desktopFixtureLocal, "", 10)
		if err == nil || !strings.Contains(err.Error(), "历史内容过大") || len(events) > 0 || next != "" {
			t.Fatalf("mapped limit suppressed: %v %d %q", err, len(events), next)
		}
	})
}
