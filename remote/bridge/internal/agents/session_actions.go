package agents

import (
	"context"
	"strings"
	"unicode"

	"salcara/bridge/internal/protocol"
)

type SessionActionProvider interface {
	SessionActions(context.Context, string, func() error) (protocol.SessionActions, error)
	ApplySessionAction(context.Context, string, protocol.SessionActionRequest, func() error) (protocol.SessionActionResult, error)
}

type ArchivedSessionProvider interface {
	ArchivedSessions(context.Context, string, int, func() error) ([]protocol.SessionInfo, string, error)
}

func ValidSessionAction(request protocol.SessionActionRequest) bool {
	if !ValidTurnID(request.ExpectedRevision) || !ValidControlOperationID(request.OperationID) {
		return false
	}
	switch request.Action {
	case "rename":
		if strings.TrimSpace(request.Name) != request.Name || request.Name == "" || len([]rune(request.Name)) > 80 {
			return false
		}
		for _, r := range request.Name {
			if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
				return false
			}
		}
		return true
	case "archive", "unarchive", "fork":
		return request.Name == ""
	default:
		return false
	}
}
