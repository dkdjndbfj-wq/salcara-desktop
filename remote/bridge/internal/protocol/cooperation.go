package protocol

import "strings"

// These fields describe a Bridge-owned user send receipt, not a native agent
// collaboration API. They are never accepted as phone-authored provenance.
func ValidCrossTaskOrigin(origin *MessageOrigin, target string) bool {
	if origin == nil {
		return true
	}
	return origin.Kind == "user_cross_task" && origin.Verification == "bridge_receipt" &&
		origin.TargetSessionKey == target && ValidCooperationSessionKey(target) &&
		ValidCooperationSessionKey(origin.SourceSessionKey) && origin.SourceSessionKey != target &&
		strings.SplitN(origin.SourceSessionKey, ":", 2)[0] == strings.SplitN(target, ":", 2)[0] &&
		len(origin.ReceiptID) == 36 && len(origin.SourceTitle) <= 640 && !strings.ContainsAny(origin.SourceTitle, "\x00\r\n")
}

func ValidCooperationSessionKey(key string) bool {
	parts := strings.SplitN(key, ":", 2)
	if len(parts) != 2 || parts[0] != "codex" && parts[0] != "claude" || len(parts[1]) == 0 || len(parts[1]) > 160 {
		return false
	}
	for _, c := range parts[1] {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func CloneMessageOrigin(origin *MessageOrigin) *MessageOrigin {
	if origin == nil {
		return nil
	}
	copy := *origin
	return &copy
}

type CooperationPreparation struct {
	Ticket    string      `json:"ticket"`
	ExpiresAt int64       `json:"expiresAt"`
	Source    SessionInfo `json:"source"`
	Target    SessionInfo `json:"target"`
}
