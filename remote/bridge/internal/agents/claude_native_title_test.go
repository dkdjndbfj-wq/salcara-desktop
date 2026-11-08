package agents

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func appendClaudeTitleFixturePadding(t *testing.T, path string, bytes int) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := json.NewEncoder(file).Encode(map[string]string{"type": "system", "fixture": strings.Repeat("x", bytes)}); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeHistoryNativeTitleSurvivesMiddleRowsAndRestart(t *testing.T) {
	f := newClaudeActionFixture(t)
	if err := appendActionFixtureTitle(f.path, f.p.id, "Older native title"); err != nil {
		t.Fatal(err)
	}
	appendClaudeTitleFixturePadding(t, f.path, 300<<10)
	if err := appendActionFixtureTitle(f.path, f.p.id, "Latest native title"); err != nil {
		t.Fatal(err)
	}
	appendClaudeTitleFixturePadding(t, f.path, 300<<10)
	if err := appendActionFixtureTitle(f.path, "99999999-9999-4999-8999-999999999999", "Wrong session"); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(f.path)
	info, ok := f.a.hist.summary(f.path, st)
	if !ok || info.Title != "Latest native title" {
		t.Fatalf("middle title lost: %+v", info)
	}
	restarted := newClaudeHistory(f.a.hist.dir)
	if info, ok := restarted.summary(f.path, st); !ok || info.Title != "Latest native title" {
		t.Fatalf("restart lost native title: %+v", info)
	}
	// A replaced transcript must not inherit the previous summary cache even
	// when replacement size and mtime happen to be identical.
	data, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), "Latest native title", "Newest native title", 1))
	if err := os.Rename(f.path, f.path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(f.path, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	current, _ := os.Stat(f.path)
	if info, ok := f.a.hist.summary(f.path, current); !ok || info.Title != "Newest native title" {
		t.Fatalf("replaced file kept old title: %+v", info)
	}
}

func TestClaudeHistoryNativeTitleAcrossReverseReadBoundary(t *testing.T) {
	f := newClaudeActionFixture(t)
	if err := appendActionFixtureTitle(f.path, f.p.id, "跨分块边界的原生标题"); err != nil {
		t.Fatal(err)
	}
	// The final row is just smaller than one read block, splitting the title
	// row between two backwards reads.
	appendClaudeTitleFixturePadding(t, f.path, (64<<10)-100)
	title, err := readClaudeActionTitle(f.path, f.p.id)
	if err != nil || title != "跨分块边界的原生标题" {
		t.Fatalf("split row: %q %v", title, err)
	}
}

func TestClaudeHistoryNativeTitleBoundIsExplicit(t *testing.T) {
	f := newClaudeActionFixture(t)
	if err := appendActionFixtureTitle(f.path, f.p.id, "Must not silently revert"); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(f.path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	err = file.Truncate(claudeNativeTitleScanBytes + 1)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readClaudeActionTitle(f.path, f.p.id); !errors.Is(err, errClaudeTitleUnconfirmed) {
		t.Fatalf("unbounded or silently incomplete title read: %v", err)
	}
	st, _ := os.Stat(f.path)
	info, ok := scanTranscript(f.path, st)
	if !ok || info.Title != "会话标题待确认" {
		t.Fatalf("scan limit silently reverted the title: %+v", info)
	}
}
