package agents

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"salcara/bridge/internal/protocol"
)

// claudeHistory reads Claude Code transcripts (~/.claude/projects/<project>/<sessionId>.jsonl), caching
// per-file summaries by size+mtime so listing stays cheap.
type claudeHistory struct {
	dir   string
	mu    sync.Mutex
	cache map[string]*histEntry
	// Process-local provenance only. An old transcript or its age cannot
	// establish remote execution ownership after a Bridge restart.
	ownedLeases map[string]*claudeOwnedLease
}

type histEntry struct {
	file os.FileInfo
	info protocol.SessionInfo
	ok   bool
}

func newClaudeHistory(dir string) *claudeHistory {
	return &claudeHistory{dir: dir, cache: map[string]*histEntry{}}
}

func sortSessions(list []protocol.SessionInfo) {
	sort.SliceStable(list, func(i, j int) bool { return list[i].UpdatedAt > list[j].UpdatedAt })
}

func (h *claudeHistory) files() []string {
	files, _ := h.filesContext(context.Background())
	return files
}

// Resolve directory boundaries once per project, rather than walking every
// ancestor again for every transcript. Links still receive the full check.
func (h *claudeHistory) filesContext(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := filepath.EvalSymlinks(h.dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取 Claude 本地历史目录失败: %w", err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	projects, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("读取 Claude 本地历史目录失败: %w", err)
	}
	var out []string
	for _, project := range projects {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !project.IsDir() && project.Type()&os.ModeSymlink == 0 {
			continue
		}
		dir := filepath.Join(root, project.Name())
		if project.Type()&os.ModeSymlink != 0 {
			dir, err = filepath.EvalSymlinks(dir)
			if err != nil || !claudeHistoryWithin(root, dir) {
				continue
			}
			st, err := os.Stat(dir)
			if err != nil || !st.IsDir() {
				continue
			}
		}
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		} // a project removed during discovery
		if err != nil {
			return nil, fmt.Errorf("读取 Claude 本地项目历史失败: %w", err)
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			name := entry.Name()
			if !strings.HasSuffix(name, ".jsonl") || strings.HasPrefix(name, "agent-") || !validChildID(strings.TrimSuffix(name, ".jsonl")) {
				continue
			}
			p := filepath.Join(dir, name)
			if entry.Type()&os.ModeSymlink != 0 {
				if !h.safeTranscriptPath(p) {
					continue
				}
			} else if !entry.Type().IsRegular() {
				continue
			}
			out = append(out, p)
		}
	}
	return out, nil
}

func claudeHistoryWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func (h *claudeHistory) path(id string) (string, error) {
	if !validChildID(id) {
		return "", errors.New("无效的会话 ID")
	}
	m, _ := filepath.Glob(filepath.Join(h.dir, "*", id+".jsonl"))
	if len(m) == 0 {
		return "", fmt.Errorf("找不到这个会话: %w", os.ErrNotExist)
	}
	if len(m) > 1 { // the newest wins
		sort.Slice(m, func(i, j int) bool { return modTimeOf(m[i]).After(modTimeOf(m[j])) })
	}
	if !h.safeTranscriptPath(m[0]) {
		return "", errors.New("会话文件不在允许的历史目录内")
	}
	return m[0], nil
}

// Resolve links before exposing a transcript, not only for subagent files.
func (h *claudeHistory) safeTranscriptPath(path string) bool {
	root, err := filepath.EvalSymlinks(h.dir)
	if err != nil {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return false
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false
	}
	st, err := os.Stat(resolved)
	return err == nil && st.Mode().IsRegular()
}

func modTimeOf(p string) time.Time {
	st, err := os.Stat(p)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

func (h *claudeHistory) modTime(id string) (time.Time, error) {
	p, err := h.path(id)
	if err != nil {
		return time.Time{}, err
	}
	st, err := os.Stat(p)
	if err != nil {
		return time.Time{}, err
	}
	return st.ModTime(), nil
}

// summary returns the cached SessionInfo of a transcript (status fields are filled by the agent).
func (h *claudeHistory) summary(path string, st os.FileInfo) (protocol.SessionInfo, bool) {
	// Discovery is batched, but revalidate the selected transcript before using
	// even a cached summary: a project can be replaced by a link between reads.
	if !h.safeTranscriptPath(path) {
		return protocol.SessionInfo{}, false
	}
	h.mu.Lock()
	e := h.cache[path]
	h.mu.Unlock()
	if e != nil && sameClaudeTranscript(e.file, st) {
		return e.info, e.ok
	}
	info, ok := scanTranscript(path, st)
	h.mu.Lock()
	if len(h.cache) >= 512 {
		h.cache = map[string]*histEntry{}
	}
	h.cache[path] = &histEntry{file: st, info: info, ok: ok}
	h.mu.Unlock()
	return info, ok
}

func (h *claudeHistory) info(id string) (protocol.SessionInfo, bool) {
	p, err := h.path(id)
	if err != nil {
		return protocol.SessionInfo{}, false
	}
	st, err := os.Stat(p)
	if err != nil {
		return protocol.SessionInfo{}, false
	}
	return h.summary(p, st)
}

func (h *claudeHistory) list() []protocol.SessionInfo {
	list, _ := h.listContext(context.Background())
	return list
}

func (h *claudeHistory) listContext(ctx context.Context) ([]protocol.SessionInfo, error) {
	type fi struct {
		path string
		st   os.FileInfo
	}
	var files []fi
	paths, err := h.filesContext(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if st, err := os.Stat(p); err == nil {
			files = append(files, fi{p, st})
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("读取 Claude 会话信息失败: %w", err)
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].st.ModTime().After(files[j].st.ModTime()) })
	var out []protocol.SessionInfo
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(out) >= maxSessions {
			break
		}
		if info, ok := h.summary(f.path, f.st); ok {
			out = append(out, info)
		}
	}
	return out, ctx.Err()
}

func claudeClient(entrypoint string) string {
	switch {
	case entrypoint == "claude-desktop" || strings.Contains(entrypoint, "desktop"):
		return "Claude Desktop"
	default:
		return "Claude Code"
	}
}

// readLines calls f for each complete line of r (lines can be very long).
func readLines(r io.Reader, f func([]byte) bool) error {
	return readLinesAt(r, 0, func(line []byte, _ int64) bool { return f(line) })
}

// Offsets include empty and malformed rows, and remain absolute for tail reads.
func readLinesAt(r io.Reader, start int64, f func([]byte, int64) bool) error {
	br := bufio.NewReaderSize(r, 1<<20)
	offset := start
	for {
		line, err := ReadBoundedLine(br, 16<<20)
		lineStart := offset
		offset += int64(len(line))
		if len(line) > 1 {
			if !f(line, lineStart) {
				return nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// Source + absolute record position supplies identity, not the peer's text or
// sender. The raw-record digest distinguishes an in-place rewrite. Call only
// after the reader has admitted this local transcript within its own namespace.
func setClaudePeerHistoryID(l *claudeLine, scope, path string, offset int64, raw []byte) {
	if validChildID(l.UUID) || !claudeUserOrigin(l).peer {
		return
	}
	// Full readers include the newline; tail readers have already removed it.
	raw = bytes.TrimSuffix(raw, []byte{'\n'})
	raw = bytes.TrimSuffix(raw, []byte{'\r'})
	record := sha256.Sum256(raw)
	identity := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%x", scope, filepath.Clean(path), offset, record)))
	l.historyRecordID = fmt.Sprintf("peer-history:%x", identity)
}

// firstUserText extracts the typed text of a user transcript line ("" if it isn't a real prompt).
func firstUserText(l *claudeLine) string {
	if l.Type != "user" || l.IsMeta || l.IsCompactSummary || l.IsSidechain || !claudeUserOrigin(l).human {
		return ""
	}
	var msg claudeMessage
	if json.Unmarshal(l.Message, &msg) != nil {
		return ""
	}
	blocks, s := parseBlocks(msg.Content)
	if s == "" {
		for _, b := range blocks {
			if b.Type == "text" {
				s = b.Text
				break
			}
		}
	}
	if !realUserText(s) {
		return ""
	}
	return s
}

// scanTranscript reads the head (title, cwd, entrypoint) and tail (model, latest cwd, AI title).
func scanTranscript(path string, st os.FileInfo) (protocol.SessionInfo, bool) {
	id := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	info := protocol.SessionInfo{SessionKey: "claude:" + id, Tool: "claude", Client: "Claude Code",
		UpdatedAt: st.ModTime().UnixMilli(), Status: "idle", Controllable: true}
	f, err := os.Open(path)
	if err != nil {
		return info, false
	}
	defer f.Close()
	title, entry, aiTitle, summary, customTitle := "", "", "", "", ""
	read := int64(0)
	readLines(io.LimitReader(f, 4<<20), func(line []byte) bool {
		read += int64(len(line))
		var l claudeLine
		if json.Unmarshal(line, &l) != nil {
			return true
		}
		if info.Cwd == "" && l.Cwd != "" {
			info.Cwd = l.Cwd
		}
		if entry == "" && l.Entrypoint != "" {
			entry = l.Entrypoint
		}
		if l.Type == "ai-title" && l.AITitle != "" {
			aiTitle = l.AITitle
		}
		if value := claudeNativeCustomTitle(line, id); value != "" {
			customTitle = value
		}
		if l.Type == "summary" && l.Summary != "" && summary == "" {
			summary = l.Summary
		}
		if title == "" {
			title = firstUserText(&l)
		}
		return title == "" || info.Cwd == "" || entry == ""
	})
	// tail
	tailCustomTitle := false
	const tail = 256 << 10
	off := st.Size() - tail
	if off < 0 {
		off = 0
	}
	if _, err := f.Seek(off, io.SeekStart); err == nil {
		first := off > 0
		readLines(f, func(line []byte) bool {
			if first { // probably a partial line
				first = false
				return true
			}
			var l claudeLine
			if json.Unmarshal(line, &l) != nil {
				return true
			}
			if l.Cwd != "" {
				info.Cwd = l.Cwd
			}
			if l.Type == "ai-title" && l.AITitle != "" {
				aiTitle = l.AITitle
			}
			if value := claudeNativeCustomTitle(line, id); value != "" {
				customTitle = value
				tailCustomTitle = true
			}
			if l.Type == "assistant" && !l.IsSidechain {
				var msg claudeMessage
				if json.Unmarshal(l.Message, &msg) == nil && msg.Model != "" && msg.Model != "<synthetic>" {
					info.Model = msg.Model
				}
			}
			if title == "" {
				title = firstUserText(&l)
			}
			return true
		})
	}
	if !tailCustomTitle {
		// Native rename metadata may have scrolled out of both spans. Read
		// backwards only until the newest title, within a fixed IO/line bound;
		// summary() caches the result by exact file identity/size/mtime.
		value, err := readClaudeNativeTitle(f, st, id)
		if errors.Is(err, errClaudeTitleUnconfirmed) {
			if title == "" && aiTitle == "" && summary == "" && customTitle == "" {
				return info, false
			}
			info.Title, info.Client = "会话标题待确认", claudeClient(entry)
			return info, true
		}
		if err != nil {
			return info, false
		}
		customTitle = value
	}
	switch {
	case customTitle != "":
		info.Title = titleText(customTitle)
	case title != "":
		info.Title = titleText(title)
	case aiTitle != "":
		info.Title = titleText(aiTitle)
	case summary != "":
		info.Title = titleText(summary)
	default:
		return info, false // no conversation in it
	}
	info.Client = claudeClient(entry)
	return info, true
}

// open converts a whole transcript to events (≤400, oldest first).
func (h *claudeHistory) open(id string) (protocol.SessionInfo, []protocol.Event, error) {
	return h.scanHistory(context.Background(), id, maxHistoryEvents)
}

// Pagination reads the local transcript, not the Hub's expiring event cache.
func (h *claudeHistory) openUnbounded(ctx context.Context, id string) (protocol.SessionInfo, []protocol.Event, error) {
	return h.scanHistory(ctx, id, 0)
}
func (h *claudeHistory) scanHistory(ctx context.Context, id string, retain int) (protocol.SessionInfo, []protocol.Event, error) {
	p, err := h.path(id)
	if err != nil {
		return protocol.SessionInfo{}, nil, err
	}
	st, err := os.Stat(p)
	if err != nil {
		return protocol.SessionInfo{}, nil, err
	}
	info, _ := h.summary(p, st)
	if retain == 0 && st.Size() > 64<<20 {
		return info, nil, errors.New("历史文件过大，请在电脑查看")
	}
	f, err := os.Open(p)
	if err != nil {
		return info, nil, err
	}
	defer f.Close()
	m := newClaudeMapper(info.SessionKey, info.Cwd)
	var evs []protocol.Event
	childRefs := map[string]string{}
	mappedBytes := 0
	var scanError error
	readError := readLinesAt(io.LimitReader(f, st.Size()), 0, func(line []byte, offset int64) bool {
		if ctx.Err() != nil {
			scanError = ctx.Err()
			return false
		}
		var l claudeLine
		if json.Unmarshal(line, &l) == nil {
			setClaudePeerHistoryID(&l, info.SessionKey, p, offset, line)
			mapped := m.entry(&l)
			mapped = h.projectSkillReceipt(id, &l, mapped)
			for _, e := range mapped {
				if e.Kind == "subagent" && e.ID != "" {
					if agentID := claudeResultAgentID(e.Output); agentID != "" && len(childRefs) < 64 {
						childRefs[e.ID] = agentID
					}
				}
			}
			evs = append(evs, mapped...)
			if retain > 0 && len(evs) > 4*retain {
				evs = dedupeEvents(evs, 2*retain)
			}
			if retain == 0 {
				encoded, _ := json.Marshal(mapped)
				mappedBytes += len(encoded)
				if mappedBytes > 32<<20 {
					scanError = errors.New("历史内容过大，请在电脑查看")
					return false
				}
			}
		}
		return true
	})
	if readError != nil {
		return info, nil, readError
	}
	if scanError != nil {
		return info, nil, scanError
	}
	evs = append(evs, h.childTranscriptEvents(p, info, childRefs)...)
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].TS < evs[j].TS })
	return info, dedupeEvents(evs, retain), nil
}

// ---- live watching of transcripts written outside the bridge ----

type watchedFile struct {
	offset int64
	mod    time.Time
	status string
	mapper *claudeMapper
}

// watch polls transcript mtimes and emits incremental events for sessions the bridge doesn't run
// (terminal, IDE and Claude Desktop sessions), so they show live on the phone.
func (a *claudeAgent) watch(ctx context.Context, interval time.Duration) {
	state := map[string]*watchedFile{}
	first := true
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		a.watchOnce(state, first)
		first = false
		select {
		case <-ctx.Done():
			return
		case <-a.closeCh:
			return
		case <-tick.C:
		}
	}
}

func (a *claudeAgent) watchOnce(state map[string]*watchedFile, baseline bool) {
	for _, p := range a.hist.files() {
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		id := strings.TrimSuffix(filepath.Base(p), ".jsonl")
		w := state[p]
		if baseline {
			status, _ := a.hist.remoteHistoryState(id, st.ModTime().UnixMilli())
			state[p] = &watchedFile{offset: st.Size(), mod: st.ModTime(), status: status}
			continue
		}
		a.mu.Lock()
		owned := a.ownedLocked(id)
		a.mu.Unlock()
		if w == nil {
			w = &watchedFile{status: "idle"}
			if st.Size() > 1<<20 { // an old big file that just appeared (copied/synced): don't replay it
				w.offset = st.Size()
			}
			state[p] = w
		}
		if st.Size() < w.offset {
			w.offset = st.Size() // truncated / rewritten
		}
		changed := st.Size() != w.offset || !st.ModTime().Equal(w.mod)
		w.mod = st.ModTime()
		if owned {
			w.offset = st.Size() // the bridge streams these itself
			w.status = "idle"
			continue
		}
		info, ok := a.hist.summary(p, st)
		status, controllable := a.hist.remoteHistoryState(id, st.ModTime().UnixMilli())
		if changed && st.Size() > w.offset {
			var evs []protocol.Event
			if w.mapper == nil {
				w.mapper = newClaudeMapper("claude:"+id, info.Cwd)
			}
			w.offset = readFrom(p, w.offset, st.Size(), func(line []byte, offset int64) {
				var l claudeLine
				if json.Unmarshal(line, &l) == nil {
					setClaudePeerHistoryID(&l, "claude:"+id, p, offset, line)
					mapped := a.hist.projectSkillReceipt(id, &l, w.mapper.entry(&l))
					evs = append(evs, mapped...)
				}
			})
			w.mapper.trim()
			if ok && (status != w.status || len(evs) > 0) {
				info.Status, info.Controllable = status, controllable
				a.emit(protocol.Event{SessionKey: info.SessionKey, Type: "session.updated", Session: ptrInfo(info)})
				w.status = status
			}
			if len(evs) > maxHistoryEvents {
				evs = evs[len(evs)-maxHistoryEvents:]
			}
			for _, e := range evs {
				a.emit(e)
			}
			continue
		}
		if ok && status != w.status {
			info.Status, info.Controllable = status, controllable
			a.emit(protocol.Event{SessionKey: info.SessionKey, Type: "session.updated", Session: ptrInfo(info)})
			w.status = status
		}
	}
}

func activityStatus(mod time.Time) string {
	if time.Since(mod) < externalActive {
		return "running"
	}
	return "idle"
}

// readFrom reads complete lines in [from, to) and returns the offset after the last complete line.
func readFrom(path string, from, to int64, f func([]byte, int64)) int64 {
	file, err := os.Open(path)
	if err != nil {
		return from
	}
	defer file.Close()
	if _, err := file.Seek(from, io.SeekStart); err != nil {
		return from
	}
	br := bufio.NewReaderSize(io.LimitReader(file, to-from), 1<<20)
	off := from
	for {
		line, err := br.ReadBytes('\n')
		if err != nil { // incomplete last line: keep it for next time
			return off
		}
		f(line, off)
		off += int64(len(line))
	}
}
