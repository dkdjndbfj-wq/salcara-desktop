// Package localusage totals the token use that Codex and Claude Code record in
// their own local session files, for the desktop 用量 page when a relay does
// not report daily or per-model statistics. It only reads usage counters,
// model names and timestamps; message text is never kept or returned.
package localusage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Day is the total of one local calendar day.
type Day struct {
	Date     string `json:"date"`
	Tokens   int64  `json:"total_tokens"`
	Input    int64  `json:"input_tokens"`
	Output   int64  `json:"output_tokens"`
	Requests int64  `json:"requests"`
}

// Model is the total of one model over the window.
type Model struct {
	Model    string `json:"model"`
	Tokens   int64  `json:"total_tokens"`
	Requests int64  `json:"requests"`
}

// Result is the window's usage across both tools.
type Result struct {
	Days    []Day    `json:"daily_usage"`
	Models  []Model  `json:"model_stats"`
	Today   Day      `json:"today"`
	Partial bool     `json:"partial"` // the scan budget ran out; older files were skipped
	Tools   []string `json:"tools"`
}

type key struct{ date, model string }
type agg struct{ tokens, in, out, requests int64 }
type fileAgg struct {
	size   int64
	mtime  time.Time
	offset int64  // bytes of complete lines already parsed
	state  parser // carries what later lines need (message ids, running totals)
	data   map[key]agg
}

// parser consumes complete lines in file order; files only ever grow, so a
// changed file is continued from where the last scan stopped.
type parser interface {
	line([]byte)
	totals() map[key]agg
}

// Scanner keeps per-file totals so an unchanged file is never read twice.
type Scanner struct {
	ClaudeHome string // ~/.claude or $CLAUDE_CONFIG_DIR
	CodexHome  string // ~/.codex or $CODEX_HOME
	// Budget bounds the bytes read from new or changed files per call.
	Budget int64

	mu    sync.Mutex
	cache map[string]fileAgg
}

// Default resolves the tools' own data directories like the tools do.
func Default() *Scanner {
	home, _ := os.UserHomeDir()
	claude := os.Getenv("CLAUDE_CONFIG_DIR")
	if claude == "" {
		claude = filepath.Join(home, ".claude")
	}
	codex := os.Getenv("CODEX_HOME")
	if codex == "" {
		codex = filepath.Join(home, ".codex")
	}
	return &Scanner{ClaudeHome: claude, CodexHome: codex, Budget: 512 << 20}
}

// Scan totals the last `days` local days (including today).
func (s *Scanner) Scan(ctx context.Context, days int, now time.Time) (Result, error) {
	if days < 1 {
		days = 1
	}
	if days > 90 {
		days = 90
	}
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -(days - 1))
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache == nil {
		s.cache = map[string]fileAgg{}
	}
	budget := s.Budget
	if budget <= 0 {
		budget = 512 << 20
	}
	totals := map[key]agg{}
	var res Result
	type root struct {
		dir, tool string
		fresh     func() parser
		want      []byte
	}
	roots := []root{
		{filepath.Join(s.ClaudeHome, "projects"), "Claude Code", func() parser { return &claudeParser{byID: map[string]claudeLast{}} }, []byte(`"usage"`)},
		{filepath.Join(s.CodexHome, "sessions"), "Codex", func() parser { return &codexParser{out: map[key]agg{}, model: "codex"} }, []byte(`"payload"`)},
		{filepath.Join(s.CodexHome, "archived_sessions"), "Codex", func() parser { return &codexParser{out: map[key]agg{}, model: "codex"} }, []byte(`"payload"`)},
	}
	seen := map[string]bool{}
	for _, r := range roots {
		var files []string
		_ = filepath.WalkDir(r.dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || ctx.Err() != nil {
				return nil
			}
			if d.Type()&fs.ModeSymlink != 0 {
				return nil
			}
			if !d.IsDir() && strings.HasSuffix(d.Name(), ".jsonl") {
				if info, e := d.Info(); e == nil && !info.ModTime().Before(start) {
					files = append(files, path)
				}
			}
			return nil
		})
		// Newest first, so a spent budget skips the oldest files.
		sort.Slice(files, func(i, j int) bool {
			a, _ := os.Stat(files[i])
			b, _ := os.Stat(files[j])
			return a != nil && b != nil && a.ModTime().After(b.ModTime())
		})
		used := false
		for _, path := range files {
			if err := ctx.Err(); err != nil {
				return res, err
			}
			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			cached, ok := s.cache[path]
			if !ok || cached.size != info.Size() || !cached.mtime.Equal(info.ModTime()) {
				// Continue an appended file; start over if it shrank or is new.
				if !ok || info.Size() < cached.offset {
					cached = fileAgg{state: r.fresh()}
				}
				if info.Size()-cached.offset > budget {
					res.Partial = true
					continue
				}
				f, err := os.Open(path)
				if err != nil {
					continue
				}
				consumed, err := readLines(f, cached.offset, r.want, cached.state.line)
				f.Close()
				if err != nil {
					continue
				}
				budget -= consumed
				cached.offset += consumed
				cached.size, cached.mtime = info.Size(), info.ModTime()
				cached.data = cached.state.totals()
				s.cache[path] = cached
			}
			for k, v := range cached.data {
				if k.date < start.Format("2006-01-02") {
					continue
				}
				t := totals[k]
				t.tokens, t.in, t.out, t.requests = t.tokens+v.tokens, t.in+v.in, t.out+v.out, t.requests+v.requests
				totals[k] = t
				used = true
			}
		}
		if used && !seen[r.tool] {
			seen[r.tool] = true
			res.Tools = append(res.Tools, r.tool)
		}
	}
	byDay := map[string]*Day{}
	byModel := map[string]*Model{}
	for k, v := range totals {
		d := byDay[k.date]
		if d == nil {
			d = &Day{Date: k.date}
			byDay[k.date] = d
		}
		d.Tokens += v.tokens
		d.Input += v.in
		d.Output += v.out
		d.Requests += v.requests
		m := byModel[k.model]
		if m == nil {
			m = &Model{Model: k.model}
			byModel[k.model] = m
		}
		m.Tokens += v.tokens
		m.Requests += v.requests
	}
	for _, d := range byDay {
		res.Days = append(res.Days, *d)
	}
	sort.Slice(res.Days, func(i, j int) bool { return res.Days[i].Date < res.Days[j].Date })
	for _, m := range byModel {
		res.Models = append(res.Models, *m)
	}
	sort.Slice(res.Models, func(i, j int) bool { return res.Models[i].Tokens > res.Models[j].Tokens })
	today := now.Format("2006-01-02")
	if d := byDay[today]; d != nil {
		res.Today = *d
	} else {
		res.Today = Day{Date: today}
	}
	return res, nil
}

func localDate(ts string) (string, bool) {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return "", false
	}
	return t.Local().Format("2006-01-02"), true
}

// readLines feeds complete lines from offset on and returns the bytes consumed
// (a trailing line still being written is left for the next scan).
func readLines(f *os.File, offset int64, want []byte, fn func([]byte)) (int64, error) {
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, err
	}
	r := bufio.NewReaderSize(f, 256<<10)
	var consumed int64
	for {
		line, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			// Overlong line (a large tool output): skip it in pieces.
			n := int64(len(line))
			for errors.Is(err, bufio.ErrBufferFull) {
				line, err = r.ReadSlice('\n')
				n += int64(len(line))
			}
			if err != nil {
				return consumed, nil
			}
			consumed += n
			continue
		}
		if err != nil {
			return consumed, nil // EOF: an unterminated last line waits for the next scan
		}
		consumed += int64(len(line))
		if bytes.Contains(line, want) {
			fn(line)
		}
	}
}

// Claude Code: one assistant record per streamed chunk, sharing message.id;
// the usage block of the last chunk is final.
type claudeLast struct {
	k       key
	in, out int64
}
type claudeParser struct {
	byID  map[string]claudeLast
	order []string
}

func (p *claudeParser) line(line []byte) {
	var r struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Message   struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Usage *struct {
				Input       int64 `json:"input_tokens"`
				CacheCreate int64 `json:"cache_creation_input_tokens"`
				CacheRead   int64 `json:"cache_read_input_tokens"`
				Output      int64 `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &r) != nil || r.Type != "assistant" || r.Message.Usage == nil || r.Message.Model == "" || r.Message.Model == "<synthetic>" {
		return
	}
	date, ok := localDate(r.Timestamp)
	if !ok {
		return
	}
	u := r.Message.Usage
	id := r.Message.ID
	if id == "" {
		id = r.Timestamp
	}
	if _, ok := p.byID[id]; !ok {
		p.order = append(p.order, id)
	}
	p.byID[id] = claudeLast{k: key{date, r.Message.Model}, in: u.Input + u.CacheCreate + u.CacheRead, out: u.Output}
}

func (p *claudeParser) totals() map[key]agg {
	out := map[key]agg{}
	for _, id := range p.order {
		v := p.byID[id]
		a := out[v.k]
		a.in += v.in
		a.out += v.out
		a.tokens += v.in + v.out
		a.requests++
		out[v.k] = a
	}
	return out
}

// Codex: token_count events carry a running total for the session; count the
// increase so repeated events (rate-limit refreshes) are never double counted.
type codexUsage struct {
	Input  int64 `json:"input_tokens"`
	Output int64 `json:"output_tokens"`
	Total  int64 `json:"total_tokens"`
}
type codexParser struct {
	out   map[key]agg
	model string
	prev  codexUsage
}

func (p *codexParser) line(line []byte) {
	var r struct {
		Timestamp string `json:"timestamp"`
		Type      string `json:"type"`
		Payload   struct {
			Type  string `json:"type"`
			Model string `json:"model"`
			Info  *struct {
				Total codexUsage `json:"total_token_usage"`
			} `json:"info"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &r) != nil {
		return
	}
	if r.Type == "turn_context" && r.Payload.Model != "" {
		p.model = r.Payload.Model
		return
	}
	if r.Type != "event_msg" || r.Payload.Type != "token_count" || r.Payload.Info == nil {
		return
	}
	cur := r.Payload.Info.Total
	if cur.Total == 0 {
		cur.Total = cur.Input + cur.Output
	}
	if cur.Total <= p.prev.Total {
		if cur.Total < p.prev.Total {
			p.prev = cur // a new session counter inside the same file
		}
		return
	}
	date, ok := localDate(r.Timestamp)
	if !ok {
		return
	}
	k := key{date, p.model}
	a := p.out[k]
	a.tokens += cur.Total - p.prev.Total
	a.in += max(0, cur.Input-p.prev.Input)
	a.out += max(0, cur.Output-p.prev.Output)
	a.requests++
	p.out[k] = a
	p.prev = cur
}

func (p *codexParser) totals() map[key]agg {
	out := make(map[key]agg, len(p.out))
	for k, v := range p.out {
		out[k] = v
	}
	return out
}
