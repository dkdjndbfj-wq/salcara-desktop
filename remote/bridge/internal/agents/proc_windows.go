//go:build windows

package agents

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

const createNoWindow = 0x08000000

// prepareCmd hides the console window of every child (the bridge runs without a console).
func prepareCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

// killTree stops the process and all of its children (claude.cmd → node → shells).
func killTree(cmd *exec.Cmd) {
	_ = killTreeResult(cmd)
}

func killTreeResult(cmd *exec.Cmd) error {
	// The system taskkill utility can take several seconds to enumerate an
	// installed Codex process tree, even when it exits successfully. Keep a
	// bounded allowance for that operation instead of cancelling it at 3s and
	// leaving a permanent unconfirmed-tree fence after an ordinary API switch.
	return killTreeWithTerminatorTimeout(cmd, windowsTaskkillCommand, 10*time.Second)
}

// Resolve the system executable independently of the user's PATH, working
// directory, or an Agent's tool shims. A shadowed taskkill must never receive
// an owned PID or delay a configuration change until its child finally exits.
func windowsTaskkillCommand(ctx context.Context, pid int) (*exec.Cmd, error) {
	dir, err := windows.GetSystemDirectory()
	if err != nil {
		return nil, err
	}
	return exec.CommandContext(ctx, filepath.Join(dir, "taskkill.exe"), "/T", "/F", "/PID", strconv.Itoa(pid)), nil
}

// A command factory is passed explicitly so cancellation can be exercised with
// synthetic owned helpers without replacing PATH or any shared package state.
func killTreeWithTerminatorTimeout(cmd *exec.Cmd, newTerminator func(context.Context, int) (*exec.Cmd, error), timeout time.Duration) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	// Keep the original kernel process object alive until taskkill is reaped.
	// Another goroutine may Wait while the helper starts; without this pin its
	// numeric PID could be recycled before /PID is acted upon. WithHandle also
	// rejects cleanup after Wait/Release, without reopening an untrusted PID.
	var treeErr error
	handleErr := cmd.Process.WithHandle(func(handle uintptr) {
		owned := windows.Handle(handle)
		status, err := windows.WaitForSingleObject(owned, 0)
		if err != nil {
			treeErr = errors.Join(errProcessTreeTerminationUnconfirmed, err)
			_ = windows.TerminateProcess(owned, 1)
			return
		}
		if status == windows.WAIT_OBJECT_0 {
			// A pending forced cleanup cannot infer descendant exit from a
			// dead root, and must not dispatch by its numeric PID.
			treeErr = errProcessTreeTerminationUnconfirmed
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		kill, err := newTerminator(ctx, cmd.Process.Pid)
		if err != nil {
			treeErr = errors.Join(errProcessTreeTerminationUnconfirmed, err)
			_ = windows.TerminateProcess(owned, 1)
			return
		}
		kill.WaitDelay = 250 * time.Millisecond
		prepareCmd(kill)
		if err := kill.Run(); err != nil {
			treeErr = errors.Join(errProcessTreeTerminationUnconfirmed, err)
			_ = windows.TerminateProcess(owned, 1)
		}
	})
	if handleErr != nil {
		// Including ErrProcessDone: no safe numeric-PID retry is possible,
		// and a released root handle provides no descendant-exit evidence.
		return errors.Join(errProcessTreeTerminationUnconfirmed, handleErr)
	}
	return treeErr
}
