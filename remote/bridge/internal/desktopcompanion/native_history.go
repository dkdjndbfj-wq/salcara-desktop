package desktopcompanion

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

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
				var content []struct {
					Type            string `json:"type"`
					Text            string `json:"text"`
					CodexDelegation *struct {
						Input string `json:"input"`
					} `json:"codexDelegation"`
				}
				_ = json.Unmarshal(item["content"], &content)
				var texts []string
				for _, c := range content {
					if c.Type == "text" {
						if c.CodexDelegation != nil {
							texts = append(texts, bounded(c.CodexDelegation.Input, 20000))
						} else {
							texts = append(texts, bounded(c.Text, 20000))
						}
					}
				}
				e.Text = bounded(strings.Join(texts, "\n"), 20000)
			case "agentMessage":
				e.Type, e.Role, e.Text, e.Final = "message", "assistant", rawString(item, "text"), true
			case "reasoning":
				e.Type = "reasoning"
				var summary []string
				_ = json.Unmarshal(item["summary"], &summary)
				e.Text = bounded(strings.Join(summary, "\n"), 20000)
				if e.Text == "" {
					continue
				}
			case "plan":
				e.Type, e.Kind, e.Title, e.Detail = "tool", "plan", "计划", rawString(item, "text")
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
			case "webSearch":
				e.Type, e.Kind, e.Title, e.Detail = "tool", "web", "网页搜索", rawString(item, "query")
			default:
				continue
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
		e := protocol.Event{SessionKey: key, Tool: "codex", Type: "turn", ID: t.ID, TS: ts, Status: status, TurnID: t.ID, DurationMS: t.DurationMS}
		if t.DurationMS > 0 {
			e.Detail = fmt.Sprintf("处理 %.1f 秒", float64(t.DurationMS)/1000)
		}
		events = append(events, e)
	}
	return events
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
