package hubclient

import (
	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/protocol"
)

const defaultQueueBytes = 8 << 20
const maxEventWeight = 512 << 10
const maxReferencedEventWeight = 1536 << 10
const maxUploadWeight = 512 << 10 // leave space for recovery metadata under the Hub's 2 MiB body limit

func eventWeightLimit(event protocol.Event) int {
	if event.Type == "message" && event.Role == "user" && (len(event.ContextReferences) > 0 || len(event.Skills) > 0) && agents.ValidateTurnFeatures(agents.TurnOptions{Skills: event.Skills, ContextReferences: event.ContextReferences}) == nil {
		return maxReferencedEventWeight
	}
	return maxEventWeight
}

// Conservative retained/wire accounting. Six bytes per source byte covers JSON
// escaping; fixed overhead includes the event and its optional session/usage.
// This is a queue budget, not a promise about total Bridge process RSS.
func eventWeight(event protocol.Event) int {
	n := len(event.DeviceID) + len(event.SessionKey) + len(event.Tool) + len(event.Type) + len(event.ID) +
		len(event.Role) + len(event.Text) + len(event.TurnID) + len(event.Kind) + len(event.Title) +
		len(event.Detail) + len(event.Status) + len(event.Output) + len(event.Diff) + len(event.Cwd) +
		len(event.ApprovalID) + len(event.Decision) + len(event.By) + len(event.Error) + len(event.Level) + len(event.ParentID) + len(event.QuestionMode) +
		len(event.ClientID) + len(event.MessagePhase) + len(event.ErrorCode) + len(event.ApprovalTransport) + len(event.NoticeKind)
	if event.Progress {
		n += 16
	}
	if event.MessageOrigin != nil {
		n += len(event.MessageOrigin.Kind) + len(event.MessageOrigin.SourceSessionKey) + len(event.MessageOrigin.SourceTitle) + len(event.MessageOrigin.TargetSessionKey) + len(event.MessageOrigin.ReceiptID) + len(event.MessageOrigin.Verification) + 160
	}
	if context := event.Context; context != nil {
		n += len(context.SessionKey) + len(context.Revision) + len(context.UnavailableReason) + 128
		if snapshot := context.Snapshot; snapshot != nil {
			n += len(snapshot.Source) + len(snapshot.Basis) + len(snapshot.WindowKind) + len(snapshot.Model) + 192
		}
		if operation := context.Compaction; operation != nil {
			n += len(operation.OperationID) + len(operation.State) + len(operation.Error) + 128
		}
	}
	if options := event.ApprovalOptions; options != nil {
		n += 64 + len(options.DenyEffect)
		for _, decision := range options.Decisions {
			n += len(decision) + 8
		}
	}
	for _, ref := range event.Attachments {
		n += len(ref.ID) + len(ref.Mime) + len(ref.Name) + len(ref.SHA256) + 128
	}
	for _, ref := range event.Skills {
		n += len(ref.Name) + len(ref.Path) + len(ref.DisplayName) + len(ref.BrandColor) + len(ref.IconSmall) + 80
	}
	for _, ref := range event.ContextReferences {
		n += len(ref.Kind) + len(ref.SessionKey) + len(ref.ProjectPath) + len(ref.Label) + len(ref.Text) + len(ref.Path) + len(ref.MessageID) + len(ref.TextBasis) + len(ref.SourceRevision) + 256
		if ref.Annotation != nil {
			n += len(*ref.Annotation)
		}
	}
	for _, key := range event.ChildSessionKeys {
		n += len(key) + 16
	}
	for _, q := range event.Questions {
		n += len(q.ID) + len(q.Header) + len(q.Question) + len(q.InputType) + 128
		for _, option := range q.Options {
			n += len(option.Label) + len(option.Description) + 32
		}
	}
	if session := event.Session; session != nil {
		n += len(session.SessionKey) + len(session.Tool) + len(session.Client) + len(session.ControlSurface) +
			len(session.Title) + len(session.Cwd) + len(session.Status) + len(session.Model) + len(session.ParentSessionKey) +
			len(session.AgentNickname) + len(session.AgentRole) + len(session.ForkedFromSessionKey) + len(session.SessionScope)
	}
	return 1536 + 6*n
}

func (c *Client) trimQueueLocked() {
	start := c.inFlight
	if start > len(c.queue) {
		start = len(c.queue)
	}
	end := start
	for len(c.queue)-(end-start) > c.o.QueueCap || c.queueBytes > c.o.QueueBytes {
		if end == len(c.queue) {
			break
		} // never mutate an upload already in flight
		c.queueBytes -= eventWeight(c.queue[end])
		end++
	}
	if end == start {
		return
	}
	c.noteDroppedLocked(c.queue[start:end])
	remaining := make([]protocol.Event, 0, len(c.queue)-(end-start))
	remaining = append(remaining, c.queue[:start]...)
	c.queue = append(remaining, c.queue[end:]...)
}

func uploadCount(queue []protocol.Event) int {
	n, size := 0, 0
	for _, event := range queue {
		weight := eventWeight(event)
		if n == 200 || n > 0 && size+weight > maxUploadWeight {
			break
		}
		n++
		size += weight
	}
	return n
}
