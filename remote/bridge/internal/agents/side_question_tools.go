package agents

import (
	"context"

	"salcara/bridge/internal/protocol"
)

// owner is an internal Bridge source scope, never a field accepted from a phone.
// liveValidate must retain source identity without a single request's cancellation.
type SideQuestionToolProvider interface {
	SideQuestionTools(context.Context, string, string, SideQuestionSnapshot, protocol.SideQuestionToolRequest, func() error) (protocol.SideQuestionToolState, error)
}

// Private workers participate in normal project write admission without being
// published in the session catalog or the main conversation timeline.
type SideQuestionToolWorkspaceProvider interface {
	SideQuestionToolWorkspaces() []protocol.SessionInfo
}

func ValidSideQuestionToolRequest(r protocol.SideQuestionToolRequest) bool {
	if !ValidControlOperationID(r.ChainID) || !ValidTurnID(r.ExpectedRevision) {
		return false
	}
	if r.Action == "read" {
		if r.OperationID != "" {
			return false
		}
	} else if !ValidControlOperationID(r.OperationID) {
		return false
	}
	if r.Permission != "" && r.Action != "open" || r.Question != "" && r.Action != "send" || r.Model != "" && r.Action != "send" || r.Effort != "" && r.Action != "send" || (r.CollaborationMode != "" || len(r.Skills) > 0) && r.Action != "send" || r.ExpectedTurnID != "" && r.Action != "stop" && r.Action != "approve" || r.ApprovalID != "" && r.Action != "approve" || r.Decision != "" && r.Action != "approve" {
		return false
	}
	switch r.Action {
	case "open":
		return r.Permission == "workspace-write"
	case "read", "features", "close":
		return true
	case "send":
		return workflowText(r.Question, 4000, true) && (r.Model == "" || workflowText(r.Model, 200, false)) && protocol.ValidModelEffort(r.Effort) && ValidateTurnFeatures(TurnOptions{CollaborationMode: r.CollaborationMode, Skills: r.Skills}) == nil
	case "stop":
		return ValidTurnID(r.ExpectedTurnID)
	case "approve":
		return ValidTurnID(r.ExpectedTurnID) && workflowText(r.ApprovalID, 128, true) && (r.Decision == "allow" || r.Decision == "deny")
	}
	return false
}
