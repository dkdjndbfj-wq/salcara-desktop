package agents

import (
	"sort"
	"strings"

	"salcara/bridge/internal/protocol"
)

// These fields are the app-server v2 CollabAgentState, not inferred tool outcomes.
type cxAgentState struct {
	Status  string  `json:"status"`
	Message *string `json:"message"`
}

func validChildID(id string) bool {
	if id == "" || len(id) > 256 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func codexChildIDs(it cxItem) []string {
	seen := map[string]bool{}
	var ids []string
	for _, id := range it.ReceiverThreadIDs {
		if validChildID(id) && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
		if len(ids) >= 256 {
			return ids
		}
	}
	var extra []string
	for id := range it.AgentsStates {
		if validChildID(id) && !seen[id] {
			extra = append(extra, id)
		}
	}
	sort.Strings(extra)
	for _, id := range extra {
		ids = append(ids, id)
		if len(ids) >= 256 {
			break
		}
	}
	return ids
}

func codexSubagentTitle(tool string) string {
	switch tool {
	case "spawnAgent":
		return "启动子智能体"
	case "wait":
		return "等待子智能体"
	case "sendInput", "sendMessage":
		return "发送子任务消息"
	case "followupTask":
		return "继续子任务"
	case "resumeAgent":
		return "恢复子智能体"
	case "closeAgent":
		return "关闭子智能体"
	case "interruptAgent":
		return "中断子智能体"
	case "listAgents":
		return "查看子智能体"
	}
	return titleText("子智能体 · " + tool)
}

func codexAgentStatus(status, message string) (string, string) {
	state, prefix := "running", ""
	switch status {
	case "pendingInit":
		prefix = "正在启动"
	case "running", "started", "interacted":
	case "completed":
		state = "done"
	case "shutdown":
		state, prefix = "done", "已关闭"
	case "interrupted":
		state, prefix = "failed", "已中断"
	case "errored":
		state, prefix = "failed", "子任务失败"
	case "notFound":
		state, prefix = "failed", "子会话不可用"
	default:
		prefix = "状态未返回"
	}
	return state, truncTail(strings.TrimSpace(prefix+"\n"+message), maxOutputChars)
}

func codexSubagentEvents(sk string, it cxItem) []protocol.Event {
	if it.Type != "collabAgentToolCall" {
		return nil
	}
	var out []protocol.Event
	for _, id := range codexChildIDs(it) {
		st := it.AgentsStates[id]
		message := ""
		if st.Message != nil {
			message = *st.Message
		}
		status, output := codexAgentStatus(st.Status, message)
		e := protocol.Event{SessionKey: sk, Tool: "codex", TS: nowMs(), Type: "tool", Kind: "subagent",
			ID: it.ID + ":child:" + id, ParentID: it.ID, Title: titleText("子智能体 · " + id), Status: status, Output: output}
		if it.Prompt != nil {
			e.Detail = truncHead(*it.Prompt, maxDetailChars)
		}
		if st.Status != "notFound" {
			e.ChildSessionKeys = []string{"codex:" + id}
		}
		out = append(out, e)
	}
	return out
}
