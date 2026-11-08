package gateway

import (
	"encoding/json"
	"strings"
)

// ValidMessagesEnvelope leaves content/tool JSON intact but rejects ambiguous
// envelope fields before named-client ACL/alias resolution.
func ValidMessagesEnvelope(body []byte) bool {
	if !chatUniqueJSON(body) {
		return false
	}
	obj := chatObject(body)
	if obj == nil {
		return false
	}
	for key := range obj {
		for _, canonical := range []string{"model", "messages", "system", "tools", "tool_choice", "max_tokens", "thinking", "stream", "metadata", "output_config"} {
			if key != canonical && strings.EqualFold(key, canonical) {
				return false
			}
		}
	}
	var model string
	var messages []json.RawMessage
	if json.Unmarshal(obj["model"], &model) != nil || model == "" || json.Unmarshal(obj["messages"], &messages) != nil || len(messages) == 0 {
		return false
	}
	if raw, ok := obj["stream"]; ok {
		var stream bool
		if string(raw) == "null" || json.Unmarshal(raw, &stream) != nil {
			return false
		}
	}
	return true
}
