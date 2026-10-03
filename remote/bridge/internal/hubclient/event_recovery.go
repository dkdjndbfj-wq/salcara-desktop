package hubclient

import (
	"fmt"
	"sort"

	"salcara/bridge/internal/protocol"
)

// Metadata only: never keep dropped text, approvals, or tool output. Excess
// sessions get a recovery notice when their next retained event is uploaded;
// opening any session also reads its authoritative local history.
const maxRecoverySessions = 256
const historyGapPrefix = "[salcara:history-gap:v1]"

type recoveryMark struct {
	generation uint64
	tool       string
	overflow   uint64
}

// The overflow epoch deliberately survives an empty queue. We cannot know which
// untracked sessions lost events. A bounded acknowledgement cache avoids repeated
// notices for recently recovered sessions; eviction can cause an extra read, but
// can never silently suppress the first recovery of an untracked session.
func recoveryTool(tool string) string {
	if tool == "codex" || tool == "claude" {
		return tool
	}
	return ""
}

// All helpers run under qmu. A generation prevents an old upload acknowledgement
// from clearing a newer gap that occurred while that upload was in flight.
func (c *Client) noteDroppedLocked(events []protocol.Event) {
	c.recoveryGeneration++
	if c.recovery == nil {
		c.recovery = make(map[string]recoveryMark)
	}
	for _, event := range events {
		if event.SessionKey == "" || len(event.SessionKey) > 512 {
			continue
		}
		if _, exists := c.recovery[event.SessionKey]; exists || len(c.recovery) < maxRecoverySessions {
			c.recovery[event.SessionKey] = recoveryMark{generation: c.recoveryGeneration, tool: recoveryTool(event.Tool)}
		} else {
			c.recoveryAll = true
			c.recoveryOverflow = c.recoveryGeneration
			c.recoverySeen = nil
			c.recoverySeenOrder = nil
		}
	}
}

func (c *Client) recoveryBatchLocked(batch []protocol.Event) ([]protocol.Event, map[string]recoveryMark) {
	marks := make(map[string]recoveryMark)
	keys := make([]string, 0, len(c.recovery))
	for key := range c.recovery {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if len(marks) == 100 {
			break
		}
		marks[key] = c.recovery[key]
	}
	if c.recoveryAll {
		for _, event := range batch {
			if event.SessionKey != "" && len(event.SessionKey) <= 512 && c.recoverySeen[event.SessionKey] != c.recoveryOverflow {
				mark, exists := marks[event.SessionKey]
				if !exists {
					mark = recoveryMark{generation: c.recoveryGeneration, tool: recoveryTool(event.Tool)}
				}
				mark.overflow = c.recoveryOverflow
				marks[event.SessionKey] = mark
			}
		}
	}
	keys = keys[:0]
	for key := range marks {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]protocol.Event, 0, len(marks)+len(batch))
	for _, key := range keys {
		mark := marks[key]
		out = append(out, protocol.Event{
			Type: "notice", SessionKey: key, Tool: mark.tool, TS: nowMS(), Level: "warn",
			ID:   fmt.Sprintf("history-gap:%d", mark.generation),
			Text: historyGapPrefix + " 离线期间部分进度缓存已清理，正在重新读取电脑上的原会话。",
		})
	}
	return append(out, batch...), marks
}

func (c *Client) acknowledgeRecoveryLocked(marks map[string]recoveryMark) {
	for key, mark := range marks {
		if current, exists := c.recovery[key]; exists && current.generation == mark.generation && current.tool == mark.tool {
			delete(c.recovery, key)
		}
		if mark.overflow == 0 || mark.overflow != c.recoveryOverflow {
			continue
		}
		if c.recoverySeen == nil {
			c.recoverySeen = make(map[string]uint64)
		}
		if _, exists := c.recoverySeen[key]; !exists {
			if len(c.recoverySeenOrder) == maxRecoverySessions {
				delete(c.recoverySeen, c.recoverySeenOrder[0])
				c.recoverySeenOrder[0] = ""
				c.recoverySeenOrder = c.recoverySeenOrder[1:]
			}
			c.recoverySeenOrder = append(c.recoverySeenOrder, key)
		}
		c.recoverySeen[key] = mark.overflow
	}
}

func (c *Client) clearRecoveryLocked() {
	c.recovery = nil
	c.recoveryAll = false
	c.recoveryOverflow = 0
	c.recoverySeen = nil
	c.recoverySeenOrder = nil
	// Keep generations monotonic across station changes. No private event data
	// survives; stale acknowledgements are additionally scoped to queueConfig.
}
