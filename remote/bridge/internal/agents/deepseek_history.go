package agents

import (
	"bufio"
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
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/klauspost/compress/zstd"
	"salcara/bridge/internal/protocol"
)

// The SDK stores v4 JSONL under DSH_HOME/sessions, optionally compressed with
// zstd. See @deepseek-ai/dsh-sdk-minimal/cordis.patch.yml and the released v4 codec
// in dsh-session-format-v3-to-v4. This reader never mounts a native runtime or
// asks its writer to repair/migrate a log. Unknown generations fail explicitly.
const deepSeekHistoryBytes = 64 << 20
const deepSeekHistoryLineBytes = 4 << 20
const deepSeekHistoryRows = 100000

var deepSeekGenerationName = regexp.MustCompile(`^session(?:\.v([1-9][0-9]*))?\.jsonl(\.zstd)?$`)

type deepSeekNativeHistory struct {
	events []protocol.Event
	scope  string
	torn   bool
	found  bool
}

// projectKey follows the native UTF-16 path encoding, including its component
// length bound; the final path is still opened relative to a confined os.Root.
func deepSeekProjectKey(cwd string) string {
	var out strings.Builder
	separator := false
	for _, code := range utf16.Encode([]rune(cwd)) {
		if code == '/' || code == '\\' || code == ':' {
			if !separator {
				out.WriteByte('-')
			}
			separator = true
		} else {
			separator = false
			if code >= 'a' && code <= 'z' || code >= 'A' && code <= 'Z' || code >= '0' && code <= '9' || code == '.' || code == '_' || code == '-' {
				out.WriteByte(byte(code))
			} else {
				fmt.Fprintf(&out, "~%04X", code)
			}
		}
	}
	key := strings.TrimLeft(out.String(), "-")
	if key == "" {
		key = "root"
	}
	if len(key) > 251 {
		key = key[:251]
	}
	return "--" + key + "--"
}

func deepSeekHistoryError(detail string) error {
	return errors.New("DeepSeek 原生历史读取失败：" + detail + "；请在电脑检查记录后重试")
}

// The native append lease is unrelated to process-tree termination. A readable
// history (even one ending in turn/end) is never evidence for clearing a fence.
func (a *deepSeekAgent) deepSeekNativeSessionExists(ctx context.Context, id string) (bool, error) {
	a.mu.Lock()
	s := a.sessions[id]
	if s == nil {
		a.mu.Unlock()
		return false, errors.New("DeepSeek Harness 会话不存在")
	}
	info := s.Info
	a.mu.Unlock()
	history, err := a.readDeepSeekNativeHistory(ctx, id, info.Cwd)
	return history.found, err
}

func (a *deepSeekAgent) readDeepSeekNativeHistory(ctx context.Context, id, cwd string) (deepSeekNativeHistory, error) {
	var out deepSeekNativeHistory
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if !validDeepSeekSessionID(id) || !filepath.IsAbs(cwd) || a.stateDir == "" {
		return out, deepSeekHistoryError("会话存储身份无效")
	}
	// SDK initialize resolves an absolute cwd through Node path.resolve before
	// creating the native header and project key. Clean the Bridge spelling by
	// the same lexical rules (separators and . / ..), without following symlinks
	// or making a different directory eligible for this session.
	cwd = filepath.Clean(cwd)
	root, err := os.OpenRoot(a.stateDir)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, deepSeekHistoryError("无法打开会话存储")
	}
	defer root.Close()
	base := filepath.Join(id, "native", "sessions")
	dir := filepath.Join(base, deepSeekProjectKey(cwd), id)
	// Reject redirected components as well as escapes; a native file may not
	// silently turn into a read of another session or a user-selected path.
	part := ""
	for _, component := range strings.Split(filepath.ToSlash(base), "/") {
		part = filepath.Join(part, component)
		st, err := root.Lstat(part)
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return out, deepSeekHistoryError("历史目录不可读取或被重定向")
		}
	}
	projects, err := root.Open(base)
	if err != nil {
		return out, deepSeekHistoryError("无法列出原生项目历史")
	}
	projectNames, projectErr := projects.ReadDir(1025)
	projects.Close()
	if projectErr != nil && !errors.Is(projectErr, io.EOF) || len(projectNames) > 1024 {
		return out, deepSeekHistoryError("原生项目历史目录过多")
	}
	matches := 0
	for _, project := range projectNames {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if project.Name() == deepSeekProjectKey(cwd) && !project.IsDir() {
			return out, deepSeekHistoryError("原生项目目录被重定向")
		}
		if !project.IsDir() {
			continue
		}
		for _, suffix := range []string{".jsonl", ".jsonl.zstd"} {
			if _, err := root.Lstat(filepath.Join(base, project.Name(), id+suffix)); !errors.Is(err, os.ErrNotExist) {
				return out, deepSeekHistoryError("原生历史仍使用旧目录格式，不能当作新会话启动")
			}
		}
		candidate := filepath.Join(base, project.Name(), id)
		entry, err := root.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !entry.IsDir() || entry.Mode()&os.ModeSymlink != 0 || candidate != dir {
			return out, deepSeekHistoryError("原生会话目录与项目不匹配")
		}
		matches++
	}
	if matches == 0 {
		return out, nil
	}
	entries, err := root.Open(dir)
	if err != nil {
		return out, deepSeekHistoryError("无法列出原生历史")
	}
	names, readErr := entries.ReadDir(257)
	entries.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) || len(names) > 256 {
		return out, deepSeekHistoryError("历史目录内容异常")
	}
	filename, version, compressed := "", -1, false
	encoding := ""
	for _, entry := range names {
		match := deepSeekGenerationName.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		if !entry.Type().IsRegular() {
			return out, deepSeekHistoryError("原生历史不是普通文件")
		}
		v := 0
		if match[1] != "" {
			v, err = strconv.Atoi(match[1])
			if err != nil {
				return out, deepSeekHistoryError("历史格式版本无效")
			}
		}
		mode := "plain"
		if match[2] != "" {
			mode = "zstd"
		}
		if encoding != "" && encoding != mode {
			return out, deepSeekHistoryError("存在冲突的历史编码")
		}
		encoding = mode
		if v > version {
			filename, version, compressed = entry.Name(), v, mode == "zstd"
		}
	}
	if filename == "" {
		return out, nil
	}
	if version != 4 {
		return out, deepSeekHistoryError("当前只支持原生 v4 历史格式，请先使用兼容的 DeepSeek Harness 版本")
	}
	file, err := root.Open(filepath.Join(dir, filename))
	if err != nil {
		return out, deepSeekHistoryError("无法打开原生历史文件")
	}
	defer file.Close()
	st, err := file.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > deepSeekHistoryBytes {
		return out, deepSeekHistoryError("历史文件超出读取范围")
	}
	// Snapshot the current byte extent: concurrent native appends belong to the
	// next read. A partial final line is visible as a warning, never repaired.
	var reader io.Reader = io.NewSectionReader(file, 0, st.Size())
	if compressed {
		decoder, err := zstd.NewReader(reader, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(deepSeekHistoryBytes))
		if err != nil {
			return out, deepSeekHistoryError("无法解压原生历史")
		}
		defer decoder.Close()
		reader = decoder
	}
	limited := &io.LimitedReader{R: reader, N: deepSeekHistoryBytes + 1}
	scan := bufio.NewReaderSize(limited, 64<<10)
	line, complete, err := deepSeekReadHistoryLine(scan)
	if err != nil || !complete {
		return out, deepSeekHistoryError("原生历史头不完整")
	}
	var header struct {
		Type      string `json:"type"`
		Version   int    `json:"version"`
		ID        string `json:"id"`
		Cwd       string `json:"cwd"`
		CreatedAt *int64 `json:"createdAt"`
		Seeded    *bool  `json:"isSeeded"`
		Depth     *int   `json:"delegationDepth"`
		Origin    string `json:"origin"`
	}
	if !utf8.Valid(line) || json.Unmarshal(line, &header) != nil || header.Type != "session" || header.Version != version || header.ID != id || !filepath.IsAbs(header.Cwd) || filepath.Clean(header.Cwd) != cwd || header.CreatedAt == nil || *header.CreatedAt < 0 || header.Seeded == nil || header.Depth == nil || *header.Depth != 0 || header.Origin != "" {
		return out, deepSeekHistoryError("原生历史头与本机会话不匹配")
	}
	digest := sha256.Sum256(line)
	out.scope = deepSeekID + ":" + id + ":v4:" + hex.EncodeToString(digest[:])
	out.found = true
	seq := int64(0)
	seedSeen := false
	toolCalls := map[string]protocol.Event{}
	for {
		if err := ctx.Err(); err != nil {
			return deepSeekNativeHistory{}, err
		}
		line, complete, err = deepSeekReadHistoryLine(scan)
		if limited.N <= 0 {
			return deepSeekNativeHistory{}, deepSeekHistoryError("解压后的历史超出读取范围")
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return deepSeekNativeHistory{}, deepSeekHistoryError("原生历史文件损坏或记录过大")
		}
		if !complete {
			out.torn = len(line) != 0
			break
		}
		if seq >= deepSeekHistoryRows {
			return deepSeekNativeHistory{}, deepSeekHistoryError("历史记录超过读取上限")
		}
		var envelope struct {
			Type string          `json:"type"`
			Seq  *int64          `json:"seq"`
			Time *int64          `json:"time"`
			Data json.RawMessage `json:"data"`
		}
		if !utf8.Valid(line) || json.Unmarshal(line, &envelope) != nil || envelope.Type == "" || envelope.Seq == nil || *envelope.Seq != seq || envelope.Time == nil || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
			return deepSeekNativeHistory{}, deepSeekHistoryError("原生历史包含损坏或不连续的记录")
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(envelope.Data, &fields) != nil {
			return deepSeekNativeHistory{}, deepSeekHistoryError("原生事件内容无效")
		}
		if envelope.Type == "session/end-seed" && string(fields["inherited"]) == "true" {
			seedSeen = true
		}
		raw := dshEvent{Type: envelope.Type, Seq: seq, Time: *envelope.Time, Data: envelope.Data}
		if err := deepSeekValidateHistoryEvent(raw); err != nil {
			return deepSeekNativeHistory{}, err
		}
		for _, event := range mapDeepSeekEvent(raw) {
			event.Tool, event.SessionKey = deepSeekID, deepSeekID+":"+id
			if event.Type == "tool" {
				if event.Status == "running" {
					toolCalls[event.ID] = event
				} else if call, ok := toolCalls[event.ID]; ok {
					event.Title, event.Detail = call.Title, call.Detail
					delete(toolCalls, event.ID)
				}
			}
			event.Text, event.Detail, event.Output = truncHead(event.Text, 256<<10), truncHead(event.Detail, 128<<10), truncHead(event.Output, 256<<10)
			event.Title, event.Error = truncHead(event.Title, 4096), truncHead(event.Error, 16<<10)
			out.events = append(out.events, event)
		}
		seq++
	}
	if *header.Seeded != seedSeen {
		return deepSeekNativeHistory{}, deepSeekHistoryError("原生历史的继承边界不完整")
	}
	return out, nil
}

func deepSeekReadHistoryLine(reader *bufio.Reader) ([]byte, bool, error) {
	var line []byte
	for {
		part, err := reader.ReadSlice('\n')
		if len(line)+len(part) > deepSeekHistoryLineBytes {
			return nil, false, errors.New("record too large")
		}
		line = append(line, part...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			return line, false, err
		}
		return line[:len(line)-1], true, nil
	}
}

// Validate the records we render rather than treating a failed conversion as
// an empty message. Unrendered native metadata stays unrendered.
func deepSeekValidateHistoryEvent(raw dshEvent) error {
	var data map[string]json.RawMessage
	if json.Unmarshal(raw.Data, &data) != nil {
		return deepSeekHistoryError("原生事件内容无效")
	}
	message := raw.Data
	role := ""
	switch raw.Type {
	case "user/message":
		role = "user"
	case "assistant/message":
		role, message = "assistant", data["message"]
	case "tool/result":
		role, message = "tool", data["message"]
	case "tool/call":
		var id, name, args string
		if json.Unmarshal(data["callId"], &id) != nil || id == "" || json.Unmarshal(data["name"], &name) != nil || name == "" || json.Unmarshal(data["arguments"], &args) != nil {
			return deepSeekHistoryError("原生工具调用内容无效")
		}
	case "turn/start", "turn/end":
		var turn int64
		if json.Unmarshal(data["turn"], &turn) != nil || turn <= 0 {
			return deepSeekHistoryError("原生任务轮次无效")
		}
	}
	if role == "" {
		return nil
	}
	if role == "user" {
		var source struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(data["source"], &source) != nil || source.Kind == "" {
			return deepSeekHistoryError("原生用户消息来源无效")
		}
	}
	var m struct {
		ID      string            `json:"id"`
		Role    string            `json:"role"`
		Content []json.RawMessage `json:"content"`
		CallID  string            `json:"toolCallId"`
	}
	if json.Unmarshal(message, &m) != nil || m.Role != role || m.Content == nil || role != "tool" && m.ID == "" || role == "tool" && m.CallID == "" {
		return deepSeekHistoryError("原生消息内容无效")
	}
	for _, block := range m.Content {
		var b struct {
			Type string  `json:"type"`
			Text *string `json:"text"`
		}
		if json.Unmarshal(block, &b) != nil || b.Type == "" || (b.Type == "text" || b.Type == "reasoning") && b.Text == nil {
			return deepSeekHistoryError("原生消息内容块无效")
		}
	}
	return nil
}

func (a *deepSeekAgent) OpenPage(ctx context.Context, id, cursor string, limit int) (protocol.SessionInfo, []protocol.Event, string, error) {
	return a.openDeepSeekHistoryPage(ctx, id, cursor, limit, paginateHistory)
}

func (a *deepSeekAgent) OpenMessagesPage(ctx context.Context, id, cursor string, limit int) (protocol.SessionInfo, []protocol.Event, string, error) {
	return a.openDeepSeekHistoryPage(ctx, id, cursor, limit, paginateMessageHistory)
}

func (a *deepSeekAgent) openDeepSeekHistoryPage(ctx context.Context, id, cursor string, limit int, paginate historyPaginator) (protocol.SessionInfo, []protocol.Event, string, error) {
	a.mu.Lock()
	s := a.sessions[id]
	if s == nil {
		a.mu.Unlock()
		return protocol.SessionInfo{}, nil, "", errors.New("DeepSeek Harness 会话不存在")
	}
	cwd := s.Info.Cwd
	a.mu.Unlock()
	history, err := a.readDeepSeekNativeHistory(ctx, id, cwd)
	if err != nil {
		return protocol.SessionInfo{}, nil, "", err
	}
	a.mu.Lock()
	info, started, unconfirmed := s.Info, s.Started, s.Unconfirmed
	cached := append([]protocol.Event(nil), s.Events...)
	pending := make(map[string]bool, len(s.approvals))
	for approval := range s.approvals {
		pending[approval] = true
	}
	a.mu.Unlock()
	seen := make(map[string]bool, len(history.events))
	for _, event := range history.events {
		seen[event.Type+":"+event.ID] = true
	}
	events := history.events
	approvals := []protocol.Event{}
	for _, event := range cached {
		if event.Type == "approval.request" {
			if pending[event.ApprovalID] {
				approvals = append(approvals, event)
			}
			continue
		}
		if event.ID == "" || !seen[event.Type+":"+event.ID] {
			events = append(events, event)
			seen[event.Type+":"+event.ID] = true
		}
	}
	scope := history.scope
	if !history.found {
		scope = info.SessionKey + ":cached"
	}
	page, next, err := paginate(scope, events, cursor, limit)
	if err != nil {
		return info, nil, "", err
	}
	if cursor == "" {
		page = append(page, approvals...)
		notice := func(id, text string) {
			page = append(page, protocol.Event{Tool: deepSeekID, SessionKey: info.SessionKey, Type: "notice", ID: id, Level: "warning", Text: text})
		}
		if !history.found && started {
			notice("deepseek-history-unavailable", "原生持久历史暂不可读取，目前仅显示 Bridge 保留的最近记录；不会将其当作完整历史。")
		}
		if history.torn {
			notice("deepseek-history-incomplete-tail", "原生历史末尾尚未写入完整记录，当前显示已完整保存的内容；不会修复文件或自动重发。")
		}
		if unconfirmed {
			notice("deepseek-runtime-unconfirmed", "此前 DeepSeek 任务的进程退出未确认，旧记录已保留。请在电脑检查原进程；不会自动重发或启动第二个实例。")
		}
	}
	return info, page, next, nil
}
