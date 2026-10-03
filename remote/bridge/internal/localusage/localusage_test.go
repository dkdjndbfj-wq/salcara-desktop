package localusage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestScanTotalsBothToolsWithoutDoubleCounting(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	ts := now.UTC().Format(time.RFC3339Nano)
	old := now.AddDate(0, 0, -40).UTC().Format(time.RFC3339Nano)
	claude := filepath.Join(root, "claude")
	codex := filepath.Join(root, "codex")
	write(t, filepath.Join(claude, "projects", "p", "a.jsonl"),
		`{"type":"user","timestamp":"`+ts+`","message":{"content":"secret prompt text"}}`,
		// two streamed chunks of the same message: the last usage counts once
		`{"type":"assistant","timestamp":"`+ts+`","message":{"id":"m1","model":"claude-sonnet-4-5","usage":{"input_tokens":10,"cache_read_input_tokens":90,"output_tokens":1}}}`,
		`{"type":"assistant","timestamp":"`+ts+`","message":{"id":"m1","model":"claude-sonnet-4-5","usage":{"input_tokens":10,"cache_read_input_tokens":90,"output_tokens":50}}}`,
		`{"type":"assistant","timestamp":"`+ts+`","message":{"id":"m2","model":"<synthetic>","usage":{"input_tokens":0,"output_tokens":0}}}`,
		`{"type":"assistant","timestamp":"`+old+`","message":{"id":"m0","model":"claude-sonnet-4-5","usage":{"input_tokens":999,"output_tokens":999}}}`)
	write(t, filepath.Join(codex, "sessions", "2026", "10", "03", "rollout-x.jsonl"),
		`{"timestamp":"`+ts+`","type":"turn_context","payload":{"model":"gpt-5-codex"}}`,
		`{"timestamp":"`+ts+`","type":"event_msg","payload":{"type":"token_count","info":null}}`,
		`{"timestamp":"`+ts+`","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1000,"output_tokens":200,"total_tokens":1200}}}}`,
		`{"timestamp":"`+ts+`","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1000,"output_tokens":200,"total_tokens":1200}}}}`,
		`{"timestamp":"`+ts+`","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1500,"output_tokens":300,"total_tokens":1800}}}}`)
	s := &Scanner{ClaudeHome: claude, CodexHome: codex, Budget: 1 << 20}
	r, err := s.Scan(context.Background(), 7, now)
	if err != nil {
		t.Fatal(err)
	}
	if r.Today.Tokens != 150+1800 || r.Today.Requests != 3 {
		t.Fatalf("today: %+v", r.Today)
	}
	if len(r.Models) != 2 || r.Models[0].Model != "gpt-5-codex" || r.Models[0].Tokens != 1800 || r.Models[1].Tokens != 150 {
		t.Fatalf("models: %+v", r.Models)
	}
	if len(r.Days) != 1 || len(r.Tools) != 2 {
		t.Fatalf("days/tools: %+v %+v", r.Days, r.Tools)
	}
	// Cached files are not re-read; a changed file is.
	again, _ := s.Scan(context.Background(), 7, now)
	if again.Today.Tokens != r.Today.Tokens {
		t.Fatal("cached rescan changed totals")
	}
}

func TestScanRespectsBudget(t *testing.T) {
	root := t.TempDir()
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	write(t, filepath.Join(root, "c", "projects", "p", "a.jsonl"),
		`{"type":"assistant","timestamp":"`+ts+`","message":{"id":"m1","model":"claude-x","usage":{"input_tokens":1,"output_tokens":1}}}`)
	s := &Scanner{ClaudeHome: filepath.Join(root, "c"), CodexHome: filepath.Join(root, "x"), Budget: 10}
	r, err := s.Scan(context.Background(), 1, time.Now())
	if err != nil || !r.Partial || r.Today.Tokens != 0 {
		t.Fatalf("budget not respected: %+v %v", r, err)
	}
}

func TestAppendedFileIsContinuedNotRecounted(t *testing.T) {
	root := t.TempDir()
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	path := filepath.Join(root, "x", "sessions", "r.jsonl")
	write(t, path,
		`{"timestamp":"`+ts+`","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"output_tokens":10,"total_tokens":110}}}}`)
	s := &Scanner{ClaudeHome: filepath.Join(root, "c"), CodexHome: filepath.Join(root, "x"), Budget: 1 << 20}
	r1, _ := s.Scan(context.Background(), 1, time.Now())
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(`{"timestamp":"` + ts + `","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":150,"output_tokens":20,"total_tokens":170}}}}` + "\n" + `{"timestamp":"` + ts + `","type":"event_msg","payload":{"type":"tok`)
	f.Close()
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(path, future, future)
	r2, _ := s.Scan(context.Background(), 1, time.Now())
	if r1.Today.Tokens != 110 || r2.Today.Tokens != 170 || r2.Today.Requests != 2 {
		t.Fatalf("incremental scan: %d then %d (%d requests)", r1.Today.Tokens, r2.Today.Tokens, r2.Today.Requests)
	}
}
