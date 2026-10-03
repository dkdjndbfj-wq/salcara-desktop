package hubclient

import "salcara/bridge/internal/protocol"

const defaultQueueBytes = 8 << 20
const maxEventWeight = 512 << 10
const maxUploadWeight = 512 << 10 // leave space for recovery metadata under the Hub's 2 MiB body limit

// Conservative retained/wire accounting. Six bytes per source byte covers JSON
// escaping; fixed overhead includes the event and its optional session/usage.
// This is a queue budget, not a promise about total Bridge process RSS.
func eventWeight(event protocol.Event) int {
	n := len(event.DeviceID) + len(event.SessionKey) + len(event.Tool) + len(event.Type) + len(event.ID) +
		len(event.Role) + len(event.Text) + len(event.TurnID) + len(event.Kind) + len(event.Title) +
		len(event.Detail) + len(event.Status) + len(event.Output) + len(event.Diff) + len(event.Cwd) +
		len(event.ApprovalID) + len(event.Decision) + len(event.By) + len(event.Error) + len(event.Level) + len(event.ParentID) + len(event.QuestionMode)
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
			len(session.Title) + len(session.Cwd) + len(session.Status) + len(session.Model) + len(session.ParentSessionKey)
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
