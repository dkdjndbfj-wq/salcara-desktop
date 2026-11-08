package agents

import "os"

// A file's recent modification is normally a conservative signal of another
// writer. During an API switch we know which idle threads belonged to the
// exact process we just stopped. Retain that exception only when the file is
// unchanged both across confirmed tree shutdown and immediately before resume.
// This proof is never persisted, inferred from the owned-thread ID set, or
// used to override active/unknown native thread status.
type codexIdleRollout struct {
	path string
	file os.FileInfo
}

func (r *codexIdleRollout) matches(path string) bool {
	if r == nil || r.file == nil || path != r.path {
		return false
	}
	current, err := os.Stat(path)
	return err == nil && os.SameFile(r.file, current) && current.Size() == r.file.Size() && current.ModTime().Equal(r.file.ModTime())
}

func (a *codexAgent) captureIdleCodexRolloutsLocked() map[*codexThread]*codexIdleRollout {
	result := map[*codexThread]*codexIdleRollout{}
	for _, t := range a.threads {
		if !t.loaded || t.statusUnconfirmed || t.turnID != "" || t.info.ActiveTurnID != "" {
			continue
		}
		switch t.info.Status {
		case "idle", "completed", "failed", "interrupted":
		default:
			continue
		}
		if file, err := os.Stat(t.path); err == nil && file.Mode().IsRegular() {
			result[t] = &codexIdleRollout{path: t.path, file: file}
		}
	}
	return result
}

func (a *codexAgent) rememberIdleCodexRolloutsLocked(c *rpcConn, captured map[*codexThread]*codexIdleRollout) {
	if !c.processExitConfirmed() {
		return
	}
	for t, rollout := range captured {
		if a.threads[t.id] == t && rollout.matches(t.path) {
			t.idleRestartRollout = rollout
		}
	}
}
