package agents

import (
	"encoding/json"
	"os"
	"strings"
	"time"

	"salcara/bridge/internal/protocol"
)

// claudeLine is one line of Claude Code's stream-json output or of a ~/.claude/projects/*/*.jsonl
// transcript (both carry the same message objects).
type claudeLine struct {
	Type           string          `json:"type"`
	Subtype        string          `json:"subtype"`
	SessionID      string          `json:"session_id"`
	Message        json.RawMessage `json:"message"`
	Event          json.RawMessage `json:"event"`
	ParentTU       *string         `json:"parent_tool_use_id"`
	AgentID        string          `json:"agent_id"`
	Origin         json.RawMessage `json:"origin"`
	ToolUseResult  json.RawMessage `json:"tool_use_result"`
	ToolUseResultT json.RawMessage `json:"toolUseResult"`
	TaskID         string          `json:"task_id"`
	ToolUseID      string          `json:"tool_use_id"`
	TaskType       string          `json:"task_type"`
	Description    string          `json:"description"`
	LastToolName   string          `json:"last_tool_name"`
	Status         string          `json:"status"`
	Patch          map[string]any  `json:"patch"`
	Response       json.RawMessage `json:"response"`
	PermissionMode string          `json:"permissionMode"`

	// stream-json init / result
	Model          string          `json:"model"`
	Cwd            string          `json:"cwd"`
	IsError        bool            `json:"is_error"`
	Result         string          `json:"result"`
	TotalCostUSD   float64         `json:"total_cost_usd"`
	DurationMS     *int64          `json:"duration_ms"`
	TerminalReason string          `json:"terminal_reason"`
	APIErrorStatus int             `json:"api_error_status"`
	RetryError     json.RawMessage `json:"error"`
	RetryAttempt   int             `json:"attempt"`
	RetryMax       int             `json:"max_retries"`
	RetryDelayMS   int64           `json:"retry_delay_ms"`
	RetryStatus    int             `json:"error_status"`
	Usage          *struct {
		DurationMS          int64 `json:"duration_ms"`
		InputTokens         int64 `json:"input_tokens"`
		OutputTokens        int64 `json:"output_tokens"`
		CacheReadTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
	Errors []string `json:"errors"`

	// transcript fields
	UUID             string `json:"uuid"`
	Timestamp        string `json:"timestamp"`
	IsSidechain      bool   `json:"isSidechain"`
	IsMeta           bool   `json:"isMeta"`
	IsCompactSummary bool   `json:"isCompactSummary"`
	Entrypoint       string `json:"entrypoint"`
	SessionIDT       string `json:"sessionId"`
	AITitle          string `json:"aiTitle"`
	Summary          string `json:"summary"`
	// Assigned only by verified local transcript readers, never by stream JSON.
	historyRecordID string `json:"-"`
}

type claudeMessage struct {
	ID      string          `json:"id"`
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
}

type claudeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	Source    struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		Path      string `json:"path"`
	} `json:"source"`
}

func (l *claudeLine) ts() int64 {
	if l.Timestamp != "" {
		if t, err := time.Parse(time.RFC3339Nano, l.Timestamp); err == nil {
			return t.UnixMilli()
		}
	}
	return nowMs()
}

func parseBlocks(raw json.RawMessage) ([]claudeBlock, string) {
	if len(raw) == 0 {
		return nil, ""
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return nil, s
	}
	var blocks []claudeBlock
	_ = json.Unmarshal(raw, &blocks)
	return blocks, ""
}

func claudeImageData(blocks []claudeBlock) []string {
	result := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Type != "image" || block.Source.Type != "base64" {
			continue
		}
		data := strings.TrimSpace(block.Source.Data)
		// Keep the adapter bounded; hubclient performs the final strict decode
		// and magic-byte check before accepting any bytes.
		if len(data) >= 16 && len(data) <= 12<<20 {
			result = append(result, data)
		}
	}
	return result
}

// realUserText reports whether a user text is something the person typed (not command wrappers,
// injected reminders or caveats).
func realUserText(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return false
	}
	for _, p := range []string{"<command-name>", "<command-message>", "<command-args>", "<local-command-stdout>",
		"<local-command-stderr>", "<local-command-caveat>", "<system-reminder>", "<bash-stdout>", "<bash-stderr>",
		"<user-memory-input>", "<task-notification>", "Caveat: The messages below", "[Request interrupted"} {
		if strings.HasPrefix(t, p) {
			return false
		}
	}
	return true
}

const maxClaudePeerTextChars = 64 << 10

// The SDK's structured origin is a display category, never proof of the
// sender's identity or permission to address another session. Only documented
// fromSession may be retained as unverified navigation. Addresses, process IDs
// and arbitrary upstream fields never leave this adapter.
type claudeUserProvenance struct {
	human         bool
	peer          bool
	body          *string
	sourceSession string
}

func claudeUserOrigin(l *claudeLine) claudeUserProvenance {
	raw := strings.TrimSpace(string(l.Origin))
	if raw == "" || raw == "null" {
		return claudeUserProvenance{human: true} // older CLI / unattributed input
	}
	var origin struct {
		Kind        string          `json:"kind"`
		Subkind     string          `json:"subkind"`
		Body        json.RawMessage `json:"body"`
		FromSession string          `json:"fromSession"`
	}
	if json.Unmarshal(l.Origin, &origin) != nil {
		return claudeUserProvenance{}
	}
	if origin.Kind == "human" {
		return claudeUserProvenance{human: true}
	}
	if origin.Kind != "peer" && !(origin.Kind == "task-notification" && origin.Subkind == "peer-send-message") {
		return claudeUserProvenance{}
	}
	result := claudeUserProvenance{peer: true}
	// SDK fromSession is a navigation hint, explicitly sender-asserted.
	if origin.Kind == "peer" && protocol.ValidCooperationSessionKey("claude:"+origin.FromSession) {
		result.sourceSession = "claude:" + origin.FromSession
	}
	// Only peer.body is documented as the decoded, byte-exact model input.
	// Other kinds retain the supplied content without parsing text envelopes.
	if origin.Kind == "peer" && len(origin.Body) != 0 && string(origin.Body) != "null" {
		var body string
		if json.Unmarshal(origin.Body, &body) != nil {
			return claudeUserProvenance{}
		}
		result.body = &body
	}
	return result
}

type claudeToolUse struct {
	name  string
	input map[string]any
	ev    protocol.Event
}

// claudeMapper turns Claude messages into timeline events. Block ids are "<message id>:<block index>" so
// streaming partials (stream_event, indexed per message) and the final assistant lines (one block per
// line, same message id) land on the same event id.
type claudeMapper struct {
	sk          string
	cwd         string
	parentID    string
	children    map[string]*claudeMapper
	taskTools   map[string]string
	activeTasks map[string]bool
	blockIdx    map[string]int
	tools       map[string]*claudeToolUse
	checklist   *claudeTaskChecklist

	// streaming state
	streamMsg  string
	streamKind map[int]string
	streamText map[int]string
}

func newClaudeMapper(sk, cwd string) *claudeMapper {
	return &claudeMapper{sk: sk, cwd: cwd, blockIdx: map[string]int{}, tools: map[string]*claudeToolUse{},
		children: map[string]*claudeMapper{}, taskTools: map[string]string{}, activeTasks: map[string]bool{},
		streamKind: map[int]string{}, streamText: map[int]string{}}
}

func (m *claudeMapper) base(ts int64) protocol.Event {
	return protocol.Event{SessionKey: m.sk, Tool: "claude", TS: ts, ParentID: m.parentID}
}

func (m *claudeMapper) trim() {
	if len(m.tools) > 4000 {
		m.tools = map[string]*claudeToolUse{}
	}
	if len(m.blockIdx) > 4000 {
		m.blockIdx = map[string]int{}
	}
}

// assistant maps one assistant message (final blocks).
func (m *claudeMapper) assistant(raw json.RawMessage, fallbackID string, ts int64, source ...*claudeLine) []protocol.Event {
	var msg claudeMessage
	if json.Unmarshal(raw, &msg) != nil {
		return nil
	}
	mid := msg.ID
	if mid == "" {
		mid = fallbackID
	}
	blocks, text := parseBlocks(msg.Content)
	if text != "" {
		blocks = []claudeBlock{{Type: "text", Text: text}}
	}
	m.trim()
	var out []protocol.Event
	for _, b := range blocks {
		idx := m.blockIdx[mid]
		m.blockIdx[mid] = idx + 1
		id := mid + ":" + itoa(idx)
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) == "" {
				continue
			}
			e := m.base(ts)
			e.Type, e.ID, e.Role, e.Text, e.Final = "message", id, "assistant", b.Text, true
			out = append(out, e)
		case "thinking":
			if strings.TrimSpace(b.Thinking) == "" {
				continue
			}
			e := m.base(ts)
			e.Type, e.ID, e.Text, e.Final = "reasoning", id, b.Thinking, true
			out = append(out, e)
		case "image":
			e := m.base(ts)
			e.Type, e.ID, e.Role, e.Text, e.Final = "message", id, "assistant", "[图片]", true
			e.ImageData = claudeImageData([]claudeBlock{b})
			out = append(out, e)
		case "tool_use", "server_tool_use":
			var input map[string]any
			_ = json.Unmarshal(b.Input, &input)
			e := m.base(ts)
			e.Type, e.ID, e.Status = "tool", b.ID, "running"
			e.Computer = computerActivityForTool("", b.Name, b.Input)
			d := describeClaudeTool(b.Name, input, m.cwd, false)
			e.Kind, e.Title, e.Detail, e.Diff = d.kind, d.title, d.detail, d.diff
			if b.Name == "ExitPlanMode" && strings.TrimSpace(inStr(input, "plan")) != "" {
				e.Kind, e.PlanKind, e.Output, e.Detail = "plan", "proposal", truncHead(inStr(input, "plan"), maxOutputChars), ""
			}
			if d.kind == "plan" && e.PlanKind == "" {
				e.PlanKind = "checklist"
				e.Output = d.detail
				e.Detail = ""
				e.Status = d.status
			}
			if claudeChecklistTool(b.Name) {
				if len(source) == 0 || m.checklistSourceMatches(source[0]) {
					out = append(out, m.beginChecklistTool(b.ID, b.Name, input, e)...)
				}
				continue
			}
			m.tools[b.ID] = &claudeToolUse{name: b.Name, input: input, ev: e}
			out = append(out, e)
		}
	}
	return out
}

func toolResultText(raw json.RawMessage) string {
	blocks, s := parseBlocks(raw)
	if s != "" || len(blocks) == 0 {
		return s
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "image":
			parts = append(parts, "[图片]")
		}
	}
	return strings.Join(parts, "\n")
}

// user maps a user message: typed text and tool results. userTextID is used for the typed text.
func (m *claudeMapper) user(l *claudeLine, userTextID string, ts int64) []protocol.Event {
	var msg claudeMessage
	if json.Unmarshal(l.Message, &msg) != nil {
		return nil
	}
	blocks, text := parseBlocks(msg.Content)
	structuredResult := claudeChecklistPayload(l, blocks)
	var evs []protocol.Event
	var texts []string
	if text != "" {
		texts = append(texts, text)
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			texts = append(texts, b.Text)
		case "image":
			texts = append(texts, "[图片]")
		case "tool_result":
			if taskEvents, handled := m.checklistResult(l, b, structuredResult, ts); handled {
				evs = append(evs, taskEvents...)
			} else {
				evs = append(evs, m.toolResult(b, ts))
			}
		}
	}
	joined := strings.TrimSpace(strings.Join(texts, "\n"))
	origin := claudeUserOrigin(l)
	if origin.body != nil {
		joined = *origin.body
	}
	switch {
	case l.IsMeta || l.IsCompactSummary || l.IsSidechain && m.parentID == "" || strings.TrimSpace(joined) == "" || !origin.human && !origin.peer:
	case origin.peer:
		e := m.base(ts)
		id := newID("peer-live:")
		if validChildID(l.UUID) {
			id = "peer:" + l.UUID
		} else if l.historyRecordID != "" {
			id = l.historyRecordID
		}
		e.Type, e.ID, e.Role, e.Text, e.Final = "message", id, "user", truncHead(joined, maxClaudePeerTextChars), true
		e.MessageOrigin = &protocol.MessageOrigin{Kind: "delegation"}
		if m.parentID == "" && origin.sourceSession != "" && origin.sourceSession != m.sk {
			e.MessageOrigin.SourceSessionKey, e.MessageOrigin.Verification = origin.sourceSession, "native_reported"
		}
		evs = append([]protocol.Event{e}, evs...)
	case strings.HasPrefix(joined, "[Request interrupted"):
		e := m.base(ts)
		e.Type, e.Status = "turn", "interrupted"
		evs = append(evs, e)
	case realUserText(joined):
		e := m.base(ts)
		e.Type, e.ID, e.Role, e.Text, e.Final = "message", userTextID, "user", joined, true
		e.ImageData = claudeImageData(blocks)
		evs = append([]protocol.Event{e}, evs...)
	}
	return evs
}

func (m *claudeMapper) toolResult(b claudeBlock, ts int64) protocol.Event {
	var e protocol.Event
	var tu *claudeToolUse
	if tu = m.tools[b.ToolUseID]; tu != nil {
		e = tu.ev
		if e.Kind != "subagent" {
			delete(m.tools, b.ToolUseID)
		}
	} else {
		e = m.base(ts)
		e.Type, e.ID, e.Kind, e.Title = "tool", b.ToolUseID, "other", "工具调用"
	}
	e.TS = ts
	e.Status = "done"
	if b.IsError {
		e.Status = "failed"
	}
	preserveTaskResult := false
	if e.Kind == "subagent" {
		// A lifecycle terminal update is authoritative even when a delayed,
		// successful or failed launch acknowledgement arrives afterward.
		if tu != nil && (tu.ev.Status == "done" || tu.ev.Status == "failed" || tu.ev.Status == "interrupted") {
			e.Status = tu.ev.Status
			preserveTaskResult = true
		} else if !b.IsError {
			background := false
			explicitForeground := false
			if tu != nil {
				background, _ = tu.input["run_in_background"].(bool)
				v, exists := tu.input["run_in_background"].(bool)
				explicitForeground = exists && !v
			}
			if background || !explicitForeground && m.activeTasks[b.ToolUseID] {
				e.Status = "running"
			}
		}
	}
	text := toolResultText(b.Content)
	switch {
	case preserveTaskResult:
		// A delayed launch acknowledgement cannot replace the actual task result.
	case e.PlanKind == "proposal":
		// Native exit acknowledgement must not replace the reviewable plan body.
	case e.Kind == "plan" && !b.IsError:
		// TodoWrite's tool result is just an acknowledgement; retain the typed checklist.
		if tu != nil {
			e.Status = tu.ev.Status
		}
	case e.Kind == "read":
		e.Output = truncHead(text, maxOutputChars)
	case e.Kind == "file_change" && !b.IsError:
		// the diff says it all
	default:
		e.Output = truncTail(text, maxOutputChars)
	}
	if tu != nil && e.Kind == "subagent" {
		tu.ev = e
		if e.Status != "running" {
			delete(m.activeTasks, b.ToolUseID)
		}
	}
	if blocks, _ := parseBlocks(b.Content); len(blocks) > 0 {
		e.ImageData = claudeImageData(blocks)
	}
	if tu != nil && !b.IsError && e.Status == "done" {
		e.Browser = browserNavigation("", tu.name, tu.input, b.Content)
	}
	return e
}

// stream maps a stream_event (Anthropic SSE event) to partial message/reasoning events.
func (m *claudeMapper) stream(raw json.RawMessage) []protocol.Event {
	var ev struct {
		Type    string `json:"type"`
		Index   int    `json:"index"`
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
		ContentBlock struct {
			Type string `json:"type"`
		} `json:"content_block"`
		Delta struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"delta"`
	}
	if json.Unmarshal(raw, &ev) != nil {
		return nil
	}
	switch ev.Type {
	case "message_start":
		m.streamMsg = ev.Message.ID
		m.streamKind = map[int]string{}
		m.streamText = map[int]string{}
	case "content_block_start":
		m.streamKind[ev.Index] = ev.ContentBlock.Type
	case "content_block_delta":
		if m.streamMsg == "" {
			return nil
		}
		id := m.streamMsg + ":" + itoa(ev.Index)
		e := m.base(nowMs())
		switch ev.Delta.Type {
		case "text_delta":
			m.streamText[ev.Index] += ev.Delta.Text
			e.Type, e.ID, e.Role, e.Text = "message", id, "assistant", m.streamText[ev.Index]
		case "thinking_delta":
			m.streamText[ev.Index] += ev.Delta.Thinking
			e.Type, e.ID, e.Text = "reasoning", id, m.streamText[ev.Index]
		default:
			return nil
		}
		return []protocol.Event{e}
	}
	return nil
}

// entry maps one transcript (.jsonl) line.
func (m *claudeMapper) entry(l *claudeLine) []protocol.Event {
	if l.ParentTU != nil && *l.ParentTU != "" {
		return m.subagentEntry(l)
	}
	if l.IsSidechain {
		return nil
	}
	if l.Cwd != "" {
		m.cwd = l.Cwd
	}
	ts := l.ts()
	switch l.Type {
	case "assistant":
		return m.assistant(l.Message, l.UUID, ts, l)
	case "user":
		return m.user(l, l.UUID, ts)
	case "system":
		if l.Subtype == "api_retry" {
			e := claudeAPIRetryEvent(l, m.sk, "")
			e.ParentID = m.parentID
			return []protocol.Event{e}
		}
		if evs, handled := m.taskLifecycle(l); handled {
			return evs
		}
		if l.Subtype == "compact_boundary" {
			e := m.base(ts)
			e.Type, e.Level, e.NoticeKind, e.Text = "notice", "info", "context_compaction", "上下文已压缩"
			return []protocol.Event{e}
		}
	}
	return nil
}

// ---- tool descriptions ----

type toolDesc struct {
	kind, title, detail, diff string
	status                    string
}

func inStr(in map[string]any, k string) string { return str(in, k) }

func readFileLimited(path string) string {
	st, err := os.Stat(path)
	if err != nil || st.IsDir() || st.Size() > 4<<20 {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// describeClaudeTool gives the phone-facing kind/title/detail/diff of a Claude Code tool call.
// live=true (approval prompts) reads the file on disk to place hunks and diff Write against the old file.
func describeClaudeTool(name string, in map[string]any, cwd string, live bool) toolDesc {
	path := func(k string) string { return shortPath(inStr(in, k), cwd) }
	abs := func(p string) string {
		if p == "" || isAbs(p) || cwd == "" {
			return p
		}
		return joinPath(cwd, p)
	}
	switch name {
	case "Skill", "skill":
		skill := inStr(in, "skill")
		if skill == "" {
			skill = inStr(in, "skill_name")
		}
		if skill == "" {
			skill = inStr(in, "name")
		}
		title := "使用技能"
		if skill != "" {
			title += " · " + skill
		}
		return toolDesc{kind: "skill", title: titleText(title), detail: truncHead(inStr(in, "args"), maxDetailChars)}
	case "Bash":
		cmd := inStr(in, "command")
		d := toolDesc{kind: "command", title: titleText("运行 " + cmd)}
		d.detail = cmd
		if desc := inStr(in, "description"); desc != "" {
			d.detail = desc + "\n" + cmd
		}
		d.detail = truncHead(d.detail, maxDetailChars)
		return d
	case "BashOutput", "KillShell", "KillBash":
		return toolDesc{kind: "command", title: "查看后台命令"}
	case "Edit":
		fp := inStr(in, "file_path")
		d := toolDesc{kind: "file_change", title: titleText("修改 " + path("file_path")), detail: fp}
		file := ""
		if live {
			file = readFileLimited(abs(fp))
		}
		d.diff = truncHead(diffHeader(fp, fp)+editDiff(inStr(in, "old_string"), inStr(in, "new_string"), file), maxDiffChars)
		return d
	case "MultiEdit":
		fp := inStr(in, "file_path")
		d := toolDesc{kind: "file_change", title: titleText("修改 " + path("file_path")), detail: fp}
		file := ""
		if live {
			file = readFileLimited(abs(fp))
		}
		var sb strings.Builder
		sb.WriteString(diffHeader(fp, fp))
		if edits, ok := in["edits"].([]any); ok {
			for _, x := range edits {
				em, _ := x.(map[string]any)
				sb.WriteString(editDiff(str(em, "old_string"), str(em, "new_string"), file))
			}
		}
		d.diff = truncHead(sb.String(), maxDiffChars)
		return d
	case "Write":
		fp := inStr(in, "file_path")
		content := inStr(in, "content")
		old := ""
		if live {
			old = readFileLimited(abs(fp))
		}
		d := toolDesc{kind: "file_change", title: titleText("写入 " + path("file_path")), detail: fp}
		if old == "" {
			d.title = titleText("创建 " + path("file_path"))
		}
		d.diff = truncHead(unifiedDiff(fp, old, content), maxDiffChars)
		return d
	case "NotebookEdit":
		fp := inStr(in, "notebook_path")
		d := toolDesc{kind: "file_change", title: titleText("修改 " + path("notebook_path")), detail: fp}
		d.diff = truncHead(addedFileDiff(fp, inStr(in, "new_source")), maxDiffChars)
		return d
	case "Read", "NotebookRead":
		k := "file_path"
		if name == "NotebookRead" {
			k = "notebook_path"
		}
		return toolDesc{kind: "read", title: titleText("读取 " + path(k)), detail: inStr(in, k)}
	case "LS":
		return toolDesc{kind: "read", title: titleText("列出 " + path("path")), detail: inStr(in, "path")}
	case "Glob":
		return toolDesc{kind: "search", title: titleText("查找文件 " + inStr(in, "pattern")), detail: inStr(in, "path")}
	case "Grep":
		detail := inStr(in, "path")
		if g := inStr(in, "glob"); g != "" {
			detail = strings.TrimSpace(detail + " " + g)
		}
		return toolDesc{kind: "search", title: titleText("搜索 “" + inStr(in, "pattern") + "”"), detail: detail}
	case "WebFetch":
		return toolDesc{kind: "web", title: titleText("访问 " + inStr(in, "url")), detail: truncHead(inStr(in, "prompt"), maxDetailChars)}
	case "WebSearch", "web_search":
		return toolDesc{kind: "web", title: titleText("搜索网页 “" + inStr(in, "query") + "”")}
	case "TodoWrite":
		todos, _ := in["todos"].([]any)
		done := 0
		var lines []string
		for _, x := range todos {
			tm, _ := x.(map[string]any)
			mark := "○"
			switch str(tm, "status") {
			case "completed":
				mark = "✓"
				done++
			case "in_progress":
				mark = "▸"
			}
			lines = append(lines, mark+" "+str(tm, "content"))
		}
		status := "running"
		if done == len(todos) {
			status = "done"
		}
		return toolDesc{kind: "plan", title: "计划 · " + itoa(done) + "/" + itoa(len(todos)), status: status,
			detail: truncHead(strings.Join(lines, "\n"), maxOutputChars)}
	case "Task", "Agent":
		return toolDesc{kind: "subagent", title: titleText("子任务 · " + inStr(in, "description")), detail: truncHead(inStr(in, "prompt"), maxDetailChars)}
	case "TaskCreate":
		return toolDesc{kind: "other", title: "创建任务", detail: truncHead(strings.TrimSpace(inStr(in, "subject")+"\n"+inStr(in, "description")), maxDetailChars)}
	case "TaskUpdate":
		return toolDesc{kind: "other", title: titleText("更新任务 #" + inStr(in, "taskId")), detail: truncHead(jsonCompact(in), maxDetailChars)}
	case "TaskGet":
		return toolDesc{kind: "other", title: titleText("查看任务 #" + inStr(in, "taskId"))}
	case "TaskList":
		return toolDesc{kind: "other", title: "查看任务列表"}
	case "ExitPlanMode":
		return toolDesc{kind: "other", title: "提交计划", detail: truncHead(inStr(in, "plan"), maxDetailChars)}
	}
	if strings.HasPrefix(name, "mcp__") {
		rest := strings.TrimPrefix(name, "mcp__")
		server, tool, _ := strings.Cut(rest, "__")
		return toolDesc{kind: "mcp", title: titleText("调用 " + server + "." + tool), detail: truncHead(jsonCompact(in), maxDetailChars)}
	}
	return toolDesc{kind: "other", title: titleText("使用 " + name), detail: truncHead(jsonCompact(in), maxDetailChars)}
}

// approvalKind maps a Claude tool to the approval.request kind.
func claudeApprovalKind(kind string) string {
	switch kind {
	case "command", "file_change":
		return kind
	}
	return "tool"
}

var claudeEditTools = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true}
