//go:build !windows

package agents

import (
	"errors"
	"os/exec"
	"syscall"
)

// prepareCmd puts the child in its own process group for group cleanup.
func prepareCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killTree requests cleanup of the prepared group and ignores the result.
func killTree(cmd *exec.Cmd) {
	_ = killTreeResult(cmd)
}

// A failed group signal is not a termination receipt, even if the best-effort
// root cleanup succeeds. Callers retain their fence until both this operation
// and their separate root Wait succeed. This covers the prepared process group,
// not descendants which deliberately detach into another group/session.
func killTreeResult(cmd *exec.Cmd) error {
	return killTreeWithGroupSignal(cmd, syscall.Kill)
}

// The explicit signal function lets tests reject only the group operation
// without replacing shared state or preventing cleanup of their owned root.
func killTreeWithGroupSignal(cmd *exec.Cmd, signalGroup func(int, syscall.Signal) error) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if cmd.Process.Pid <= 0 {
		return errProcessTreeTerminationUnconfirmed
	}
	if err := signalGroup(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		// ESRCH also remains unconfirmed: an absent group does not establish
		// that an old or detached descendant exited. Never turn a successful
		// root-only fallback into proof that the group operation succeeded.
		return errors.Join(errProcessTreeTerminationUnconfirmed, err, cmd.Process.Kill())
	}
	return nil
}
