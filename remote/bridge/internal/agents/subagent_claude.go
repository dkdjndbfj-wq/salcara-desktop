package agents

import (
	"strings"

	"salcara/bridge/internal/protocol"
)

// Claude's agentId is not a standalone resumable session_id. Keep child output inside its
// real parent's timeline and do not manufacture a childSessionKey from a tool-use identifier.
func (m *claudeMapper) subagentEntry(l *claudeLine) []protocol.Event {
	parent := strings.TrimSpace(*l.ParentTU)
	if parent == "" || len(parent) > 512 {
		return nil
	}
	child := m.children[parent]
	if child == nil {
		if len(m.children) >= 256 {
			return nil
		}
		child = newClaudeMapper(m.sk, m.cwd)
		child.parentID = parent
		m.children[parent] = child
	}
	if l.Cwd != "" {
		child.cwd = l.Cwd
	}
	copyLine := *l
	copyLine.ParentTU, copyLine.IsSidechain = nil, false
	ts := l.ts()
	switch l.Type {
	case "assistant":
		fallback := l.UUID
		if fallback == "" {
			fallback = newID("m_")
		}
		return child.assistant(l.Message, fallback, ts)
	case "user":
		return child.user(&copyLine, l.UUID, ts)
	case "stream_event":
		return child.stream(l.Event)
	case "result":
		e := m.base(ts)
		if tu := m.tools[parent]; tu != nil {
			e = tu.ev
		}
		e.Type, e.Kind, e.ID, e.TS = "tool", "subagent", parent, ts
		if e.Title == "" {
			e.Title = "子任务"
		}
		e.Status, e.Output = "done", truncTail(l.Result, maxOutputChars)
		if l.IsError || l.Subtype != "" && l.Subtype != "success" {
			e.Status = "failed"
			if e.Output == "" {
				e.Output = truncTail(strings.Join(l.Errors, "\n"), maxOutputChars)
			}
		}
		if tu := m.tools[parent]; tu != nil {
			tu.ev = e
		}
		delete(m.activeTasks, parent)
		return []protocol.Event{e}
	case "system":
		if evs, handled := child.taskLifecycle(&copyLine); handled {
			return evs
		}
	}
	return nil
}

func (m *claudeMapper) hasActiveSubtasks() bool {
	for _, active := range m.activeTasks {
		if active {
			return true
		}
	}
	for _, tu := range m.tools {
		if tu.ev.Kind == "subagent" && tu.ev.Status == "running" {
			return true
		}
	}
	return false
}

// SDK system task events provide the true background-task lifecycle. A successful Agent
// tool result may only acknowledge a launch and must not mark such a task as complete.
func (m *claudeMapper) taskLifecycle(l *claudeLine) ([]protocol.Event, bool) {
	switch l.Subtype {
	case "task_started", "task_progress", "task_notification", "task_updated":
	default:
		return nil, false
	}
	id := l.ToolUseID
	if id == "" {
		id = m.taskTools[l.TaskID]
	}
	if id == "" {
		if l.TaskID == "" || len(l.TaskID) > 512 {
			return nil, true
		}
		id = "task:" + l.TaskID
	}
	if len(id) > 512 {
		return nil, true
	}
	if l.TaskID != "" && len(m.taskTools) < 4000 {
		m.taskTools[l.TaskID] = id
	}
	e := m.base(l.ts())
	if tu := m.tools[id]; tu != nil {
		e = tu.ev
	}
	e.Type, e.ID, e.TS = "tool", id, l.ts()
	if e.Kind == "" {
		e.Kind, e.Title = "other", "后台任务"
	}
	if l.Subtype == "task_progress" && (e.Status == "done" || e.Status == "failed") {
		return nil, true
	}
	if l.Description != "" && e.Kind != "subagent" {
		e.Title = titleText("后台任务 · " + l.Description)
	}
	status := l.Status
	if l.Subtype == "task_updated" && status == "" {
		status = str(l.Patch, "status")
	}
	if status == "" && l.Subtype == "task_updated" {
		// A patch without status must not reopen a completed task or claim a terminal result.
		if e.Status == "" {
			return nil, true
		}
	} else {
		e.Status = "running"
		switch status {
		case "completed":
			e.Status = "done"
		case "failed", "stopped", "killed":
			e.Status = "failed"
		}
	}
	if l.Summary != "" {
		e.Output = truncTail(l.Summary, maxOutputChars)
	}
	if l.LastToolName != "" {
		e.Output = titleText("正在使用 " + l.LastToolName)
	}
	if status == "stopped" || status == "killed" {
		e.Output = strings.TrimSpace("已停止\n" + e.Output)
	}
	if l.Usage != nil && l.Usage.DurationMS > 0 {
		e.DurationMS = l.Usage.DurationMS
	}
	if e.Status == "running" {
		if len(m.activeTasks) < 4000 {
			m.activeTasks[id] = true
		}
	} else {
		delete(m.activeTasks, id)
	}
	if tu := m.tools[id]; tu != nil {
		tu.ev = e
	}
	return []protocol.Event{e}, true
}
