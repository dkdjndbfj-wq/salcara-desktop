package agents

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClaudeHistoryListCancellationAndFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if rows, err := newClaudeHistory(t.TempDir()).listContext(ctx); !errors.Is(err, context.Canceled) || rows != nil {
		t.Fatalf("cancelled read: %v %d", err, len(rows))
	}
	root := t.TempDir()
	if rows, err := newClaudeHistory(filepath.Join(root, "absent")).listContext(context.Background()); err != nil || len(rows) != 0 {
		t.Fatalf("no installed history: %v", err)
	}
	file := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(file, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if rows, err := newClaudeHistory(file).listContext(context.Background()); err == nil || rows != nil {
		t.Fatal("unreadable root was treated as empty history")
	}
}

func TestClaudeHistoryListInvalidatesChangesWithoutCrossingRoots(t *testing.T) {
	home := t.TempDir()
	h := newClaudeHistory(filepath.Join(home, "projects"))
	p := writeTranscript(t, home, "one", transcript, time.Hour)
	rows, err := h.listContext(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatalf("first read: %v %d", err, len(rows))
	}
	writeTranscript(t, home, "two", transcript, 0)
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	rows, err = h.listContext(context.Background())
	if err != nil || len(rows) != 1 || rows[0].SessionKey != "claude:two" {
		t.Fatalf("stale index: %v", err)
	}
	writeTranscript(t, home, "two", transcript+"\n{\"type\":\"custom-title\",\"sessionId\":\"two\",\"customTitle\":\"Updated fixture title\"}\n", 0)
	rows, err = h.listContext(context.Background())
	if err != nil || len(rows) != 1 || rows[0].Title != "Updated fixture title" {
		t.Fatalf("stale summary: %v", err)
	}
	other := newClaudeHistory(t.TempDir())
	rows, err = other.listContext(context.Background())
	if err != nil || len(rows) != 0 {
		t.Fatal("history crossed roots")
	}
}

func BenchmarkClaudeHistoryList(b *testing.B) {
	root := b.TempDir()
	for i := 0; i < 600; i++ {
		dir := filepath.Join(root, fmt.Sprintf("project-%d", i%12))
		if err := os.MkdirAll(dir, 0700); err != nil {
			b.Fatal(err)
		}
		p := filepath.Join(dir, fmt.Sprintf("session-%04d.jsonl", i))
		if err := os.WriteFile(p, []byte(fmt.Sprintf("{\"type\":\"user\",\"sessionId\":\"session-%04d\",\"cwd\":\"/fixture/project-%d\",\"entrypoint\":\"cli\",\"message\":{\"role\":\"user\",\"content\":\"Fixture\"}}\n", i, i%12)), 0600); err != nil {
			b.Fatal(err)
		}
	}
	h := newClaudeHistory(root)
	h.list()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if rows := h.list(); len(rows) == 0 {
			b.Fatal("missing fixture rows")
		}
	}
}
