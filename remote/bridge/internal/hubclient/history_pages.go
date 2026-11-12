package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/protocol"
)

func pageArguments(cmd map[string]any, maximum int) (string, int, error) {
	cursor, limit := "", maximum
	if raw, exists := cmd["cursor"]; exists {
		value, ok := raw.(string)
		if !ok || len(value) > 4096 {
			return "", 0, errors.New("读取位置无效")
		}
		cursor = value
	}
	if raw, exists := cmd["limit"]; exists {
		switch value := raw.(type) {
		case int:
			limit = value
		case float64:
			if value != float64(int(value)) {
				return "", 0, errors.New("分页数量无效")
			}
			limit = int(value)
		default:
			return "", 0, errors.New("分页数量无效")
		}
		if limit < 1 || limit > maximum {
			return "", 0, errors.New("分页数量无效")
		}
	}
	return cursor, limit, nil
}

func messagePageLimit(cmd map[string]any) (int, error) {
	if raw, exists := cmd["messageLimit"]; exists {
		_, limit, err := pageArguments(map[string]any{"limit": raw}, 10)
		return limit, err
	}
	return 0, nil
}
func dispatchSessionsPage(ctx context.Context, m agents.Manager, cmd map[string]any) (map[string]any, error) {
	tool := str(cmd, "tool")
	client := str(cmd, "client")
	if tool != "" && m.Get(tool) == nil {
		return nil, errors.New("电脑没有这个 Agent")
	}
	// `client` is a Claude Desktop/Code partition, not a generic adapter
	// selector. Extension Agents must expose their own session list without
	// being forced through Claude's client names.
	if client != "" && (tool != "claude" || client != "code" && client != "desktop-code") {
		return nil, errors.New("会话类型无效")
	}
	cursor, limit, err := pageArguments(cmd, maxSessions)
	if err != nil {
		return nil, err
	}
	if tool == "" {
		if cursor != "" {
			return nil, errors.New("请先选择 Agent")
		}
		return ListSessions(ctx, m, "")
	}
	agent := m.Get(tool)
	if err := requireAgentCapability(agent, "list"); err != nil {
		return nil, err
	}
	if pager, ok := agent.(agents.SessionPager); ok {
		sessions, next, err := pager.SessionsPage(ctx, cursor, limit, client)
		if err != nil {
			return nil, err
		}
		if len(sessions) > limit || len(next) > 4096 {
			return nil, errors.New("Agent 返回的会话目录超过读取上限")
		}
		if err := requireAgentCapability(agent, "list"); err != nil {
			return nil, err
		}
		d, err := cliAgentDescriptor(agent)
		if err != nil {
			return nil, err
		}
		sessions = append([]protocol.SessionInfo(nil), sessions...)
		for index := range sessions {
			if !agents.SessionBelongsTo(agent, sessions[index].SessionKey, sessions[index]) {
				return nil, errors.New("Agent 返回的会话目录身份不一致")
			}
			sessions[index] = agents.ProjectSessionInfo(sessions[index], agent.ID(), d.Capabilities)
			agents.ApplyRememberedWorkspaceMode(m, &sessions[index])
		}
		result := map[string]any{"sessions": sessions, "nextCursor": next, "pagination": "v1"}
		encoded, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		if len(encoded) > 1<<20 {
			return nil, errors.New("会话目录过大，请在电脑整理后刷新")
		}
		return result, nil
	}
	if cursor != "" {
		return nil, errors.New("请更新电脑端以加载更多对话")
	}
	result, err := ListSessions(ctx, m, tool)
	if err != nil {
		return nil, err
	}
	if client != "" {
		all, _ := result["sessions"].([]protocol.SessionInfo)
		filtered := []protocol.SessionInfo{}
		for _, info := range all {
			agents.ApplyRememberedWorkspaceMode(m, &info)
			if (info.Client == "Claude Desktop") == (client == "desktop-code") {
				filtered = append(filtered, info)
			}
		}
		result["sessions"] = filtered
	}
	return result, nil
}
func dispatchHistoryPage(ctx context.Context, m agents.Manager, cmd map[string]any) (map[string]any, error) {
	return dispatchHistoryPageWithAttachments(ctx, m, cmd, phoneAttachments)
}

func dispatchHistoryPageWithAttachments(ctx context.Context, m agents.Manager, cmd map[string]any, attachments *attachmentStore) (map[string]any, error) {
	key := str(cmd, "sessionKey")
	agent, id, err := agentFor(m, key)
	if err != nil {
		return nil, err
	}
	if err := requireAgentCapability(agent, "read"); err != nil {
		return nil, err
	}
	if err := validateAdapterCommand(agent, cmd); err != nil {
		return nil, err
	}
	messageLimit, err := messagePageLimit(cmd)
	if err != nil {
		return nil, err
	}
	cursor, limit, err := pageArguments(cmd, maxOpenEvents)
	if err != nil {
		return nil, err
	}
	var info protocol.SessionInfo
	var events []protocol.Event
	next := ""
	paged := false
	if pager, ok := agent.(agents.MessageHistoryPager); ok && messageLimit != 0 {
		info, events, next, err = pager.OpenMessagesPage(ctx, id, cursor, messageLimit)
		paged = true
	} else if pager, ok := agent.(agents.HistoryPager); ok {
		info, events, next, err = pager.OpenPage(ctx, id, cursor, limit)
		paged = true
	} else {
		if cursor != "" {
			return nil, errors.New("请更新电脑端以加载更早记录")
		}
		info, events, err = agent.Open(ctx, id)
	}
	if err != nil {
		return nil, err
	}
	if !agents.SessionBelongsTo(agent, key, info) {
		return nil, errors.New("会话身份不一致，已取消读取")
	}
	if err := requireAgentCapability(agent, "read"); err != nil {
		return nil, err
	}
	d, err := cliAgentDescriptor(agent)
	if err != nil {
		return nil, err
	}
	controlEvents := 0
	for _, event := range events {
		if event.Type == "session.reverted" && agents.ValidTurnID(event.TurnID) && agents.ValidTurnID(event.ID) && agents.ValidControlOperationID(event.OperationID) {
			controlEvents++
		}
	}
	if len(next) > 4096 || controlEvents > 256 || paged && len(events)-controlEvents > maxOpenEvents {
		return nil, errors.New("Agent 返回的历史超过读取上限")
	}
	filtered := []protocol.Event{}
	for _, event := range events {
		if event.Tool != "" && event.Tool != agent.ID() || event.SessionKey != "" && event.SessionKey != key || event.Session != nil && !agents.SessionBelongsTo(agent, key, *event.Session) {
			continue
		}
		if event.Type == "approval.request" && (event.Kind == "question" && !d.Capabilities.Questions || event.Kind != "question" && !d.Capabilities.Approval) {
			continue
		}
		agents.ProjectEventMedia(&event, d.Capabilities)
		decorateEventMediaWith(attachments, &event)
		event.SessionKey = key
		event.Tool = agent.ID()
		event = protocol.NormalizeTimelineEvent(event)
		if event.Session != nil {
			copy := *event.Session
			copy = agents.ProjectSessionInfo(copy, agent.ID(), d.Capabilities)
			agents.ApplyRememberedWorkspaceMode(m, &copy)
			event.Session = &copy
		}
		filtered = append(filtered, event)
	}
	if !paged && len(filtered) > limit {
		filtered = filtered[len(filtered)-limit:]
	}
	info = agents.ProjectSessionInfo(info, agent.ID(), d.Capabilities)
	agents.ApplyRememberedWorkspaceMode(m, &info)
	result := map[string]any{"session": info, "events": filtered}
	if paged {
		result["nextCursor"] = next
		result["pagination"] = "v1"
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	if len(encoded) > 1<<20 {
		return nil, errors.New("历史内容过大，请在电脑查看")
	}
	return result, nil
}
