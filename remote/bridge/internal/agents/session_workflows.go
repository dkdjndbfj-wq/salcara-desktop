package agents

import (
	"context"
	"strings"
	"unicode"

	"salcara/bridge/internal/protocol"
)

type SessionWorkflowProvider interface {
	SessionWorkflows(context.Context, string, func() error) (protocol.SessionWorkflows, error)
	ApplySessionWorkflow(context.Context, string, protocol.SessionWorkflowRequest, func() error) (protocol.SessionWorkflowResult, error)
}

func workflowText(value string, maximum int, multiline bool) bool {
	if value == "" || value != strings.TrimSpace(value) || len([]rune(value)) > maximum {
		return false
	}
	for _, r := range value {
		if unicode.In(r, unicode.Cf) || unicode.IsControl(r) && !(multiline && (r == '\n' || r == '\t')) {
			return false
		}
	}
	return true
}

func ValidSessionWorkflow(request protocol.SessionWorkflowRequest) bool {
	if !ValidTurnID(request.ExpectedRevision) || !ValidControlOperationID(request.OperationID) {
		return false
	}
	if request.TokenBudget != nil && (*request.TokenBudget <= 0 || *request.TokenBudget > 9007199254740991) {
		return false
	}
	if request.Kind == "review" {
		if request.Target == nil || request.Objective != "" || request.TokenBudget != nil {
			return false
		}
		target := request.Target
		switch target.Type {
		case "uncommittedChanges":
			return target.Branch == "" && target.SHA == "" && target.Instructions == ""
		case "baseBranch":
			return workflowText(target.Branch, 256, false) && !strings.HasPrefix(target.Branch, "-") && target.SHA == "" && target.Instructions == ""
		case "commit":
			if len(target.SHA) < 7 || len(target.SHA) > 64 || target.Branch != "" || target.Instructions != "" {
				return false
			}
			for _, r := range target.SHA {
				if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
					return false
				}
			}
			return true
		case "custom":
			return workflowText(target.Instructions, 4000, true) && target.Branch == "" && target.SHA == ""
		default:
			return false
		}
	}
	if request.Target != nil {
		return false
	}
	switch request.Kind {
	case "goal.set":
		return workflowText(request.Objective, 4000, true)
	case "goal.pause", "goal.resume", "goal.clear":
		return request.Objective == "" && request.TokenBudget == nil
	default:
		return false
	}
}
