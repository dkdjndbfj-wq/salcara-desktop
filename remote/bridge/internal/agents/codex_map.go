package agents

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"salcara/bridge/internal/protocol"
)

// Wire shapes of `codex app-server` (v2), trimmed to what the bridge uses. Field names follow
// codex-rs/app-server-protocol/schema/typescript/v2.

type cxThread struct {
	ID             string          `json:"id"`
	Preview        string          `json:"preview"`
	Ephemeral      bool            `json:"ephemeral"`
	HistoryMode    string          `json:"historyMode"`
	ModelProvider  string          `json:"modelProvider"`
	Model          *string         `json:"model"`
	CreatedAt      int64           `json:"createdAt"`
	UpdatedAt      int64           `json:"updatedAt"`
	Status         cxThreadStatus  `json:"status"`
	Path           *string         `json:"path"`
	Cwd            string          `json:"cwd"`
	Originator     *string         `json:"originator"`
	Source         json.RawMessage `json:"source"`
	ParentThreadID *string         `json:"parentThreadId"`
	ForkedFromID   *string         `json:"forkedFromId"`
	AgentNickname  *string         `json:"agentNickname"`
	AgentRole      *string         `json:"agentRole"`
	Name           *string         `json:"name"`
	Turns          []cxTurn        `json:"turns"`
}

type cxThreadStatus struct {
	Type        string   `json:"type"`
	ActiveFlags []string `json:"activeFlags"`
}

type cxTurn struct {
	ID          string            `json:"id"`
	Items       []json.RawMessage `json:"items"`
	ItemsView   string            `json:"itemsView,omitempty"`
	Status      string            `json:"status"`
	Error       *cxTurnError      `json:"error"`
	StartedAt   *int64            `json:"startedAt"`
	CompletedAt *int64            `json:"completedAt"`
	DurationMS  *int64            `json:"durationMs"`
}

type cxTurnError struct {
	Message           string          `json:"message"`
	AdditionalDetails *string         `json:"additionalDetails"`
	CodexErrorInfo    json.RawMessage `json:"codexErrorInfo"`
}

func (e *cxTurnError) executionCode() string {
	if e == nil {
		return ""
	}
	return protocol.CodexExecutionErrorCode(e.CodexErrorInfo, e.Message)
}

func (e *cxTurnError) retryAt(observedAt int64) int64 {
	if e == nil {
		return 0
	}
	detail := ""
	if e.AdditionalDetails != nil {
		detail = *e.AdditionalDetails
	}
	return protocol.ExecutionRetryAt(e.executionCode(), observedAt, detail, e.Message, string(e.CodexErrorInfo))
}

type cxAction struct {
	Type    string  `json:"type"`
	Command string  `json:"command"`
	Name    string  `json:"name"`
	Path    *string `json:"path"`
	Query   *string `json:"query"`
}

type cxChange struct {
	Path string `json:"path"`
	Kind struct {
		Type     string  `json:"type"`
		MovePath *string `json:"move_path"`
	} `json:"kind"`
	Diff string `json:"diff"`
}

type cxItem struct {
	Type     string  `json:"type"`
	ID       string  `json:"id"`
	ClientID *string `json:"clientId"`
	Phase    *string `json:"phase"`

	Content json.RawMessage `json:"content"` // userMessage: []UserInput; reasoning: []string
	Text    string          `json:"text"`
	Summary []string        `json:"summary"`

	Command          string     `json:"command"`
	Cwd              string     `json:"cwd"`
	Status           string     `json:"status"`
	CommandActions   []cxAction `json:"commandActions"`
	AggregatedOutput *string    `json:"aggregatedOutput"`
	ExitCode         *int       `json:"exitCode"`

	Changes []cxChange `json:"changes"`

	Server            string                  `json:"server"`
	Tool              string                  `json:"tool"`
	Namespace         *string                 `json:"namespace"`
	Arguments         json.RawMessage         `json:"arguments"`
	Result            json.RawMessage         `json:"result"`
	Error             json.RawMessage         `json:"error"`
	ContentItems      json.RawMessage         `json:"contentItems"`
	Success           *bool                   `json:"success"`
	Prompt            *string                 `json:"prompt"`
	ReceiverThreadIDs []string                `json:"receiverThreadIds"`
	AgentsStates      map[string]cxAgentState `json:"agentsStates"`
	AgentThreadID     string                  `json:"agentThreadId"`
	AgentPath         string                  `json:"agentPath"`
	Kind              string                  `json:"kind"`

	Query  string          `json:"query"`
	Action json.RawMessage `json:"action"`
	Path   string          `json:"path"`
	// imageGeneration may return a local saved path in this field. It is
	// converted to an opaque attachment by hubclient before leaving the PC.
	SavedPath  string `json:"savedPath"`
	Review     string `json:"review"`
	DurationMS int64  `json:"durationMs"`
	Fragments  []struct {
		Text string `json:"text"`
	} `json:"fragments"`
}

type cxUserInput struct {
	Type            string          `json:"type"`
	Text            string          `json:"text"`
	Path            string          `json:"path"`
	URL             string          `json:"url"`
	Name            string          `json:"name"`
	CodexDelegation json.RawMessage `json:"codexDelegation"`
}

// codexClient maps Thread.source / originator to SessionInfo.Client.
func codexClient(source json.RawMessage, originator string, owned bool) string {
	if owned {
		return "Codex"
	}
	var s string
	_ = json.Unmarshal(source, &s)
	o := strings.ToLower(originator)
	appLike := strings.Contains(o, "desktop") || strings.Contains(o, "codex_app") || strings.Contains(o, "codex app")
	switch s {
	case "cli", "exec":
		return "Codex CLI"
	case "vscode":
		if appLike {
			return "Codex App"
		}
		return "Codex IDE"
	case "appServer":
		return "Codex App"
	}
	if appLike {
		return "Codex App"
	}
	return "Codex"
}

func userInputText(raw json.RawMessage) string {
	text, _ := codexUserInputProjection(raw)
	return text
}

func codexUserInputProjection(raw json.RawMessage) (string, *protocol.MessageOrigin) {
	var parts []cxUserInput
	if json.Unmarshal(raw, &parts) != nil {
		return "", nil
	}
	var out []string
	var origin *protocol.MessageOrigin
	for _, p := range parts {
		switch p.Type {
		case "text":
			if input, ok := codexDelegationInput(p.CodexDelegation); ok {
				out = append(out, input)
				origin = &protocol.MessageOrigin{Kind: "delegation"}
			} else {
				out = append(out, p.Text)
			}
		case "image", "localImage":
			out = append(out, "[图片]")
		case "mention":
			out = append(out, "@"+p.Name)
		}
	}
	text := strings.Join(out, "\n")
	if origin != nil && len(text) > 20000 {
		text = text[:20000]
	}
	return text, origin
}

// Match the native history adapter's observed extension: only an exact,
// string-valued input inside an object carries the delegation category. This
// is not public UserInput sender identity or permission to control another
// task; sourceThreadId, clientId, and natural-language claims are never used.
func codexDelegationInput(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || raw[0] != '{' {
		return "", false
	}
	var value map[string]json.RawMessage
	if json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	input, exists := value["input"]
	var text *string
	if !exists || json.Unmarshal(input, &text) != nil || text == nil {
		return "", false
	}
	if len(*text) > 20000 {
		return (*text)[:20000], true
	}
	return *text, true
}

// userInputAttachments extracts only the opaque attachment filenames created
// by the Bridge.  A remote URL or an arbitrary local path is never fetched or
// exposed to the phone; this also makes old Codex history safe to project.
func userInputAttachments(raw json.RawMessage) []protocol.AttachmentRef {
	var parts []cxUserInput
	if json.Unmarshal(raw, &parts) != nil {
		return nil
	}
	var out []protocol.AttachmentRef
	seen := map[string]bool{}
	for _, p := range parts {
		if p.Type != "image" && p.Type != "localImage" {
			continue
		}
		base := filepath.Base(p.Path)
		if !strings.HasPrefix(base, "att_") {
			continue
		}
		parts := strings.SplitN(base, ".", 2)
		id := parts[0]
		if len(id) != 36 || seen[id] || !isAttachmentID(id) {
			continue
		}
		mime := "image/jpeg"
		switch strings.ToLower(filepath.Ext(base)) {
		case ".png":
			mime = "image/png"
		case ".webp":
			mime = "image/webp"
		}
		ref := protocol.AttachmentRef{ID: id, Mime: mime}
		if info, err := os.Stat(p.Path); err == nil && info.Size() >= 0 && info.Size() <= 8<<20 {
			ref.Size = info.Size()
		}
		out = append(out, ref)
		seen[id] = true
		if len(out) == 4 {
			break
		}
	}
	return out
}

func isAttachmentID(id string) bool {
	if !strings.HasPrefix(id, "att_") || len(id) != 36 {
		return false
	}
	for _, c := range id[4:] {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func reasoningText(summary []string, content json.RawMessage) string {
	if len(summary) > 0 {
		return strings.TrimSpace(strings.Join(summary, "\n\n"))
	}
	var c []string
	_ = json.Unmarshal(content, &c)
	return strings.TrimSpace(strings.Join(c, "\n\n"))
}

func cxToolStatus(status string, exit *int) string {
	switch status {
	case "inProgress", "":
		return "running"
	case "completed":
		if exit != nil && *exit != 0 {
			return "failed"
		}
		return "done"
	default: // failed, declined
		return "failed"
	}
}

func imageGenerationStatus(status string) string {
	switch status {
	case "inProgress", "running":
		return "running"
	case "failed", "error", "declined":
		return "failed"
	case "completed", "done", "success", "":
		return "done"
	}
	return ""
}

func imageGenerationPath(it cxItem) string {
	if filepath.IsAbs(it.SavedPath) {
		return it.SavedPath
	}
	// A few app-server versions put the local path in result. Only accept an
	// absolute path; URLs, data URLs and provider text must never be opened.
	var result string
	if len(it.Result) > 0 {
		_ = json.Unmarshal(it.Result, &result)
	}
	if filepath.IsAbs(result) {
		return result
	}
	return ""
}

// imageGenerationData is a bounded fallback for app-server versions that do
// not persist savedPath but keep the generated PNG/JPEG/WebP in result.  The
// Hub client consumes the private marker and converts it to an opaque
// attachment before serialization; it is never a phone-visible data URL.
func imageGenerationData(it cxItem) string {
	var result string
	if len(it.Result) == 0 || json.Unmarshal(it.Result, &result) != nil {
		return ""
	}
	result = strings.TrimSpace(result)
	if len(result) < 16 || len(result) > 12<<20 || filepath.IsAbs(result) {
		return ""
	}
	return "image-base64:" + result
}

func codexChangesDiff(changes []cxChange) string {
	var sb strings.Builder
	for _, ch := range changes {
		mp := ""
		if ch.Kind.MovePath != nil {
			mp = *ch.Kind.MovePath
		}
		d := normalizeCodexDiff(ch.Path, ch.Kind.Type, mp, ch.Diff)
		sb.WriteString(d)
		if !strings.HasSuffix(d, "\n") {
			sb.WriteByte('\n')
		}
	}
	return truncHead(sb.String(), maxDiffChars)
}

func codexChangesTitle(changes []cxChange, cwd string) string {
	if len(changes) == 0 {
		return "修改文件"
	}
	if len(changes) == 1 {
		verb := "修改 "
		switch changes[0].Kind.Type {
		case "add":
			verb = "创建 "
		case "delete":
			verb = "删除 "
		}
		return titleText(verb + shortPath(changes[0].Path, cwd))
	}
	return titleText("修改 " + shortPath(changes[0].Path, cwd) + " 等 " + itoa(len(changes)) + " 个文件")
}

func itoa(n int) string { return strconv.Itoa(n) }

func codexCommandKindTitle(cmd string, actions []cxAction, cwd string) (string, string) {
	if len(actions) > 0 {
		allRead, allSearch, allList := true, true, true
		for _, a := range actions {
			allRead = allRead && a.Type == "read"
			allSearch = allSearch && a.Type == "search"
			allList = allList && a.Type == "listFiles"
		}
		switch {
		case allRead:
			names := make([]string, 0, len(actions))
			for _, a := range actions {
				n := a.Name
				if n == "" && a.Path != nil {
					n = shortPath(*a.Path, cwd)
				}
				names = append(names, n)
			}
			return "read", titleText("读取 " + strings.Join(names, ", "))
		case allSearch:
			q := ""
			if actions[0].Query != nil {
				q = *actions[0].Query
			}
			if q != "" {
				return "search", titleText("搜索 “" + q + "”")
			}
			return "search", titleText("搜索 " + cmd)
		case allList:
			p := ""
			if actions[0].Path != nil {
				p = shortPath(*actions[0].Path, cwd)
			}
			return "search", titleText("列出文件 " + p)
		}
	}
	return "command", titleText("运行 " + cmd)
}

func mcpResultText(result json.RawMessage) string {
	var r struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
	}
	if json.Unmarshal(result, &r) != nil {
		return ""
	}
	var parts []string
	for _, c := range r.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		} else if c.Type != "" {
			parts = append(parts, "["+c.Type+"]")
		}
	}
	if len(parts) == 0 && len(r.StructuredContent) > 0 && string(r.StructuredContent) != "null" {
		return truncTail(jsonCompact(r.StructuredContent), maxOutputChars)
	}
	return strings.Join(parts, "\n")
}

func mcpResultImages(result json.RawMessage) []string {
	var r struct {
		Content []struct {
			Type string `json:"type"`
			Data string `json:"data"`
		} `json:"content"`
	}
	if json.Unmarshal(result, &r) != nil {
		return nil
	}
	var images []string
	for _, item := range r.Content {
		if item.Type == "image" && len(item.Data) >= 16 && len(item.Data) <= 12<<20 {
			images = append(images, item.Data)
			if len(images) == 4 {
				break
			}
		}
	}
	return images
}

// Dynamic native tools use contentItems rather than MCP's result.content.
// Only bounded data images are handed to the private attachment importer;
// URLs and audio stay descriptive and never trigger a network/file read.
func dynamicToolContent(raw json.RawMessage) (string, []string) {
	var items []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL string `json:"imageUrl"`
	}
	if json.Unmarshal(raw, &items) != nil {
		return "", nil
	}
	var parts []string
	var images []string
	for i, item := range items {
		if i == 256 {
			parts = append(parts, "[更多工具输出已省略]")
			break
		}
		switch item.Type {
		case "inputText":
			parts = append(parts, truncTail(item.Text, maxOutputChars))
		case "inputImage":
			parts = append(parts, "[图片]")
			if len(images) < 4 && len(item.ImageURL) <= 12<<20 &&
				(strings.HasPrefix(item.ImageURL, "data:image/png;base64,") ||
					strings.HasPrefix(item.ImageURL, "data:image/jpeg;base64,") ||
					strings.HasPrefix(item.ImageURL, "data:image/webp;base64,")) {
				images = append(images, item.ImageURL)
			}
		case "inputAudio":
			parts = append(parts, "[音频]")
		}
	}
	return truncTail(strings.Join(parts, "\n"), maxOutputChars), images
}

// codexItemEvent converts one ThreadItem. ok=false means the item isn't shown on the phone.
// completed=true marks the final state (history items are always completed).
func codexItemEvent(sk string, it cxItem, completed bool, cwd string) (protocol.Event, bool) {
	e := protocol.Event{SessionKey: sk, Tool: "codex", TS: nowMs(), ID: it.ID}
	switch it.Type {
	case "userMessage":
		e.Type, e.Role, e.Final = "message", "user", true
		e.Text, e.MessageOrigin = codexUserInputProjection(it.Content)
		e.Skills = codexUserInputSkills(it.Content)
		if it.ClientID != nil {
			e.ClientID = truncHead(*it.ClientID, 256)
		}
		e.Attachments = userInputAttachments(it.Content)
		return e, e.Text != "" || len(e.Attachments) > 0 || len(e.Skills) > 0
	case "agentMessage":
		e.Type, e.Role, e.Text, e.Final = "message", "assistant", it.Text, completed
		e.MessagePhase = codexMessagePhase(it.Phase)
		return e, e.Text != "" || completed
	case "reasoning":
		e.Type, e.Text, e.Final = "reasoning", reasoningText(it.Summary, it.Content), completed
		return e, e.Text != ""
	case "plan":
		e.Type, e.Kind, e.Title, e.Status = "tool", "plan", "计划", "done"
		e.PlanKind = "proposal"
		e.OutputFinal = completed
		if !completed {
			e.Status = "running"
		}
		e.Output = truncHead(it.Text, maxOutputChars)
		return e, true
	case "commandExecution":
		e.Type = "tool"
		e.Kind, e.Title = codexCommandKindTitle(it.Command, it.CommandActions, it.Cwd)
		e.Detail = truncHead(it.Command, maxDetailChars)
		e.Cwd = it.Cwd
		e.Status = cxToolStatus(it.Status, it.ExitCode)
		if it.DurationMS > 0 {
			e.DurationMS = it.DurationMS
		}
		if it.AggregatedOutput != nil {
			e.Output = truncTail(*it.AggregatedOutput, maxOutputChars)
		}
		e.ExitCode = it.ExitCode
		if it.Status == "declined" {
			e.Output = "已拒绝"
		}
		return e, true
	case "fileChange":
		e.Type, e.Kind = "tool", "file_change"
		e.Title = codexChangesTitle(it.Changes, cwd)
		paths := make([]string, 0, len(it.Changes))
		for _, c := range it.Changes {
			paths = append(paths, c.Path)
		}
		e.Detail = truncHead(strings.Join(paths, "\n"), maxDetailChars)
		e.Diff = codexChangesDiff(it.Changes)
		e.Status = cxToolStatus(it.Status, nil)
		if it.Status == "declined" {
			e.Output = "已拒绝"
		}
		return e, true
	case "mcpToolCall":
		e.Type, e.Kind = "tool", "mcp"
		e.Computer = codexComputerActivity(it)
		e.Title = titleText("调用 " + it.Server + "." + it.Tool)
		e.Detail = truncHead(jsonCompact(it.Arguments), maxDetailChars)
		e.Status = cxToolStatus(it.Status, nil)
		if len(it.Error) > 0 && string(it.Error) != "null" {
			var me struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(it.Error, &me)
			e.Output = truncTail(me.Message, maxOutputChars)
			if completed {
				e.Status = "failed"
			}
		} else {
			e.Output = truncTail(mcpResultText(it.Result), maxOutputChars)
			e.ImageData = mcpResultImages(it.Result)
			if completed && codexToolResultFailed(it.Type, it.Success, it.Result, it.Error) {
				e.Status = "failed"
			}
			if completed {
				e.Browser = codexBrowserNavigation(it)
			}
		}
		return e, true
	case "dynamicToolCall":
		e.Type, e.Kind = "tool", "other"
		e.Computer = codexComputerActivity(it)
		tool := it.Tool
		if it.Namespace != nil && *it.Namespace != "" {
			tool = *it.Namespace + "." + tool
		}
		e.Title = titleText("调用 " + tool)
		e.Detail = truncHead(jsonCompact(it.Arguments), maxDetailChars)
		e.Status = cxToolStatus(it.Status, nil)
		e.Output, e.ImageData = dynamicToolContent(it.ContentItems)
		if completed && codexToolResultFailed(it.Type, it.Success, it.Result, it.Error) {
			e.Status = "failed"
		}
		if completed {
			e.Browser = codexBrowserNavigation(it)
		}
		return e, true
	case "hookPrompt":
		var fragments []string
		for i, fragment := range it.Fragments {
			if i == 64 {
				break
			}
			fragments = append(fragments, truncHead(fragment.Text, maxOutputChars))
		}
		e.Type, e.Level = "notice", "info"
		e.Text = "钩子提示：" + truncHead(strings.Join(fragments, "\n"), maxOutputChars)
		return e, len(fragments) > 0
	case "sleep":
		e.Type, e.Kind, e.Title = "tool", "other", "等待"
		e.Status = "running"
		if completed {
			e.Status = "done"
		}
		if it.DurationMS >= 0 {
			e.DurationMS = it.DurationMS
			e.Detail = fmt.Sprintf("%g 秒", float64(it.DurationMS)/1000)
		}
		return e, true
	case "collabAgentToolCall":
		e.Type, e.Kind = "tool", "subagent"
		e.Title = codexSubagentTitle(it.Tool)
		if it.Prompt != nil {
			e.Detail = truncHead(*it.Prompt, maxDetailChars)
		}
		e.Status = cxToolStatus(it.Status, nil)
		for _, id := range codexChildIDs(it) {
			if state := it.AgentsStates[id]; state.Status != "notFound" {
				e.ChildSessionKeys = append(e.ChildSessionKeys, "codex:"+id)
			}
		}
		return e, true
	case "subAgentActivity":
		if !validChildID(it.AgentThreadID) {
			return e, false
		}
		e.Type, e.Kind, e.Title = "tool", "subagent", "子智能体"
		e.Detail = truncHead(it.AgentPath, maxDetailChars)
		e.ChildSessionKeys = []string{"codex:" + it.AgentThreadID}
		e.Status, e.Output = codexAgentStatus(it.Kind, "")
		return e, true
	case "webSearch":
		e.Type, e.Kind = "tool", "web"
		e.Title = titleText("搜索网页 “" + it.Query + "”")
		var act struct {
			Type    string `json:"type"`
			URL     string `json:"url"`
			Pattern string `json:"pattern"`
		}
		if json.Unmarshal(it.Action, &act) == nil {
			switch act.Type {
			case "openPage":
				if act.URL != "" {
					e.Title = titleText("打开网页 " + act.URL)
				}
			case "findInPage":
				e.Title = titleText("在网页中查找 “" + act.Pattern + "”")
			}
		}
		if it.Query == "" && e.Title == "搜索网页 “”" {
			e.Title = "搜索网页"
		}
		e.Status = "done"
		if !completed {
			e.Status = "running"
		}
		return e, true
	case "imageView":
		// The absolute path is kept in Detail only inside the local Bridge. The
		// hub client replaces it with an opaque, authorized attachment ref before
		// the event leaves this computer.
		// Keep image events distinct from text/file reads so clients can render
		// the correct affordance. Older clients treat unknown kinds as generic
		// tool rows and still retain Detail/attachments.
		e.Type, e.Kind, e.Title, e.Detail, e.Status = "tool", "image_view", "查看图片", it.Path, "done"
		if !completed {
			e.Status = "running"
		}
		return e, true
	case "imageGeneration":
		// Generated images remain inside the folded work summary until the user
		// expands it. hubclient imports the local path into the same bounded,
		// session-scoped attachment store used by imageView.
		e.Type, e.Kind, e.Title, e.Status = "tool", "image_generation", "生成图片", imageGenerationStatus(it.Status)
		if e.Status == "" {
			return protocol.Event{SessionKey: sk, ID: it.ID, Type: "notice", Level: "warn", ErrorCode: "unsupported_item", Text: "电脑返回了无法确认状态的图片记录"}, true
		}
		e.Detail = imageGenerationPath(it)
		if e.Detail == "" && e.Status != "failed" {
			e.Detail = imageGenerationData(it)
		}
		if e.Status == "failed" {
			var result string
			_ = json.Unmarshal(it.Result, &result)
			e.Output = truncTail(result, maxOutputChars)
		}
		return e, true
	case "enteredReviewMode":
		e.Type, e.Level, e.Text, e.ID = "notice", "info", "开始代码审查", ""
		return e, true
	case "exitedReviewMode":
		e.Type, e.Role, e.Text, e.Final = "message", "assistant", it.Review, true
		return e, it.Review != ""
	case "contextCompaction":
		threadID := strings.TrimPrefix(sk, "codex:")
		e.Type, e.Level, e.NoticeKind, e.ID = "notice", "info", "context_compaction", "context-compaction:"+threadID+":"+it.ID
		e.Text, e.Progress = "正在压缩上下文", !completed
		if completed {
			e.Text = "上下文已压缩"
		}
		return e, true
	}
	// New app-server versions may add item types before this adapter learns
	// their payload. Show only a neutral record warning, never raw arguments or
	// guessed successful tool results. Repeated live updates share the item ID.
	if completed {
		e.Type, e.Level, e.ErrorCode = "notice", "warn", "unsupported_item"
		e.Text = "电脑返回了当前版本尚未支持的记录"
		if validChildID(it.Type) && len(it.Type) <= 80 {
			e.Text += "：" + it.Type
		}
		return e, true
	}
	return e, false
}

func knownCodexTurnStatus(status string) bool {
	switch status {
	case "completed", "interrupted", "failed", "inProgress":
		return true
	}
	return false
}

func codexTurnStatus(s string) string {
	switch s {
	case "completed":
		return "completed"
	case "interrupted":
		return "interrupted"
	case "failed":
		return "failed"
	}
	return "started"
}

func codexMessagePhase(phase *string) string {
	if phase != nil && (*phase == "commentary" || *phase == "final_answer") {
		return *phase
	}
	return ""
}

// codexHistory converts thread/read turns to events, oldest first.
func codexHistory(sk string, th cxThread) []protocol.Event {
	var evs []protocol.Event
	for _, t := range th.Turns {
		ts := int64(0)
		if t.StartedAt != nil {
			ts = *t.StartedAt * 1000
		} else {
			ts = th.UpdatedAt * 1000
		}
		for _, raw := range t.Items {
			var it cxItem
			if json.Unmarshal(raw, &it) != nil {
				continue
			}
			if e, ok := codexItemEvent(sk, it, true, th.Cwd); ok {
				e.TS = ts
				if t.ID != "" {
					e.TurnID = t.ID
				}
				evs = append(evs, e)
				for _, child := range codexSubagentEvents(sk, it) {
					child.TS = ts
					if t.ID != "" {
						child.TurnID = t.ID
					}
					evs = append(evs, child)
				}
			}
		}
		if !knownCodexTurnStatus(t.Status) {
			if t.Status != "" {
				evs = append(evs, protocol.Event{SessionKey: sk, Tool: "codex", TS: ts, Type: "notice", ID: "turn-state:" + t.ID, TurnID: t.ID, Level: "warn", ErrorCode: "unsupported_turn_state", Text: "电脑返回了无法确认的任务状态，请在电脑核对"})
			}
			continue
		}
		if t.DurationMS != nil && *t.DurationMS >= 0 || t.Status == "completed" || t.Status == "failed" || t.Status == "interrupted" || t.Status == "inProgress" {
			status := codexTurnStatus(t.Status)
			e := protocol.Event{SessionKey: sk, Tool: "codex", TS: ts, Type: "turn", Status: status, TurnID: t.ID}
			if t.ID != "" {
				e.ID = "turn:" + t.ID
			}
			if t.DurationMS != nil && *t.DurationMS >= 0 {
				e.DurationMS = *t.DurationMS
			}
			if t.Error != nil {
				e.Error = t.Error.Message
				e.ErrorCode = t.Error.executionCode()
				// Relative Retry-After belongs to the failure, not turn start or
				// history-read time. Missing completion timestamps stay unknown.
				completedAt := int64(0)
				if t.CompletedAt != nil {
					completedAt = *t.CompletedAt * 1000
				}
				e.RetryAt = t.Error.retryAt(completedAt)
			}
			evs = append(evs, e)
		}
	}
	return evs
}

type cxPlanStep struct {
	Step   string `json:"step"`
	Status string `json:"status"` // pending | inProgress | completed
}

// codexPlanEvent encodes the checklist as one line per step with a status mark
// (✓ done, ▸ in progress, ○ pending) so older phones still show readable text.
func codexPlanEvent(sk, id string, steps []cxPlanStep) protocol.Event {
	lines := make([]string, 0, len(steps))
	done := 0
	for _, st := range steps {
		mark := "○"
		switch st.Status {
		case "completed":
			mark = "✓"
			done++
		case "inProgress", "in_progress":
			mark = "▸"
		}
		lines = append(lines, mark+" "+strings.TrimSpace(st.Step))
	}
	status := "running"
	if done == len(steps) {
		status = "done"
	}
	return protocol.Event{SessionKey: sk, Type: "tool", ID: id, Kind: "plan", Title: fmt.Sprintf("计划 · %d/%d", done, len(steps)), Status: status,
		Output: truncHead(strings.Join(lines, "\n"), maxOutputChars)}
}
