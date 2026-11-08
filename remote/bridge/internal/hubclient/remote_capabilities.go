package hubclient

import (
	"context"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/protocol"
)

type turnOptionsStarter interface {
	StartWithOptions(context.Context, string, string, string, agents.TurnOptions) (string, error)
}

type turnOptionsSender interface {
	SendWithOptions(context.Context, string, string, agents.TurnOptions) error
}

type scopedApprovalResponder interface {
	RespondForSession(context.Context, string, string, agents.ApprovalResponse) bool
}

type nativeApprovalResponder interface {
	NativeRespondApproval(context.Context, string, string, string, string, string) error
}

// publishedRemoteCapabilities intersects an adapter's declaration with the
// contracts Dispatch can actually call. A Go method alone grants no permission.
// Local legacy Start(model) remains valid; a conversation model picker also
// requires SendWithOptions, so later turns never silently discard the choice.
func publishedRemoteCapabilities(a agents.Agent, caps protocol.AgentCapabilities) protocol.AgentCapabilities {
	if a == nil {
		return protocol.AgentCapabilities{}
	}
	_, precise := a.(agents.PreciseInterrupter)
	_, reverter := a.(agents.LatestTurnReverter)
	caps.LatestTurnRevert = caps.LatestTurnRevert && caps.Read && caps.Send && reverter
	caps.PreciseInterrupt = caps.Interrupt && caps.PreciseInterrupt && precise
	// The remote protocol intentionally has no ID-less legacy interrupt route.
	caps.Interrupt = caps.PreciseInterrupt
	_, continuer := a.(agents.TerminalContinuer)
	caps.Continue = caps.Send && caps.Continue && continuer
	_, steerer := a.(agents.ActiveTurnSteerer)
	caps.Steer = caps.Send && caps.Steer && steerer
	_, responder := a.(scopedApprovalResponder)
	caps.Approval = caps.Approval && responder
	caps.Questions = caps.Questions && caps.Approval
	_, starter := a.(turnOptionsStarter)
	_, sender := a.(turnOptionsSender)
	caps.ContextReferences = caps.ContextReferences && caps.Send && starter && sender
	caps.ContextAnnotations = caps.ContextAnnotations && caps.ContextReferences
	caps.ContextDiffAnnotations = caps.ContextDiffAnnotations && caps.ContextReferences
	_, features := a.(agents.FeatureProvider)
	_, sideQuestion := a.(agents.SideQuestionProvider)
	caps.SideQuestion = caps.SideQuestion && caps.Read && caps.Send && sideQuestion
	_, sessionActions := a.(agents.SessionActionProvider)
	_, sessionWorkflows := a.(agents.SessionWorkflowProvider)
	caps.SessionWorkflows = caps.SessionWorkflows && caps.Read && caps.Send && sessionWorkflows
	_, archivedSessions := a.(agents.ArchivedSessionProvider)
	caps.SessionActions = caps.SessionActions && caps.Read && sessionActions
	caps.ArchivedSessions = caps.ArchivedSessions && caps.List && archivedSessions
	_, contextReader := a.(agents.ContextProvider)
	_, compactor := a.(agents.ContextCompactor)
	caps.Context = caps.Context && caps.Read && contextReader
	caps.Compact = caps.Compact && caps.Context && caps.Send && compactor
	caps.Features = caps.Features && caps.Send && features && starter && sender
	caps.ModelOverride = caps.Send && caps.ModelOverride && sender
	caps.StartModelOverride = caps.Send && caps.StartModelOverride && starter
	caps.ImageInput = caps.Send && caps.Attachments && caps.ImageInput && starter && sender
	caps.Attachments = caps.ImageInput
	// Only these values are understood by commandPermissionMode. Copy rather
	// than editing a descriptor-owned slice, which may be shared with a worker.
	modes := []string(nil)
	// The phone's permission selector currently implements the Codex contract
	// only. Shared API families do not grant extensions that UI/protocol mode.
	if a.ID() == "codex" && caps.Send && starter && sender {
		seen := map[string]bool{}
		for _, mode := range caps.PermissionModes {
			if (mode == "ask" || mode == "auto_review" || mode == "full_access") && !seen[mode] {
				modes = append(modes, mode)
				seen[mode] = true
			}
		}
	}
	caps.PermissionModes = modes
	approvalModes := []string(nil)
	if a.ID() == "claude" && caps.Send && starter && sender {
		seen := map[string]bool{}
		for _, mode := range caps.ApprovalModes {
			if (mode == "ask" || mode == "auto_edits" || mode == "auto_all") && !seen[mode] {
				approvalModes = append(approvalModes, mode)
				seen[mode] = true
			}
		}
	}
	caps.ApprovalModes = approvalModes
	return caps
}
