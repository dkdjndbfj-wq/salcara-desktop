package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/protocol"
)

type ReadOnlyDesktopHistory interface {
	HistoryIdentity() string
	Describe(context.Context, string) (protocol.SessionInfo, error)
	SessionsPage(context.Context, string, int, string) ([]protocol.SessionInfo, string, error)
	OpenPage(context.Context, string, string, int) (protocol.SessionInfo, []protocol.Event, string, error)
}

type readOnlyHistoryStatus struct {
	Available     bool     `json:"available"`
	ReadOnly      bool     `json:"readOnly"`
	Scopes        []string `json:"scopes"`
	SchemaVersion string   `json:"schemaVersion"`
	Identity      string   `json:"identity"`
}

var readonlyClaudeDesktopKey = regexp.MustCompile(`^claude-desktop:local_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var readonlyClaudeDesktopIdentity = regexp.MustCompile(`^[0-9a-f]{64}$`)

// This entry point must precede generic CLI/native dispatch. A native local_UUID
// must never silently become a CLI resumable session, even if a stale/modified
// phone sends a missing or different controlSurface value.
func (c *Client) dispatchClaudeDesktopHistory(ctx context.Context, cmd map[string]any) (bool, any, error) {
	typ, key, surface, scope := str(cmd, "type"), str(cmd, "sessionKey"), str(cmd, "controlSurface"), str(cmd, "client")
	localKey := strings.HasPrefix(key, "claude-desktop:")
	desktopScope := scope == "desktop-chat" || scope == "desktop-cowork"
	if !localKey && !desktopScope {
		if surface == "read-only" {
			return true, nil, errors.New("不受支持的只读历史范围")
		}
		return false, nil, nil
	}
	fail := func(message string) (bool, any, error) { return true, nil, errors.New(message) }
	if typ != "sessions.list" && typ != "session.open" && typ != "session.describe" {
		return fail("Claude Desktop Chat/Cowork 当前只支持查看本地记录")
	}
	if surface != "read-only" {
		return fail("Claude Desktop Chat/Cowork 需要只读历史入口")
	}
	if typ == "sessions.list" && (str(cmd, "tool") != "claude" || !desktopScope || key != "") {
		return fail("Claude Desktop 只读会话类型无效")
	}
	if typ == "session.open" && (!readonlyClaudeDesktopKey.MatchString(key) || !desktopScope || str(cmd, "tool") != "" && str(cmd, "tool") != "claude") {
		return fail("Claude Desktop 只读会话编号无效")
	}
	if typ == "session.describe" && (!readonlyClaudeDesktopKey.MatchString(key) || scope != "" && !desktopScope || str(cmd, "tool") != "" && str(cmd, "tool") != "claude") {
		return fail("Claude Desktop 只读会话编号无效")
	}
	if c.o.ClaudeDesktopHistory == nil {
		return fail("电脑尚未启用 Claude Desktop 本地历史读取")
	}
	cursor, limit := "", 0
	if typ != "session.describe" {
		var err error
		cursor, limit, err = pageArguments(cmd, maxOpenEvents)
		if typ == "sessions.list" {
			cursor, limit, err = pageArguments(cmd, maxSessions)
		}
		if err != nil {
			return true, nil, err
		}
	}
	provider, err := c.o.ClaudeDesktopHistory(ctx)
	if err != nil || provider == nil {
		return fail("Claude Desktop 当前第三方模式或本地历史不可用")
	}
	identity := provider.HistoryIdentity()
	if !readonlyClaudeDesktopIdentity.MatchString(identity) || str(cmd, "historyIdentity") != identity {
		return fail("Claude Desktop 本地历史范围已变化，请刷新 Agent 后重试")
	}
	var result map[string]any
	if typ == "session.describe" {
		info, err := provider.Describe(ctx, strings.TrimPrefix(key, "claude-desktop:"))
		if err != nil {
			return fail("Claude Desktop 本地会话信息读取失败，请检查原应用后重试")
		}
		validScope := info.SessionScope == "desktop-chat" || info.SessionScope == "desktop-cowork"
		if info.SessionKey != key || !validScope || scope != "" && info.SessionScope != scope || info.Tool != "claude" || info.Client != "Claude Desktop" || info.ControlSurface != "read-only" || info.Controllable || info.ControlExpiresAt != 0 {
			return fail("Claude Desktop 只读会话身份无法验证")
		}
		result = map[string]any{"session": info}
	} else if typ == "sessions.list" {
		sessions, next, err := provider.SessionsPage(ctx, cursor, limit, scope)
		if err != nil {
			return fail("Claude Desktop 本地会话目录读取失败，请检查原应用后重试")
		}
		if len(sessions) > limit {
			return fail("Claude Desktop 本地会话目录超过读取上限")
		}
		for _, session := range sessions {
			if !readonlyClaudeDesktopKey.MatchString(session.SessionKey) || session.SessionScope != scope || session.Tool != "claude" || session.Client != "Claude Desktop" || session.ControlSurface != "read-only" || session.Controllable || session.ControlExpiresAt != 0 {
				return fail("Claude Desktop 只读会话身份无法验证")
			}
		}
		result = map[string]any{"sessions": sessions, "nextCursor": next, "pagination": "v1"}
	} else {
		id := strings.TrimPrefix(key, "claude-desktop:")
		messageLimit, err := messagePageLimit(cmd)
		if err != nil {
			return true, nil, err
		}
		var info protocol.SessionInfo
		var events []protocol.Event
		var next string
		if pager, ok := provider.(agents.MessageHistoryPager); ok && messageLimit != 0 {
			info, events, next, err = pager.OpenMessagesPage(ctx, id, cursor, messageLimit)
		} else {
			info, events, next, err = provider.OpenPage(ctx, id, cursor, limit)
		}
		if err != nil {
			return fail("Claude Desktop 本地历史读取失败，请检查原应用后重试")
		}
		if info.SessionKey != key || info.SessionScope != scope || info.Tool != "claude" || info.Client != "Claude Desktop" || info.ControlSurface != "read-only" || info.Controllable || info.ControlExpiresAt != 0 || len(events) > limit {
			return fail("Claude Desktop 只读历史身份无法验证")
		}
		for _, event := range events {
			if event.SessionKey != key || event.Tool != "claude" || event.Session != nil && (event.Session.SessionKey != key || event.Session.Tool != "claude" || event.Session.Client != "Claude Desktop" || event.Session.SessionScope != scope || event.Session.Controllable || event.Session.ControlSurface != "read-only" || event.Session.ControlExpiresAt != 0) || event.Type == "approval.request" || event.ApprovalID != "" || len(event.ChildSessionKeys) > 0 {
				return fail("Claude Desktop 本地历史含未经授权的操作记录")
			}
		}
		result = map[string]any{"session": info, "events": events, "nextCursor": next, "pagination": "v1"}
	}
	result["historyIdentity"] = identity
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > 1<<20 {
		return fail("Claude Desktop 本地历史超过传输上限")
	}
	return true, result, nil
}

func (c *Client) claudeReadOnlyHistoryStatus(ctx context.Context) *readOnlyHistoryStatus {
	if c.o.ClaudeDesktopHistory == nil {
		return nil
	}
	provider, err := c.o.ClaudeDesktopHistory(ctx)
	if err != nil || provider == nil {
		return nil
	}
	identity := provider.HistoryIdentity()
	if !readonlyClaudeDesktopIdentity.MatchString(identity) {
		return nil
	}
	return &readOnlyHistoryStatus{Available: true, ReadOnly: true, Scopes: []string{"desktop-chat", "desktop-cowork"}, SchemaVersion: "2.16120.0", Identity: identity}
}
