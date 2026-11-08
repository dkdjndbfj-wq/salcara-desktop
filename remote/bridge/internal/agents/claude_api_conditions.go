package agents

import (
	"encoding/json"
	"strconv"

	"salcara/bridge/internal/protocol"
)

// Native categories outrank incidental error prose. Never forward raw provider
// headers/body or reuse the previous retry's cause for a different turn result.
func claudeAPICondition(category string, status int) (string, string) {
	switch category {
	case "authentication_failed":
		return "upstream_unauthorized", "API 授权失败"
	case "oauth_org_not_allowed", "account_on_hold":
		return "upstream_forbidden", "API 访问被拒绝"
	case "billing_error":
		return "usage_limit", "API 额度不足"
	case "rate_limit":
		return "upstream_rate_limit", "API 暂时限流"
	case "overloaded":
		return "model_capacity", "模型服务暂时繁忙"
	case "invalid_request":
		return "upstream_bad_request", "API 请求被拒绝"
	case "model_not_found":
		return "", "当前 API 不支持该模型"
	case "server_error":
		return "upstream_internal_error", "API 服务暂时异常"
	case "max_output_tokens", "cloud_credential_error":
		return "unknown_execution_error", "Claude 请求暂时无法完成"
	case "", "unknown":
		// Older versions can report only a structured HTTP status.
	default:
		return "unknown_execution_error", "Claude 请求暂时无法完成"
	}
	switch status {
	case 400:
		return "upstream_bad_request", "API 请求被拒绝"
	case 401:
		return "upstream_unauthorized", "API 授权失败"
	case 402:
		return "usage_limit", "API 额度不足"
	case 403:
		return "upstream_forbidden", "API 访问被拒绝"
	case 429:
		return "upstream_rate_limit", "API 暂时限流"
	case 529:
		return "model_capacity", "模型服务暂时繁忙"
	}
	if status >= 500 && status <= 599 {
		return "upstream_internal_error", "API 服务暂时异常"
	}
	return "unknown_execution_error", "Claude 请求暂时无法完成"
}

func claudeAPIRetryEvent(l *claudeLine, sessionKey, turnID string) protocol.Event {
	category := ""
	if len(l.RetryError) > 0 && json.Unmarshal(l.RetryError, &category) != nil {
		category = "unsupported-category"
	}
	code, text := claudeAPICondition(category, l.RetryStatus)
	text += "，正在重试"
	// One item per admitted turn updates in place instead of flooding the
	// conversation. History uses native UUID when no live turn ID is available.
	id := "claude-api-retry:" + turnID
	if turnID == "" {
		id = "claude-api-retry:" + truncHead(l.UUID, 256)
	}
	if l.RetryAttempt > 0 && l.RetryAttempt <= 10000 {
		text += "（" + strconv.Itoa(l.RetryAttempt)
		if l.RetryMax >= l.RetryAttempt && l.RetryMax <= 10000 {
			text += "/" + strconv.Itoa(l.RetryMax)
		}
		text += "）"
	}
	return protocol.Event{SessionKey: sessionKey, Tool: "claude", TS: l.ts(), TurnID: turnID,
		ID: id, Type: "notice", Level: "warn", Text: text, ErrorCode: code, Retrying: true}
}
