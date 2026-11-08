package agents

import (
	"errors"
	"os"
	"path/filepath"
	"time"
)

const maxClaudeOwnedLeases = 4096

type claudeExternalStateUnknownError struct{}

func (*claudeExternalStateUnknownError) Error() string {
	return "external_session_state_unknown: 无法确认外部 Claude 会话是否已停止，请在电脑处理原会话；不会启动另一个进程接管"
}
func (*claudeExternalStateUnknownError) ExecutionErrorCode() string {
	return "external_session_state_unknown"
}

var errClaudeExternalStateUnknown error = &claudeExternalStateUnknownError{}

type claudeOwnedLease struct {
	worker *claudeProc
	// sdkSource is an explicit native fork creation receipt, not an OS-exit
	// receipt. Only a verified SDK-created file can acquire this provenance.
	sdkSource string
	terminal  string
	exited    bool
	path      string
	file      os.FileInfo
}

// These hooks are called outside p.mu. They never acquire a.mu, start/stop a
// process or read conversation content. h.mu guards process-local provenance.
// A new admitted turn invalidates the previous completed snapshot before its
// frame can become visible to the worker.
func (h *claudeHistory) noteOwnedTurn(p *claudeProc) {
	if h == nil || p == nil || !validChildID(p.id) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ownedLeases == nil {
		h.ownedLeases = map[string]*claudeOwnedLease{}
	}
	if _, exists := h.ownedLeases[p.id]; !exists && len(h.ownedLeases) >= maxClaudeOwnedLeases {
		return // Lack of provenance can deny recovery, never grant it.
	}
	h.ownedLeases[p.id] = &claudeOwnedLease{worker: p}
}

func (h *claudeHistory) noteOwnedTerminal(p *claudeProc, status string) {
	if h == nil || p == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	lease := h.ownedLeases[p.id]
	if lease == nil || lease.worker != p || lease.exited {
		return
	}
	lease.path, lease.file = "", nil
	lease.terminal = status
}

// recordOwnedExit must run only after final session cleanup for this worker.
// It independently checks a real Wait-confirmed OS process, not pipe EOF or a
// synthetic/no-process record. A completed turn or a confirmed explicit stop
// can be retained; a crash, failed turn, or uncertain exit cannot.
func (h *claudeHistory) recordOwnedExit(p *claudeProc, normalExit bool) {
	if h == nil || p == nil || !normalExit || p.cmd == nil || p.cmd.Process == nil || !p.processExitConfirmed() {
		return
	}
	h.mu.Lock()
	lease := h.ownedLeases[p.id]
	if lease == nil || lease.worker != p || lease.exited || lease.terminal != "completed" && lease.terminal != "interrupted" {
		h.mu.Unlock()
		return
	}
	terminal := lease.terminal
	h.mu.Unlock()
	p.mu.Lock()
	busy, stopped := p.turns > 0 || p.activeSubtasks.Load(), p.stopped
	p.mu.Unlock()
	if terminal == "completed" && busy || terminal == "interrupted" && !stopped {
		return
	}
	path, err := h.path(p.id)
	if err != nil {
		return
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return
	}
	before, err := os.Stat(path)
	if err != nil || !before.Mode().IsRegular() || !h.safeTranscriptPath(path) {
		return
	}
	// Re-resolve the selected transcript and check its identity twice. A rename,
	// replacement, truncation or final late write while sampling denies recovery.
	selected, err := h.path(p.id)
	if err != nil {
		return
	}
	selected, err = filepath.Abs(selected)
	if err != nil || filepath.Clean(selected) != filepath.Clean(path) {
		return
	}
	final, err := os.Stat(path)
	if err != nil || !sameClaudeTranscript(before, final) || !h.safeTranscriptPath(path) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ownedLeases[p.id] != lease || lease.worker != p || lease.terminal != terminal || lease.exited {
		return
	}
	lease.path, lease.file, lease.exited = filepath.Clean(path), final, true
}

func sameClaudeTranscript(a, b os.FileInfo) bool {
	return a != nil && b != nil && a.Mode().IsRegular() && b.Mode().IsRegular() &&
		os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

func (h *claudeHistory) ownedCompletion(id string) bool {
	if h == nil || !validChildID(id) {
		return false
	}
	h.mu.Lock()
	lease := h.ownedLeases[id]
	if lease == nil || !lease.exited && lease.sdkSource == "" || lease.file == nil || lease.path == "" {
		h.mu.Unlock()
		return false
	}
	path, expected := lease.path, lease.file
	h.mu.Unlock()
	selected, err := h.path(id)
	if err == nil {
		selected, err = filepath.Abs(selected)
	}
	var current os.FileInfo
	if err == nil && filepath.Clean(selected) == path && h.safeTranscriptPath(path) {
		current, err = os.Stat(path)
	} else if err == nil {
		err = errors.New("transcript path changed")
	}
	valid := err == nil && sameClaudeTranscript(expected, current)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ownedLeases[id] != lease {
		return false
	}
	if !valid {
		delete(h.ownedLeases, id) // A later restoration cannot revive old ownership.
	}
	return valid
}

// A recent external write is a useful busy hint. A silent or old file is not
// proof that an external process is idle; absent our own confirmed completion,
// unknown history remains read-only irrespective of its age.
func (h *claudeHistory) remoteHistoryState(id string, updatedAt int64) (string, bool) {
	if h.ownedCompletion(id) {
		return "idle", true
	}
	// A future timestamp can be produced by a clock correction or a copied
	// transcript. It is not evidence that another process is currently busy;
	// fail closed as unknown until the bridge has its own exit receipt.
	age := time.Since(time.UnixMilli(updatedAt))
	if updatedAt > 0 && age >= 0 && age < externalActive {
		return "running", false
	}
	return "unknown", false
}
