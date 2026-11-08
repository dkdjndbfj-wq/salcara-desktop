package agents

import (
	"context"
	"regexp"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

var sideQuestionAttachmentID = regexp.MustCompile(`^att_[a-f0-9]{32}$`)

type SideQuestionMessage struct{ ID, Role, Text string }

// Snapshots are supplied only by the Bridge's bounded history reader. They
// cannot be supplied as phone command fields or grant filesystem access.
type SideQuestionSnapshot struct {
	Revision  string
	Messages  []SideQuestionMessage
	Truncated bool
}

func validateSideQuestionModel(ctx context.Context, family, model string) error {
	if model == "" {
		return nil
	}
	cfg, ok := ctx.Value(configSnapshotKey{}).(config.Config)
	if !ok {
		return errSideQuestionStale
	}
	target, _, _ := workerGatewayForContext(ctx, cfg, family)
	account, ok := cfg.GatewaySourceAccount(target)
	if !ok || !config.ModelAllowed(account, model) {
		return errSideQuestionInvalid
	}
	// Bridge additionally checks the live catalog before dispatch. Adapters
	// still enforce the immutable account policy if called directly.
	return nil
}

type SideQuestionProvider interface {
	SideQuestionCatalog(context.Context, string, SideQuestionSnapshot, func() error) (protocol.SideQuestionCatalog, error)
	AskSideQuestion(context.Context, string, SideQuestionSnapshot, protocol.SideQuestionRequest, func() error) (protocol.SideQuestionResult, error)
}

func ValidSideQuestionRequest(sessionKey string, request protocol.SideQuestionRequest) bool {
	if request.PreviousOperationID != "" && (!ValidControlOperationID(request.PreviousOperationID) || request.Selection != nil) {
		return false
	}
	if !ValidControlOperationID(request.OperationID) || !ValidTurnID(request.ExpectedRevision) || !workflowText(request.Question, 4000, true) {
		return false
	}
	if request.Model != "" && !workflowText(request.Model, 200, false) || !protocol.ValidModelEffort(request.Effort) || len(request.Attachments) > 4 || len(request.Images) > 4 {
		return false
	}
	for _, id := range request.Attachments {
		if !sideQuestionAttachmentID.MatchString(id) {
			return false
		}
	}
	return request.Selection == nil || request.Selection.Kind == "message" && request.Selection.SessionKey == sessionKey && protocol.ValidateContextReferences([]protocol.ContextReference{*request.Selection}) == nil
}
