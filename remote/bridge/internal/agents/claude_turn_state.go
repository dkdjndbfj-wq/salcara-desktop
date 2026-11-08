package agents

import "salcara/bridge/internal/protocol"

// activeTurnID is the exact stream admission ID at the head of this worker's
// queue. It is never reconstructed from transcript text or timestamps.
func (p *claudeProc) activeTurnID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.turnIDs) == 0 {
		return ""
	}
	return p.turnIDs[0]
}

func (p *claudeProc) wasStopped() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stopped
}

// updateClaudeTerminal publishes the exact turn that just reached a terminal
// record and the next admitted turn (if any). A queue remains running even
// while an earlier result is being emitted, so the next ID is authoritative.
func (a *claudeAgent) updateClaudeTerminal(p *claudeProc, completed, next string, idle bool) {
	a.setLive(p.id, func(info *protocol.SessionInfo) {
		info.ActiveTurnID = ""
		if ValidTurnID(next) {
			info.ActiveTurnID = next
		}
		if idle {
			info.Status = "idle"
			if ValidTurnID(completed) {
				info.LastTerminalTurnID = completed
			}
		} else {
			info.Status = "running"
		}
	})
	a.mu.Lock()
	if a.continuedTerminal != nil && a.continuedTerminal[p.id] == completed {
		delete(a.continuedTerminal, p.id)
	}
	a.mu.Unlock()
}
