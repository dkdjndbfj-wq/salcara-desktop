package desktopcompanion

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/protocol"
)

type nativeRead struct {
	SchemaVersion int          `json:"schemaVersion"`
	Thread        nativeThread `json:"thread"`
	Page          struct {
		HasMore    bool   `json:"hasMore"`
		NextCursor string `json:"nextCursor"`
	} `json:"page"`
	Turns          []nativeTurn     `json:"turns"`
	ApprovalEvents []protocol.Event `json:"approvalEvents"`
}

func nativeApprovalEvents(key string, input []protocol.Event, now int64) ([]protocol.Event, error) {
	if len(input) > 132 {
		return nil, ErrDesktopUnavailable
	}
	output := []protocol.Event{}
	seen := map[string]bool{}
	for _, event := range input {
		if event.SessionKey != key || event.Tool != "codex" || event.ApprovalTransport != NativeApprovalTransport || !canonicalUUID.MatchString(event.ApprovalID) || seen[event.ApprovalID] {
			return nil, ErrDesktopUnavailable
		}
		seen[event.ApprovalID] = true
		if event.Type == "approval.request" {
			if event.ExpiresAt <= now {
				continue
			}
			if event.ExpiresAt-now > 110000 || !canonicalUUID.MatchString(event.TurnID) || len(event.Detail) > 16000 || len(event.Cwd) > 16384 || len(event.Title) > 160 || (event.Kind != "command" && event.Kind != "file_change" && event.Kind != "permission") || event.Questions != nil {
				return nil, ErrDesktopUnavailable
			}
			output = append(output, protocol.Event{SessionKey: key, Tool: "codex", Type: event.Type, TS: event.TS, ApprovalID: event.ApprovalID, Kind: event.Kind, Title: event.Title, Detail: event.Detail, Cwd: event.Cwd, TurnID: event.TurnID, ExpiresAt: event.ExpiresAt, ApprovalTransport: NativeApprovalTransport})
		} else if event.Type == "approval.resolved" {
			if (event.Decision != "allow" && event.Decision != "deny") || (event.By != "phone" && event.By != "timeout") {
				return nil, ErrDesktopUnavailable
			}
			output = append(output, protocol.Event{SessionKey: key, Tool: "codex", Type: event.Type, TS: event.TS, ApprovalID: event.ApprovalID, Decision: event.Decision, By: event.By, ApprovalTransport: NativeApprovalTransport})
		} else {
			return nil, ErrDesktopUnavailable
		}
	}
	return output, nil
}

type nativeTurn struct {
	ID          string                       `json:"id"`
	Status      string                       `json:"status"`
	StartedAt   int64                        `json:"startedAt"`
	CompletedAt int64                        `json:"completedAt"`
	DurationMS  int64                        `json:"durationMs"`
	Error       json.RawMessage              `json:"error"`
	Items       []map[string]json.RawMessage `json:"items"`
}

func rawString(m map[string]json.RawMessage, k string) string {
	var s string
	_ = json.Unmarshal(m[k], &s)
	return bounded(s, 20000)
}
func textWrapper(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return bounded(s, 20000)
	}
	var o struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &o)
	return bounded(o.Text, 20000)
}

// Only explicitly returned native items are projected. Hidden chain-of-thought,
// missing outputs, complete history and unreturned sub-agent details are never
// inferred. Native pages are newest_first; emit turns in conversation order.
func nativeEvents(key string, r nativeRead, scopes ...[]string) []protocol.Event {
	events := []protocol.Event{}
	for ti := len(r.Turns) - 1; ti >= 0; ti-- {
		t := r.Turns[ti]
		for _, item := range t.Items {
			kind := rawString(item, "type")
			e := protocol.Event{SessionKey: key, Tool: "codex", TS: t.StartedAt, ID: rawString(item, "id"), Status: itemStatus(rawString(item, "status")), TurnID: t.ID}
			_ = json.Unmarshal(item["durationMs"], &e.DurationMS)
			if e.TS > 0 && e.TS < 100000000000 {
				e.TS *= 1000
			}
			switch kind {
			case "userMessage":
				e.Type, e.Role, e.Final = "message", "user", true
				e.ClientID = bounded(rawString(item, "clientId"), 256)
				var content []struct {
					Type            string          `json:"type"`
					Text            string          `json:"text"`
					CodexDelegation json.RawMessage `json:"codexDelegation"`
				}
				_ = json.Unmarshal(item["content"], &content)
				var texts []string
				for _, c := range content {
					if c.Type == "text" {
						if input, ok := nativeDelegationInput(c.CodexDelegation); ok {
							texts = append(texts, input)
							e.MessageOrigin = &protocol.MessageOrigin{Kind: "delegation"}
						} else {
							texts = append(texts, bounded(c.Text, 20000))
						}
					}
				}
				e.Text = bounded(strings.Join(texts, "\n"), 20000)
			case "agentMessage":
				e.Type, e.Role, e.Text, e.Final = "message", "assistant", rawString(item, "text"), true
				if phase := rawString(item, "phase"); phase == "commentary" || phase == "final_answer" {
					e.MessagePhase = phase
				}
			case "reasoning":
				e.Type = "reasoning"
				var summary []string
				_ = json.Unmarshal(item["summary"], &summary)
				e.Text = bounded(strings.Join(summary, "\n"), 20000)
				if e.Text == "" {
					continue
				}
			case "plan":
				e.Type, e.Kind, e.Title, e.Output = "tool", "plan", "计划", rawString(item, "text")
				if t.Status == "completed" || t.Status == "failed" || t.Status == "interrupted" {
					e.OutputFinal = true
				}
			case "commandExecution":
				e.Type, e.Kind, e.Title, e.Cwd, e.Output = "tool", "command", rawString(item, "command"), rawString(item, "cwd"), textWrapper(item["output"])
				_ = json.Unmarshal(item["exitCode"], &e.ExitCode)
			case "fileChange":
				e.Type, e.Kind, e.Title = "tool", "file_change", "文件修改"
				var changes []struct {
					Path string          `json:"path"`
					Diff json.RawMessage `json:"diff"`
				}
				_ = json.Unmarshal(item["changes"], &changes)
				var paths, diffs []string
				for _, change := range changes {
					paths = append(paths, bounded(change.Path, 4096))
					if diff := textWrapper(change.Diff); diff != "" {
						diffs = append(diffs, diff)
					}
				}
				e.Detail = bounded(strings.Join(paths, "\n"), 20000)
				e.Diff = bounded(strings.Join(diffs, "\n"), 20000)
			case "collabAgentToolCall", "subAgentActivity":
				e.Type, e.Kind, e.Title = "tool", "agent", "子智能体"
				e.Detail = rawString(item, "tool")
				if e.Detail == "" {
					e.Detail = rawString(item, "kind")
				}
				if path := rawString(item, "agentPath"); path != "" {
					e.Detail += " · " + path
				}
				// Only explicit app-server child IDs, and only already-authorized
				// desktop targets, become navigable links. A path/title/prompt is
				// not a session identity and never expands the approved lease.
				e.ChildSessionKeys = nativeChildKeys(key, item, scopes)
			case "mcpToolCall", "dynamicToolCall", "functionCallOutput":
				e.Type, e.Kind, e.Title = "tool", "tool", rawString(item, "tool")
				if e.Title == "" {
					e.Title = rawString(item, "name")
				}
				e.Output = textWrapper(item["output"])
				if raw, err := json.Marshal(item); err == nil {
					if e.Status == "done" && agents.CodexToolResultFailed(raw) {
						e.Status = "failed"
					}
					e.Browser = agents.BrowserNavigationFromCodexItem(raw)
					e.Computer = agents.ComputerActivityFromCodexItem(raw)
					e.ImageData = agents.ComputerActivityImages(raw)
				}
			case "webSearch":
				e.Type, e.Kind, e.Title, e.Detail = "tool", "web", "网页搜索", rawString(item, "query")
			case "imageView", "imageGeneration":
				e = nativeImageEvent(e, item, kind)
			default:
				// A newer host may return a record this adapter cannot interpret.
				// Keep a bounded placeholder in the real turn, not raw arguments,
				// paths, results, or a fabricated successful tool operation.
				e = protocol.Event{SessionKey: key, Tool: "codex", TS: e.TS, ID: e.ID, TurnID: t.ID, Type: "notice", Level: "warn", ErrorCode: "unsupported_item", Text: "电脑返回了当前版本尚未支持的记录"}
			}
			if e.Type == "message" && e.Text == "" {
				continue
			}
			events = append(events, e)
		}
		ts := t.CompletedAt
		if ts == 0 {
			ts = t.StartedAt
		}
		if ts > 0 && ts < 100000000000 {
			ts *= 1000
		}
		status := t.Status
		if status == "inProgress" {
			status = "started"
		}
		if status != "started" && status != "completed" && status != "failed" && status != "interrupted" {
			if status != "" {
				events = append(events, protocol.Event{SessionKey: key, Tool: "codex", Type: "notice", ID: "turn-state:" + t.ID, TS: ts, TurnID: t.ID, Level: "warn", ErrorCode: "unsupported_turn_state", Text: "电脑返回了无法确认的任务状态，请在电脑核对"})
			}
			continue
		}
		e := protocol.Event{SessionKey: key, Tool: "codex", Type: "turn", ID: t.ID, TS: ts, Status: status, TurnID: t.ID, DurationMS: t.DurationMS}
		completedAt := t.CompletedAt
		if completedAt > 0 && completedAt < 100000000000 {
			completedAt *= 1000
		}
		e.Error, e.ErrorCode, e.RetryAt = nativeTurnError(t.Error, completedAt)
		if t.DurationMS > 0 {
			e.Detail = fmt.Sprintf("处理 %.1f 秒", float64(t.DurationMS)/1000)
		}
		events = append(events, e)
	}
	return events
}

// codexDelegation is an existing native-host extension, not a field in the
// public app-server UserInput schema. Preserve only its observed category and
// bounded input, never infer ChatGPT or a source thread from text/clientId.
func nativeDelegationInput(raw json.RawMessage) (string, bool) {
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
	return bounded(*text, 20000), true
}

// A returned terminal error is safe to display, but never proves that a
// NativeSend was unsent. This parser does not affect delivery receipts/retries.
func nativeTurnError(raw json.RawMessage, completedAt int64) (string, string, int64) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		text = bounded(text, 20000)
		return text, protocol.CapacityTextCode(text), 0
	}
	var value struct {
		Message           string          `json:"message"`
		CodexErrorInfo    json.RawMessage `json:"codexErrorInfo"`
		AdditionalDetails string          `json:"additionalDetails"`
	}
	if json.Unmarshal(raw, &value) != nil {
		return "", "", 0
	}
	text = bounded(value.Message, 20000)
	code := protocol.CodexExecutionErrorCode(value.CodexErrorInfo, text)
	return text, code, protocol.ExecutionRetryAt(code, completedAt, value.AdditionalDetails, value.Message, string(value.CodexErrorInfo))
}

func nativeChildKeys(parent string, item map[string]json.RawMessage, scopes [][]string) []string {
	allowed := map[string]bool{}
	for _, scope := range scopes {
		for _, key := range scope {
			if key != parent {
				if _, ok := desktopSessionID(key); ok {
					allowed[key] = true
				}
			}
		}
	}
	seen := map[string]bool{}
	var keys []string
	add := func(id string) {
		key := "codex:" + id
		if allowed[key] && !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	var receivers []string
	_ = json.Unmarshal(item["receiverThreadIds"], &receivers)
	for _, id := range receivers {
		add(id)
	}
	var states map[string]json.RawMessage
	_ = json.Unmarshal(item["agentsStates"], &states)
	var extra []string
	for id := range states {
		extra = append(extra, id)
	}
	sort.Strings(extra)
	for _, id := range extra {
		add(id)
	}
	return keys
}

func itemStatus(s string) string {
	switch s {
	case "inProgress", "running":
		return "running"
	case "completed", "success":
		return "done"
	case "failed", "error":
		return "failed"
	default:
		return s
	}
}

// Native imageView has no required status field. A returned path proves what
// was viewed, not whether the operation succeeded. Current clients require a
// known tool status, so use a neutral notice for this status-less native record.
// imageGeneration reports its own status, independent of the parent turn.
func nativeImageEvent(e protocol.Event, item map[string]json.RawMessage, kind string) protocol.Event {
	var status string
	raw := item["status"]
	valid := json.Unmarshal(raw, &status) == nil && string(raw) != "null"
	e.Status = ""
	switch status {
	case "inProgress", "running":
		e.Status = "running"
	case "completed", "done", "success":
		e.Status = "done"
	case "failed", "error", "declined":
		e.Status = "failed"
	case "":
		valid = false
	default:
		valid = false
	}
	if !valid {
		if kind == "imageView" {
			return protocol.Event{SessionKey: e.SessionKey, Tool: e.Tool, TS: e.TS, ID: e.ID, TurnID: e.TurnID,
				Type: "notice", Level: "info", Text: "图片查看记录 · 电脑未提供执行状态"}
		}
		return protocol.Event{SessionKey: e.SessionKey, Tool: e.Tool, TS: e.TS, ID: e.ID, TurnID: e.TurnID,
			Type: "notice", Level: "warn", ErrorCode: "unsupported_item", Text: "电脑返回了无法确认状态的图片记录"}
	}
	e.Type = "tool"
	if kind == "imageView" {
		e.Kind, e.Title = "image_view", "查看图片"
		if e.Status != "failed" {
			e.Detail = nativeImagePath(item["path"])
		}
		return e
	}
	e.Kind, e.Title = "image_generation", "生成图片"
	if e.Status == "failed" {
		e.Output = rawString(item, "result")
		return e
	}
	if e.Status != "done" {
		return e
	}
	// Only explicit existing output fields can feed the local media importer.
	// It verifies the actual file/image bytes and creates session-bound refs;
	// the mapper never invents attachments or treats arbitrary URLs as media.
	e.Detail = nativeImagePath(item["savedPath"])
	if e.Detail == "" {
		e.Detail = nativeImagePath(item["result"])
	}
	if e.Detail == "" && len(item["result"]) <= 16<<20 {
		var result string
		if json.Unmarshal(item["result"], &result) == nil {
			result = strings.TrimSpace(result)
			if len(result) >= 16 && len(result) <= 12<<20 &&
				(strings.HasPrefix(result, "iVBOR") || strings.HasPrefix(result, "/9j/") || strings.HasPrefix(result, "UklGR") ||
					strings.HasPrefix(result, "data:image/png;base64,") || strings.HasPrefix(result, "data:image/jpeg;base64,") || strings.HasPrefix(result, "data:image/webp;base64,")) {
				e.ImageData = []string{result}
			}
		}
	}
	return e
}

func nativeImagePath(raw json.RawMessage) string {
	if len(raw) > 64<<10 {
		return ""
	}
	var path string
	if json.Unmarshal(raw, &path) != nil || len(path) > 16384 || strings.ContainsAny(path, "\x00\r\n") || !filepath.IsAbs(path) {
		return ""
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".webp":
		return path
	}
	return ""
}
