package agents

import (
	"encoding/json"
	"strings"

	"salcara/bridge/internal/protocol"
)

// Supplemental app-server notifications stay in their real thread. In
// particular, a global configuration warning must not invent a target task.
func (a *codexAgent) onSupplementalNotify(method string, params json.RawMessage) bool {
	switch method {
	case "item/plan/delta", "item/mcpToolCall/progress", "thread/name/updated", "thread/compacted", "model/rerouted", "warning", "guardianWarning":
	default:
		return false
	}
	var n struct {
		ThreadID   string  `json:"threadId"`
		TurnID     string  `json:"turnId"`
		ItemID     string  `json:"itemId"`
		Delta      string  `json:"delta"`
		Message    string  `json:"message"`
		ThreadName *string `json:"threadName"`
		FromModel  string  `json:"fromModel"`
		ToModel    string  `json:"toModel"`
	}
	if json.Unmarshal(params, &n) != nil || !validChildID(n.ThreadID) {
		return true
	}
	a.mu.Lock()
	t := a.threads[n.ThreadID]
	if t == nil {
		a.mu.Unlock()
		return true
	}
	sk := t.info.SessionKey
	if sk == "" {
		sk = "codex:" + n.ThreadID
	}
	if method == "thread/name/updated" {
		if n.ThreadName == nil || strings.TrimSpace(*n.ThreadName) == "" {
			a.mu.Unlock()
			return true
		}
		t.info.Title = titleText(*n.ThreadName)
		info := t.info
		a.mu.Unlock()
		a.emit(protocol.Event{SessionKey: sk, Type: "session.updated", Session: ptrInfo(info)})
		return true
	}
	if method == "item/plan/delta" || method == "item/mcpToolCall/progress" {
		if !validChildID(n.ItemID) {
			a.mu.Unlock()
			return true
		}
		l := t.live(n.ItemID)
		e := l.lastEvent
		if method == "item/plan/delta" && e.OutputFinal {
			a.mu.Unlock()
			return true
		}
		if e.ID == "" {
			e = protocol.Event{SessionKey: sk, Type: "tool", ID: n.ItemID, Kind: "mcp", Title: "工具进度"}
			if method == "item/plan/delta" {
				e.Kind, e.Title = "plan", "计划"
			}
		}
		if method == "item/plan/delta" {
			e.PlanKind = "proposal"
			l.text = truncTail(l.text+n.Delta, maxOutputChars)
			e.Output = l.text
		} else {
			l.output = truncTail(strings.TrimSpace(l.output+"\n"+n.Message), maxOutputChars)
			e.Output = l.output
		}
		e.Status, e.TurnID = "running", n.TurnID
		l.lastEvent = e
		a.mu.Unlock()
		a.partial(e)
		return true
	}
	if method == "model/rerouted" {
		if n.ToModel == "" || len(n.ToModel) > 200 || len(n.FromModel) > 200 || strings.ContainsAny(n.ToModel+n.FromModel, "\r\n") {
			a.mu.Unlock()
			return true
		}
		t.info.Model = n.ToModel
		info := t.info
		a.mu.Unlock()
		a.emit(protocol.Event{SessionKey: sk, Type: "session.updated", Session: ptrInfo(info)})
		a.emit(protocol.Event{SessionKey: sk, Type: "notice", TurnID: n.TurnID, Level: "warn", Text: "模型已从 " + n.FromModel + " 切换为 " + n.ToModel})
		return true
	}
	a.mu.Unlock()
	if method == "thread/compacted" {
		// The context observer emits one stable, deduplicated lifecycle notice.
		return true
	}
	text, level := n.Message, "warn"
	if strings.TrimSpace(text) != "" {
		a.emit(protocol.Event{SessionKey: sk, Type: "notice", TurnID: n.TurnID, Level: level, Text: truncHead(text, 1000)})
	}
	return true
}
