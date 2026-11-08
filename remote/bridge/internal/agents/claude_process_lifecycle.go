package agents

import (
	"errors"
	"time"
)

var errClaudeProcessExitUnconfirmed = errors.New("Claude Code 连接已关闭，但进程树退出尚未确认")
var errClaudeDeliveryInterrupted = &claudeDeliveryUncertainError{}

type claudeDeliveryUncertainError struct{ cause error }

func (*claudeDeliveryUncertainError) Error() string {
	return "command_delivery_uncertain: Claude Code 连接已关闭，操作可能已执行，请核对原会话；不会自动重发"
}
func (e *claudeDeliveryUncertainError) Unwrap() error { return e.cause }
func (*claudeDeliveryUncertainError) Is(target error) bool {
	return target == errClaudeDeliveryInterrupted
}
func (*claudeDeliveryUncertainError) ExecutionErrorCode() string { return "command_delivery_uncertain" }

func (p *claudeProc) processExitConfirmed() bool {
	return !p.treeTermination.unconfirmed() && p.rootProcessExitConfirmed()
}

func (p *claudeProc) rootProcessExitConfirmed() bool {
	// Legacy/synthetic records with no OS process own nothing to terminate.
	if p.cmd == nil || p.cmd.Process == nil {
		return true
	}
	if p.processWaitDone == nil {
		return false
	}
	select {
	case <-p.processWaitDone:
		// Wait also reaps signal-terminated Unix processes; Exited() is false
		// for those, despite their confirmed death. Do not fence all restarts.
		return p.cmd.ProcessState != nil
	default:
		return false
	}
}

func (p *claudeProc) transportRetired() bool {
	select {
	case <-p.stopCh:
		return true
	case <-p.done:
		return true
	default:
		return false
	}
}

// Called with a.mu held. A stopped or retired worker remains a global startup
// fence across sessions and repeated retries until its OS exit is confirmed.
func (a *claudeAgent) hasUnconfirmedWorkerLocked() bool {
	for p := range a.workers {
		// Pending cleanup is itself a retirement boundary, even before
		// stopCh/done can be closed by the stopping goroutine.
		if p.treeTermination.unconfirmed() {
			return true
		}
		if p.processExitConfirmed() {
			// Keep a confirmed worker's same-session startup gate until its
			// final approval and terminal-event cleanup is completely finished.
			select {
			case <-p.done:
				delete(a.workers, p)
			default:
			}
			continue
		}
		if p.transportRetired() {
			return true
		}
	}
	return false
}

func (p *claudeProc) closeOwnedPipes() {
	if p.stdin != nil {
		_ = p.stdin.Close()
	}
	if p.stdout != nil {
		_ = p.stdout.Close()
	}
	if p.stderr != nil {
		_ = p.stderr.Close()
	}
}

func (a *claudeAgent) waitForClaudeProcessExit(p *claudeProc) error {
	history := a.hist // A late reaper need not retain the whole agent.
	waited := make(chan error, 1)
	go func() {
		err := p.cmd.Wait()
		if p.cmd.ProcessState != nil {
			removeClaudeRuntimeSettings(p.runtimeSettingsFile)
		}
		close(p.processWaitDone)
		waited <- err
		// Cleanup and successful tree termination can both precede this
		// late Wait. Complete their three-way handoff without relaxing the
		// original worker/lease, terminal, busy, tree or transcript checks.
		if p.exitCleanupReady.Load() {
			history.recordOwnedExit(p, err == nil || p.wasStopped())
		}
	}()
	confirmed := func(err error) error {
		if !p.processExitConfirmed() {
			return errClaudeProcessExitUnconfirmed
		}
		return err
	}
	select {
	case err := <-waited:
		return confirmed(err)
	case <-p.stopCh:
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		select {
		case err := <-waited:
			return confirmed(err)
		case <-timer.C:
			return errClaudeProcessExitUnconfirmed
		}
	}
}
