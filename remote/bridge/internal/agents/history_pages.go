package agents

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"salcara/bridge/internal/protocol"
)

// Optional adapters leave older Agent implementations and their defaults intact.
type SessionPager interface {
	SessionsPage(context.Context, string, int, string) ([]protocol.SessionInfo, string, error)
}
type HistoryPager interface {
	OpenPage(context.Context, string, string, int) (protocol.SessionInfo, []protocol.Event, string, error)
}
type MessageHistoryPager interface {
	OpenMessagesPage(context.Context, string, string, int) (protocol.SessionInfo, []protocol.Event, string, error)
}
type historyPaginator func(string, []protocol.Event, string, int) ([]protocol.Event, string, error)

const historyPageBytes = 512 << 10

func pageLimit(limit, max int) int {
	if limit <= 0 || limit > max {
		return max
	}
	return limit
}
func cursorValue(value any) string {
	raw, _ := json.Marshal(value)
	return base64.RawURLEncoding.EncodeToString(raw)
}
func readCursor(value string, target any) error {
	if len(value) > 4096 {
		return errors.New("读取位置无效，请重新打开")
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || json.Unmarshal(raw, target) != nil {
		return errors.New("读取位置无效，请重新打开")
	}
	return nil
}

// Cursor identities exclude streamed content/updated timestamps. Newer appends
// cannot shift the boundary; if a transcript was compacted, fail rather than skip.
func eventAnchor(event protocol.Event, index int) string {
	if event.ID != "" || event.ApprovalID != "" {
		index = -1
	}
	raw, _ := json.Marshal([]any{event.Type, event.ID, event.TurnID, event.ApprovalID, event.Role, event.ParentID, index})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

type historyCursor struct {
	Session string `json:"s"`
	Anchor  string `json:"a"`
}

func paginateHistory(key string, events []protocol.Event, cursor string, limit int) ([]protocol.Event, string, error) {
	return paginateHistoryUnit(key, events, cursor, limit, false)
}

// Count user/assistant records, not tool/status events. Preserve the associated
// events within the existing event/byte budget and the same append-stable cursor.
func paginateMessageHistory(key string, events []protocol.Event, cursor string, limit int) ([]protocol.Event, string, error) {
	return paginateHistoryUnit(key, events, cursor, limit, true)
}

// PaginateMessageHistory shares the exact message/tool budget with the native
// desktop adapter. The scope must include its lease identity, not only a title.
func PaginateMessageHistory(scope string, events []protocol.Event, cursor string, limit int) ([]protocol.Event, string, error) {
	return paginateMessageHistory(scope, events, cursor, limit)
}

func paginateHistoryUnit(key string, events []protocol.Event, cursor string, limit int, messages bool) ([]protocol.Event, string, error) {
	events = dedupeEvents(events, 0)
	end := len(events)
	if cursor != "" {
		var c historyCursor
		if readCursor(cursor, &c) != nil || c.Session != key || len(c.Anchor) != 64 {
			return nil, "", errors.New("历史读取位置不匹配，请刷新")
		}
		end = -1
		for index, event := range events {
			if eventAnchor(event, index) == c.Anchor {
				end = index
				break
			}
		}
		if end < 0 {
			return nil, "", errors.New("电脑历史已变化，请刷新后重新加载")
		}
	}
	limit = pageLimit(limit, maxHistoryEvents)
	if messages {
		limit = pageLimit(limit, 10)
	}
	start, bytes, count := end, 0, 0
	for start > 0 && count < limit && end-start < maxHistoryEvents {
		encoded, err := json.Marshal(events[start-1])
		if err != nil {
			return nil, "", err
		}
		if bytes+len(encoded) > historyPageBytes {
			if start == end {
				return nil, "", errors.New("这一条历史过大，请在电脑查看")
			}
			break
		}
		bytes += len(encoded)
		start--
		if !messages || events[start].Type == "message" && events[start].ParentID == "" && (events[start].Role == "user" || events[start].Role == "assistant") {
			count++
		}
	}
	next := ""
	if start > 0 {
		next = cursorValue(historyCursor{Session: key, Anchor: eventAnchor(events[start], start)})
	}
	page := append([]protocol.Event{}, events[start:end]...)
	return page, next, nil
}

type codexListCursor struct {
	Tool string `json:"t"`
	Next string `json:"n"`
}

func (a *codexAgent) SessionsPage(ctx context.Context, cursor string, limit int, client string) ([]protocol.SessionInfo, string, error) {
	if client != "" {
		return nil, "", errors.New("Codex 会话类型无效")
	}
	nativeCursor := ""
	if cursor != "" {
		var c codexListCursor
		if readCursor(cursor, &c) != nil || c.Tool != "codex" || c.Next == "" {
			return nil, "", errors.New("会话读取位置无效")
		}
		nativeCursor = c.Next
	}
	connection, err := a.ensure(ctx)
	if err != nil {
		return nil, "", err
	}
	var result struct {
		Data       []cxThread `json:"data"`
		NextCursor string     `json:"nextCursor"`
	}
	limit = pageLimit(limit, maxSessions)
	params := map[string]any{"limit": limit, "sortKey": "updated_at", "modelProviders": []string{}, "sourceKinds": codexSourceKinds}
	if nativeCursor != "" {
		params["cursor"] = nativeCursor
	}
	if err := connection.Call(ctx, "thread/list", params, &result); err != nil {
		// Compatibility fallback never silently drops the requested cursor.
		var rpc *rpcError
		if !errors.As(err, &rpc) {
			return nil, "", err
		}
		delete(params, "sortKey")
		delete(params, "sourceKinds")
		if err := connection.Call(ctx, "thread/list", params, &result); err != nil {
			return nil, "", err
		}
	}
	if len(result.Data) > limit {
		return nil, "", errors.New("电脑返回的会话数量超过这一页的上限，请刷新")
	}
	a.mu.Lock()
	out := []protocol.SessionInfo{}
	for _, thread := range result.Data {
		if thread.Ephemeral {
			continue
		}
		item := a.applyThreadLocked(thread)
		out = append(out, item.info)
	}
	a.mu.Unlock()
	// A paginated authoritative directory must not append every previously
	// opened thread: that bypasses the requested page size and its native cursor.
	// Cached/live threads remain intact and the unpaged Sessions API is unchanged.
	sortSessions(out)
	next := ""
	if result.NextCursor != "" {
		next = cursorValue(codexListCursor{Tool: "codex", Next: result.NextCursor})
	}
	return out, next, nil
}
func (a *codexAgent) OpenPage(ctx context.Context, id, cursor string, limit int) (protocol.SessionInfo, []protocol.Event, string, error) {
	return a.openHistoryPage(ctx, id, cursor, limit, paginateHistory)
}
func (a *codexAgent) OpenMessagesPage(ctx context.Context, id, cursor string, limit int) (protocol.SessionInfo, []protocol.Event, string, error) {
	return a.openCodexMessagesPage(ctx, id, cursor, limit)
}
func (a *codexAgent) openHistoryPage(ctx context.Context, id, cursor string, limit int, paginate historyPaginator) (protocol.SessionInfo, []protocol.Event, string, error) {
	thread, err := a.readThread(ctx, id)
	if err != nil {
		return protocol.SessionInfo{}, nil, "", err
	}
	a.mu.Lock()
	item := a.applyThreadLocked(thread)
	item.opened = true
	item.tailHistory = false
	item.seen = map[string]bool{}
	for _, key := range historyKeys(thread) {
		item.seen[key] = true
	}
	info := item.info
	a.mu.Unlock()
	events, next, err := paginate(info.SessionKey, codexHistory(info.SessionKey, thread), cursor, limit)
	if err != nil {
		return info, nil, "", err
	}
	if cursor == "" {
		pending := a.aps.snapshot(info.SessionKey)
		if len(pending) > 0 {
			info.Status = "waiting_approval"
		}
		events = append(events, pending...)
	}
	return info, events, next, nil
}

type claudeListCursor struct {
	Tool    string `json:"t"`
	Client  string `json:"c"`
	Updated int64  `json:"u"`
	Key     string `json:"k"`
}

func (a *claudeAgent) SessionsPage(ctx context.Context, cursor string, limit int, client string) ([]protocol.SessionInfo, string, error) {
	if client != "" && client != "code" && client != "desktop-code" {
		return nil, "", errors.New("Claude 会话类型无效")
	}
	var boundary claudeListCursor
	if cursor != "" && (readCursor(cursor, &boundary) != nil || boundary.Tool != "claude" || boundary.Client != client || !strings.HasPrefix(boundary.Key, "claude:")) {
		return nil, "", errors.New("会话读取位置无效")
	}
	limit = pageLimit(limit, maxSessions)
	matches := func(info protocol.SessionInfo) bool {
		if client == "desktop-code" && info.Client != "Claude Desktop" || client == "code" && info.Client == "Claude Desktop" {
			return false
		}
		return cursor == "" || info.UpdatedAt < boundary.Updated || info.UpdatedAt == boundary.Updated && info.SessionKey > boundary.Key
	}
	less := func(left, right protocol.SessionInfo) bool {
		if left.UpdatedAt == right.UpdatedAt {
			return left.SessionKey < right.SessionKey
		}
		return left.UpdatedAt > right.UpdatedAt
	}
	// Keep only one page (+lookahead) in memory while scanning summary metadata.
	out, seen := []protocol.SessionInfo{}, map[string]bool{}
	add := func(info protocol.SessionInfo) {
		if !matches(info) || seen[info.SessionKey] {
			return
		}
		seen[info.SessionKey] = true
		out = append(out, info)
		sort.Slice(out, func(i, j int) bool { return less(out[i], out[j]) })
		if len(out) > limit+1 {
			removed := out[len(out)-1]
			delete(seen, removed.SessionKey)
			out = out[:limit+1]
		}
	}
	type canonicalFile struct {
		path string
		stat os.FileInfo
	}
	canonical := map[string]canonicalFile{}
	for _, path := range a.hist.files() {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		stat, err := os.Stat(path)
		if err != nil {
			continue
		}
		id := filepath.Base(path)
		if prior, ok := canonical[id]; !ok || stat.ModTime().After(prior.stat.ModTime()) {
			canonical[id] = canonicalFile{path, stat}
		}
	}
	files := make([]canonicalFile, 0, len(canonical))
	for _, file := range canonical {
		files = append(files, file)
	}
	sort.Slice(files, func(i, j int) bool {
		left, right := files[i].stat.ModTime().UnixMilli(), files[j].stat.ModTime().UnixMilli()
		if left == right {
			return filepath.Base(files[i].path) < filepath.Base(files[j].path)
		}
		return left > right
	})
	// Directory metadata determines recency. Read content summaries only until
	// this page plus one matching lookahead is known; older transcripts must not
	// all be scanned before the first ten rows can be returned.
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		metadata := protocol.SessionInfo{SessionKey: "claude:" + strings.TrimSuffix(filepath.Base(file.path), ".jsonl"), UpdatedAt: file.stat.ModTime().UnixMilli()}
		if cursor != "" && (metadata.UpdatedAt > boundary.Updated || metadata.UpdatedAt == boundary.Updated && metadata.SessionKey <= boundary.Key) {
			continue
		}
		path, stat := file.path, file.stat
		if info, ok := a.hist.summary(path, stat); ok {
			a.mu.Lock()
			a.decorateLocked(strings.TrimPrefix(info.SessionKey, "claude:"), &info)
			a.mu.Unlock()
			add(info)
		}
		if len(out) == limit+1 {
			break
		}
	}
	a.mu.Lock()
	for id, info := range a.live {
		a.decorateLocked(id, &info)
		add(info)
	}
	a.mu.Unlock()
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = cursorValue(claudeListCursor{Tool: "claude", Client: client, Updated: last.UpdatedAt, Key: last.SessionKey})
	}
	return out, next, nil
}
func (a *claudeAgent) OpenPage(ctx context.Context, id, cursor string, limit int) (protocol.SessionInfo, []protocol.Event, string, error) {
	return a.openHistoryPage(ctx, id, cursor, limit, paginateHistory)
}
func (a *claudeAgent) OpenMessagesPage(ctx context.Context, id, cursor string, limit int) (protocol.SessionInfo, []protocol.Event, string, error) {
	info, events, next, err := a.hist.openTailPage(ctx, id, cursor, limit)
	if errors.Is(err, os.ErrNotExist) {
		return a.openHistoryPage(ctx, id, cursor, limit, paginateMessageHistory)
	}
	if err != nil {
		return info, nil, "", err
	}
	a.mu.Lock()
	a.decorateLocked(id, &info)
	a.mu.Unlock()
	if cursor == "" {
		pending := a.aps.snapshot(info.SessionKey)
		if len(pending) > 0 {
			info.Status = "waiting_approval"
		}
		events = append(events, pending...)
	}
	return info, events, next, nil
}
func (a *claudeAgent) openHistoryPage(ctx context.Context, id, cursor string, limit int, paginate historyPaginator) (protocol.SessionInfo, []protocol.Event, string, error) {
	if err := ctx.Err(); err != nil {
		return protocol.SessionInfo{}, nil, "", err
	}
	info, all, err := a.hist.openUnbounded(ctx, id)
	if err != nil {
		// A newly-created live session may not have written its first transcript
		// yet. Read limits, cancellation and other failures are not empty history.
		if !errors.Is(err, os.ErrNotExist) || !validChildID(id) {
			return info, nil, "", err
		}
		if contextError := ctx.Err(); contextError != nil {
			return info, nil, "", contextError
		}
		a.mu.Lock()
		live, ok := a.live[id]
		a.mu.Unlock()
		if !ok || live.SessionKey != "claude:"+id || live.Tool != "claude" {
			return info, nil, "", err
		}
		info, all = live, nil
	}
	a.mu.Lock()
	a.decorateLocked(id, &info)
	a.mu.Unlock()
	events, next, err := paginate(info.SessionKey, all, cursor, limit)
	if err != nil {
		return info, nil, "", err
	}
	if cursor == "" {
		pending := a.aps.snapshot(info.SessionKey)
		if len(pending) > 0 {
			info.Status = "waiting_approval"
		}
		events = append(events, pending...)
	}
	return info, events, next, nil
}
