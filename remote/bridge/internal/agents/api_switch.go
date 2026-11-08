package agents

import "time"

// pendingRemoteApprovals is independent of process/turn bookkeeping: an
// authenticated MCP hook can still be waiting after its parent reports idle.
// Call only after releasing agent/proc mutexes; the approval registry must not
// be nested with vendor state locks or invoke callbacks during this check.
func pendingRemoteApprovals(registry *approvals) bool {
	if registry == nil {
		return false
	}
	now := time.Now()
	registry.mu.Lock()
	defer registry.mu.Unlock()
	for _, pending := range registry.waiting {
		if now.Before(pending.expiresAt) && approvalSourceOpen(pending.sourceClosed) {
			return true
		}
	}
	return false
}

// ActiveRemoteTurns is a read-only admission check. An API switch must not change
// the loopback gateway's upstream credential while a worker is using it.
func (a *codexAgent) ActiveRemoteTurns() bool {
	if a.sideTools.activeTurns() {
		return true
	}
	if a.sideQuestions.activeTurns() {
		return true
	}
	a.mu.Lock()
	busy := a.hasActiveTurnsLocked() || a.unconfirmedProcess != nil && !a.unconfirmedProcess.processExitConfirmed()
	a.mu.Unlock()
	return busy || pendingRemoteApprovals(a.aps)
}

func (a *claudeAgent) ActiveRemoteTurns() bool {
	if a.sideQuestions.activeTurns() {
		return true
	}
	a.mu.Lock()
	if a.hasUnconfirmedWorkerLocked() {
		a.mu.Unlock()
		return true
	}
	procs := make([]*claudeProc, 0, len(a.procs))
	for _, proc := range a.procs {
		procs = append(procs, proc)
	}
	a.mu.Unlock()
	for _, proc := range procs {
		proc.mu.Lock()
		busy := proc.turns > 0 || proc.activeSubtasks.Load() || proc.contextCompacting
		proc.mu.Unlock()
		if busy {
			return true
		}
	}
	return pendingRemoteApprovals(a.aps)
}
