package agents

// Read-only adapter for the local Chat/Cowork persistence schema in official
// Claude Desktop 2.16120.0 (Windows MSIX 2.16120.0.0). This is not Claude Code
// history and must never be passed to `claude --resume` or claimed as a native
// desktop message transport. Only an explicitly authorized account/org root is
// accepted; this adapter never discovers accounts, credentials, or cloud chats.
//
// Static distribution evidence: .vite/build/index.chunk-2nuxP6dD.js Pp,
// loadSessions, getSessionFilePath, describeSessionStorage, gm and
// resolveTranscriptFilePath; .vite/build/index.chunk-Dp-z0Dv3.js OD/l9t/kD.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"salcara/bridge/internal/protocol"
)

const ClaudeDesktopHistorySchemaVersion = "2.16120.0"

// claudeDesktopHistoryVersionCompatible accepts the verified build and later
// builds of the same major line. Later builds are read in compatibility mode:
// the record decoder still requires every identity/time field and rejects
// nulls, duplicates and invalid IDs, so a changed format fails closed instead
// of being misread. (Kept local to avoid an agents -> toolcfg import cycle.)
func claudeDesktopHistoryVersionCompatible(v string) bool {
	parse := func(s string) ([3]int, bool) {
		var out [3]int
		parts := strings.Split(strings.TrimSpace(s), ".")
		if len(parts) < 3 || len(parts) > 4 {
			return out, false
		}
		for i, part := range parts {
			if part == "" || len(part) > 9 {
				return out, false
			}
			n := 0
			for _, c := range part {
				if c < '0' || c > '9' {
					return out, false
				}
				n = n*10 + int(c-'0')
			}
			if i < 3 {
				out[i] = n
			}
		}
		return out, true
	}
	got, ok := parse(v)
	base, _ := parse(ClaudeDesktopHistorySchemaVersion)
	if !ok || got[0] != base[0] {
		return false
	}
	for i := 1; i < 3; i++ {
		if got[i] != base[i] {
			return got[i] > base[i]
		}
	}
	return true
}

const (
	claudeDesktopRecordBytes      = 1 << 20
	claudeDesktopMetadataBytes    = 16 << 20
	claudeDesktopDirectoryEntries = 8192
	claudeDesktopProjectEntries   = 512
	claudeDesktopTranscriptBytes  = 64 << 20
	claudeDesktopMappedBytes      = 32 << 20
)

var (
	claudeDesktopLocalID = regexp.MustCompile(`^local_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	claudeDesktopUUID    = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	claudeDesktopShortID = regexp.MustCompile(`^[0-9a-f]{8}$`)
)

type ClaudeDesktopHistory struct {
	root, scopeID string
}

// HistoryIdentity is an opaque namespace fingerprint, never an account ID,
// credential, or local path. Phones invalidate their cache on any scope change.
func (h *ClaudeDesktopHistory) HistoryIdentity() string { return h.scopeID }

// NewClaudeDesktopHistory accepts one already-authorized directory of the form
// userData/local-agent-mode-sessions/<account>/<org>. The full and shortened
// account/org IDs are both native layouts. The caller must establish the current
// identity first; scanning all accounts is intentionally not an API here.
func NewClaudeDesktopHistory(accountOrgRoot, desktopVersion string) (*ClaudeDesktopHistory, error) {
	if !claudeDesktopHistoryVersionCompatible(desktopVersion) {
		return nil, errors.New("Claude Desktop 版本不在支持范围内，未读取本地历史")
	}
	if !filepath.IsAbs(accountOrgRoot) {
		return nil, errors.New("Claude Desktop 历史目录需要绝对路径")
	}
	root := filepath.Clean(accountOrgRoot)
	validScope := func(id string) bool { return claudeDesktopUUID.MatchString(id) || claudeDesktopShortID.MatchString(id) }
	if !validScope(filepath.Base(root)) || !validScope(filepath.Base(filepath.Dir(root))) || filepath.Base(filepath.Dir(filepath.Dir(root))) != "local-agent-mode-sessions" {
		return nil, errors.New("Claude Desktop 历史目录必须限定到一个账号和组织")
	}
	h := &ClaudeDesktopHistory{root: root}
	if _, err := h.checkedPath(root, true); err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(root))
	h.scopeID = hex.EncodeToString(sum[:])
	return h, nil
}

// checkedPath denies links and junctions, including links to another account
// within the broader app tree. EvalSymlinks also handles Windows reparse points.
// Paths are checked again before opening; no files or directories are created.
func (h *ClaudeDesktopHistory) checkedPath(path string, directory bool) (os.FileInfo, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, errors.New("Claude Desktop 历史路径无效")
	}
	abs = filepath.Clean(abs)
	rel, err := filepath.Rel(h.root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return nil, errors.New("Claude Desktop 历史路径不在授权目录内")
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return nil, err
	}
	same := filepath.Clean(resolved) == abs
	if runtime.GOOS == "windows" {
		same = strings.EqualFold(filepath.Clean(resolved), abs)
	}
	if !same {
		return nil, errors.New("Claude Desktop 历史路径存在链接，未读取")
	}
	st, err := os.Lstat(abs)
	if err != nil {
		return nil, err
	}
	if st.Mode()&os.ModeSymlink != 0 || directory && !st.IsDir() || !directory && !st.Mode().IsRegular() {
		return nil, errors.New("Claude Desktop 历史文件类型无效")
	}
	return st, nil
}

// Check the opened descriptor as well as its path. A directory or file swapped
// between discovery and opening must not redirect a bounded read elsewhere.
func (h *ClaudeDesktopHistory) openChecked(path string, directory bool) (*os.File, os.FileInfo, error) {
	before, err := h.checkedPath(path, directory)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	opened, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	after, err := h.checkedPath(path, directory)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !os.SameFile(before, opened) || !os.SameFile(opened, after) {
		f.Close()
		return nil, nil, errors.New("Claude Desktop 历史路径读取期间已变化，请重试")
	}
	return f, opened, nil
}

func (h *ClaudeDesktopHistory) directory(ctx context.Context, path string, limit int) ([]os.DirEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, _, err := h.openChecked(path, true)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var entries []os.DirEntry
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		batch, err := f.ReadDir(128)
		if len(entries)+len(batch) > limit {
			return nil, errors.New("Claude Desktop 历史目录超过读取上限")
		}
		entries = append(entries, batch...)
		if errors.Is(err, io.EOF) {
			return entries, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// Pointer fields preserve required/optional type checking. Unrecognized native
// fields (which may include credentials or policies) are ignored, never exported.
type claudeDesktopRecord struct {
	SessionID      *string  `json:"sessionId"`
	ProcessName    *string  `json:"processName"`
	Cwd            *string  `json:"cwd"`
	CreatedAt      *float64 `json:"createdAt"`
	LastActivityAt *float64 `json:"lastActivityAt"`
	CLISessionID   *string  `json:"cliSessionId"`
	Model          *string  `json:"model"`
	Title          *string  `json:"title"`
	SessionType    *string  `json:"sessionType"`
	IsArchived     *bool    `json:"isArchived"`
	IsStarred      *bool    `json:"isStarred"`
}

func (r claudeDesktopRecord) kind() string {
	if r.SessionType == nil {
		return "desktop-cowork"
	}
	switch *r.SessionType {
	case "chat":
		return "desktop-chat"
	case "agent", "dispatch_child", "radar":
		return "hidden"
	default:
		return "desktop-cowork"
	}
}

func desktopHistoryString(s string, limit int) bool {
	if len(s) > limit || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return false
		}
	}
	return true
}

func decodeClaudeDesktopRecord(data []byte, id string) (claudeDesktopRecord, error) {
	var r claudeDesktopRecord
	// Native optional scalar fields accept absence, not JSON null. Duplicate
	// top-level fields cannot safely establish one record identity either.
	decoder := json.NewDecoder(bytes.NewReader(data))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return r, errors.New("Claude Desktop 会话记录不是 JSON 对象")
	}
	seen := map[string]bool{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		key, ok := keyToken.(string)
		if err != nil || !ok || seen[key] {
			return r, errors.New("Claude Desktop 会话记录字段重复或无效")
		}
		seen[key] = true
		var raw json.RawMessage
		if decoder.Decode(&raw) != nil {
			return r, errors.New("Claude Desktop 会话记录字段无效")
		}
		switch key {
		case "sessionId", "processName", "cwd", "createdAt", "lastActivityAt", "cliSessionId", "model", "title", "sessionType", "isArchived", "isStarred":
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				return r, errors.New("Claude Desktop 会话记录字段类型无效")
			}
		}
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
		return r, errors.New("Claude Desktop 会话记录字段无效")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return r, errors.New("Claude Desktop 会话记录含额外内容")
	}
	if json.Unmarshal(data, &r) != nil || r.SessionID == nil || *r.SessionID != id || !claudeDesktopLocalID.MatchString(id) || r.ProcessName == nil || r.Cwd == nil || r.CreatedAt == nil || r.LastActivityAt == nil {
		return r, errors.New("Claude Desktop 会话记录格式无法验证")
	}
	for _, t := range []*float64{r.CreatedAt, r.LastActivityAt} {
		if *t < 0 || *t > 1<<53-1 {
			return r, errors.New("Claude Desktop 会话时间无效")
		}
	}
	if !desktopHistoryString(*r.ProcessName, 512) || !desktopHistoryString(*r.Cwd, 32768) {
		return r, errors.New("Claude Desktop 会话元数据无效")
	}
	for _, s := range []*string{r.Title, r.Model, r.SessionType} {
		if s != nil && !desktopHistoryString(*s, 65536) {
			return r, errors.New("Claude Desktop 会话元数据无效")
		}
	}
	if r.CLISessionID != nil && *r.CLISessionID != "" && !claudeDesktopUUID.MatchString(*r.CLISessionID) {
		return r, errors.New("Claude Desktop 转录编号无效")
	}
	return r, nil
}

func (h *ClaudeDesktopHistory) records(ctx context.Context) (map[string]claudeDesktopRecord, error) {
	result := map[string]claudeDesktopRecord{}
	budget := int64(claudeDesktopMetadataBytes)
	entryBudget := claudeDesktopDirectoryEntries
	for _, dir := range []string{h.root, filepath.Join(h.root, "agent")} {
		entries, err := h.directory(ctx, dir, entryBudget)
		if err != nil {
			if dir != h.root && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		entryBudget -= len(entries)
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			name := entry.Name()
			id := strings.TrimSuffix(name, ".json")
			if name != id+".json" || !claudeDesktopLocalID.MatchString(id) {
				continue
			}
			path := filepath.Join(dir, name)
			f, st, err := h.openChecked(path, false)
			if err != nil {
				return nil, err
			}
			if st.Size() > claudeDesktopRecordBytes || st.Size() > budget {
				f.Close()
				return nil, errors.New("Claude Desktop 会话元数据超过读取上限")
			}
			data, readErr := io.ReadAll(io.LimitReader(f, min(int64(claudeDesktopRecordBytes), budget)+1))
			f.Close()
			if readErr != nil {
				return nil, readErr
			}
			if len(data) > claudeDesktopRecordBytes || int64(len(data)) > budget {
				return nil, errors.New("Claude Desktop 会话元数据超过读取上限")
			}
			budget -= int64(len(data))
			r, err := decodeClaudeDesktopRecord(data, id)
			if err != nil {
				return nil, err
			}
			if dir != h.root && (r.SessionType == nil || *r.SessionType != "agent") {
				return nil, errors.New("Claude Desktop 隐藏会话目录格式无法验证")
			}
			if _, duplicate := result[id]; duplicate {
				return nil, errors.New("Claude Desktop 会话记录编号重复，未读取")
			}
			result[id] = r
		}
	}
	return result, nil
}

func claudeDesktopInfo(record claudeDesktopRecord) protocol.SessionInfo {
	info := protocol.SessionInfo{SessionKey: "claude-desktop:" + *record.SessionID, Tool: "claude", Client: "Claude Desktop", ControlSurface: "read-only", Controllable: false, Status: "idle", Cwd: *record.Cwd, UpdatedAt: int64(*record.LastActivityAt), Title: "未命名会话"}
	info.SessionScope = record.kind()
	if record.Title != nil && strings.TrimSpace(*record.Title) != "" {
		info.Title = titleText(*record.Title)
	}
	if record.Model != nil {
		info.Model = truncHead(*record.Model, 200)
	}
	return info
}

// Describe resolves a native local_UUID to its current sidebar category using
// metadata only. It never opens a transcript or exports a CLI resumable ID.
// A cold phone focus can therefore establish its explicit Chat/Cowork scope
// before asking for history, without guessing or scanning paginated directories.
func (h *ClaudeDesktopHistory) Describe(ctx context.Context, id string) (protocol.SessionInfo, error) {
	if !claudeDesktopLocalID.MatchString(id) {
		return protocol.SessionInfo{}, errors.New("Claude Desktop 会话编号无效")
	}
	records, err := h.records(ctx)
	if err != nil {
		return protocol.SessionInfo{}, err
	}
	record, ok := records[id]
	if !ok || record.kind() == "hidden" || record.IsArchived != nil && *record.IsArchived {
		return protocol.SessionInfo{}, fmt.Errorf("找不到这个 Claude Desktop 会话: %w", os.ErrNotExist)
	}
	return claudeDesktopInfo(record), nil
}

type claudeDesktopListCursor struct {
	ScopeID string `json:"r"`
	Scope   string `json:"s"`
	Updated int64  `json:"u"`
	Key     string `json:"k"`
}

// SessionsPage exposes native Chat/Cowork records only. Hidden agent/dispatch/
// radar records and archived records are not sidebar entries. Persisted mtimes
// cannot prove that the desktop runtime is currently executing a task.
func (h *ClaudeDesktopHistory) SessionsPage(ctx context.Context, cursor string, limit int, scope string) ([]protocol.SessionInfo, string, error) {
	if scope != "" && scope != "desktop-chat" && scope != "desktop-cowork" {
		return nil, "", errors.New("Claude Desktop 会话类型无效")
	}
	var boundary claudeDesktopListCursor
	if cursor != "" && (readCursor(cursor, &boundary) != nil || boundary.ScopeID != h.scopeID || boundary.Scope != scope || !strings.HasPrefix(boundary.Key, "claude-desktop:local_")) {
		return nil, "", errors.New("Claude Desktop 会话读取位置不匹配")
	}
	records, err := h.records(ctx)
	if err != nil {
		return nil, "", err
	}
	limit = pageLimit(limit, maxSessions)
	out := []protocol.SessionInfo{}
	for _, record := range records {
		if record.kind() == "hidden" || scope != "" && record.kind() != scope || record.IsArchived != nil && *record.IsArchived {
			continue
		}
		info := claudeDesktopInfo(record)
		if cursor != "" && !(info.UpdatedAt < boundary.Updated || info.UpdatedAt == boundary.Updated && info.SessionKey > boundary.Key) {
			continue
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt == out[j].UpdatedAt {
			return out[i].SessionKey < out[j].SessionKey
		}
		return out[i].UpdatedAt > out[j].UpdatedAt
	})
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = cursorValue(claudeDesktopListCursor{ScopeID: h.scopeID, Scope: scope, Updated: last.UpdatedAt, Key: last.SessionKey})
	}
	return out, next, nil
}

func (h *ClaudeDesktopHistory) transcriptPath(ctx context.Context, record claudeDesktopRecord, records map[string]claudeDesktopRecord) (string, error) {
	if record.CLISessionID == nil || *record.CLISessionID == "" {
		return "", fmt.Errorf("Claude Desktop 尚未保存本地转录: %w", os.ErrNotExist)
	}
	id := *record.SessionID
	short := id[len("local_") : len("local_")+8]
	fullDir, shortDir := filepath.Join(h.root, id), filepath.Join(h.root, short)
	_, fullErr := h.checkedPath(fullDir, true)
	_, shortErr := h.checkedPath(shortDir, true)
	if fullErr != nil && !errors.Is(fullErr, os.ErrNotExist) {
		return "", fullErr
	}
	if shortErr != nil && !errors.Is(shortErr, os.ErrNotExist) {
		return "", shortErr
	}
	if fullErr == nil && shortErr == nil {
		return "", errors.New("Claude Desktop 完整和缩短会话目录同时存在，未读取")
	}
	dir := fullDir
	if fullErr != nil {
		if shortErr != nil {
			return "", fullErr
		}
		for other := range records {
			if other != id && other[len("local_"):len("local_")+8] == short {
				return "", errors.New("Claude Desktop 缩短会话目录编号冲突，未读取")
			}
		}
		dir = shortDir
	}
	projects := filepath.Join(dir, ".claude", "projects")
	entries, err := h.directory(ctx, projects, claudeDesktopProjectEntries)
	if err != nil {
		return "", err
	}
	var found string
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
			continue
		}
		project := filepath.Join(projects, entry.Name())
		if _, err := h.checkedPath(project, true); err != nil {
			return "", err
		}
		candidate := filepath.Join(project, *record.CLISessionID+".jsonl")
		if _, err := h.checkedPath(candidate, false); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return "", err
		}
		if found != "" {
			return "", errors.New("Claude Desktop 本地转录编号重复，未读取")
		}
		found = candidate
	}
	if found == "" {
		return "", fmt.Errorf("找不到 Claude Desktop 本地转录: %w", os.ErrNotExist)
	}
	return found, nil
}

// OpenPage uses the native local_UUID identity, never the CLI UUID. The mapper
// reads the same JSONL message shape, but events stay under a distinct desktop
// key and no pending approvals or controllable leases are manufactured.
func (h *ClaudeDesktopHistory) OpenPage(ctx context.Context, id, cursor string, limit int) (protocol.SessionInfo, []protocol.Event, string, error) {
	return h.openHistoryPage(ctx, id, cursor, limit, paginateHistory)
}
func (h *ClaudeDesktopHistory) OpenMessagesPage(ctx context.Context, id, cursor string, limit int) (protocol.SessionInfo, []protocol.Event, string, error) {
	return h.openHistoryPage(ctx, id, cursor, limit, paginateMessageHistory, true)
}
func (h *ClaudeDesktopHistory) openHistoryPage(ctx context.Context, id, cursor string, limit int, paginate historyPaginator, messages ...bool) (protocol.SessionInfo, []protocol.Event, string, error) {
	if !claudeDesktopLocalID.MatchString(id) {
		return protocol.SessionInfo{}, nil, "", errors.New("Claude Desktop 会话编号无效")
	}
	records, err := h.records(ctx)
	if err != nil {
		return protocol.SessionInfo{}, nil, "", err
	}
	record, ok := records[id]
	if !ok || record.kind() == "hidden" || record.IsArchived != nil && *record.IsArchived {
		return protocol.SessionInfo{}, nil, "", fmt.Errorf("找不到这个 Claude Desktop 会话: %w", os.ErrNotExist)
	}
	info := claudeDesktopInfo(record)
	path, err := h.transcriptPath(ctx, record, records)
	if err != nil {
		return info, nil, "", err
	}
	f, st, err := h.openChecked(path, false)
	if err != nil {
		return info, nil, "", err
	}
	defer f.Close()
	if len(messages) > 0 && messages[0] {
		events, next, err := readClaudeTail(ctx, f, st, h.scopeID+":"+info.SessionKey, info, cursor, limit, *record.CLISessionID, true)
		return info, events, next, err
	}
	if st.Size() > claudeDesktopTranscriptBytes {
		return info, nil, "", errors.New("历史文件过大，请在电脑查看")
	}
	mapper := newClaudeMapper(info.SessionKey, info.Cwd)
	var all []protocol.Event
	mappedBytes := 0
	lineIndex := 0
	var scanError error
	readError := readLines(io.LimitReader(f, st.Size()), func(line []byte) bool {
		lineIndex++
		if err := ctx.Err(); err != nil {
			scanError = err
			return false
		}
		var value claudeLine
		if json.Unmarshal(line, &value) != nil {
			return true
		}
		for _, transcriptID := range []string{value.SessionID, value.SessionIDT} {
			if transcriptID != "" && !strings.EqualFold(transcriptID, *record.CLISessionID) {
				scanError = errors.New("Claude Desktop 转录与会话编号不匹配")
				return false
			}
		}
		if value.UUID == "" {
			hash := sha256.Sum256(line)
			value.UUID = fmt.Sprintf("desktop-line-%d-%x", lineIndex, hash[:])
		}
		if value.Timestamp == "" {
			value.Timestamp = "1970-01-01T00:00:00Z"
		}
		mapped := mapper.entry(&value)
		encoded, _ := json.Marshal(mapped)
		mappedBytes += len(encoded)
		if mappedBytes > claudeDesktopMappedBytes {
			scanError = errors.New("历史内容过大，请在电脑查看")
			return false
		}
		all = append(all, mapped...)
		return true
	})
	if readError != nil {
		return info, nil, "", readError
	}
	if scanError != nil {
		return info, nil, "", scanError
	}
	// Scope-bind history cursors as well as directory cursors. An imported local
	// UUID may be present under more than one independently authorized account.
	pageKey := h.scopeID + ":" + info.SessionKey
	events, next, err := paginate(pageKey, all, cursor, limit)
	return info, events, next, err
}
