package agents

// ActiveRemoteTurns is a read-only admission check. An API switch must not change
// the loopback gateway's upstream credential while a worker is using it.
func (a *codexAgent) ActiveRemoteTurns() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.hasActiveTurnsLocked()
}

func (a *claudeAgent) ActiveRemoteTurns() bool {
	a.mu.Lock()
	procs := make([]*claudeProc, 0, len(a.procs))
	for _, proc := range a.procs {
		procs = append(procs, proc)
	}
	a.mu.Unlock()
	for _, proc := range procs {
		proc.mu.Lock()
		busy := proc.turns > 0 || proc.activeSubtasks.Load()
		proc.mu.Unlock()
		if busy {
			return true
		}
	}
	return false
}
