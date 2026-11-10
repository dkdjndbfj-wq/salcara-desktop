//go:build !windows

package agents

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestPOSIXProcessGroupHelper(t *testing.T) {
	if os.Getenv("SALCARA_POSIX_STOP_HELPER") != "1" {
		return
	}
	fmt.Println("ready")
	for {
		time.Sleep(time.Hour)
	}
}

// Each group member is an immediate test child so cleanup can reap it without
// depending on container PID 1's orphan-reaping behavior. They are real kernel
// process-group members; no user process or installed Agent is involved.
func startPOSIXStopHelper(t *testing.T, group int) (*exec.Cmd, <-chan error) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestPOSIXProcessGroupHelper$")
	cmd.Env = append(os.Environ(), "SALCARA_POSIX_STOP_HELPER=1")
	if group >= 0 {
		prepareCmd(cmd)
		cmd.SysProcAttr.Pgid = group
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("owned helper failed to exit during cleanup")
		}
	})
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		ready <- scanner.Scan() && scanner.Text() == "ready"
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("owned helper exited before readiness")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("owned helper never became ready")
	}
	return cmd, done
}

func waitPOSIXStopHelper(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("owned process did not exit after group cleanup")
	}
}

func TestPOSIXKillTreeStopsOnlyPreparedGroup(t *testing.T) {
	root, rootDone := startPOSIXStopHelper(t, 0)
	member, memberDone := startPOSIXStopHelper(t, root.Process.Pid)
	unrelated, _ := startPOSIXStopHelper(t, 0)
	if pgid, err := syscall.Getpgid(member.Process.Pid); err != nil || pgid != root.Process.Pid {
		t.Fatal("fixture member did not join the owned group", pgid, err)
	}
	if err := killTreeResult(root); err != nil {
		t.Fatal("prepared group cleanup failed", err)
	}
	waitPOSIXStopHelper(t, rootDone)
	waitPOSIXStopHelper(t, memberDone)
	if err := unrelated.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("cleanup touched an unrelated owned fixture", err)
	}
}

func TestPOSIXKillTreeGroupFailureRetainsFenceAfterRootFallback(t *testing.T) {
	for _, signalErr := range []syscall.Errno{syscall.EPERM, syscall.ESRCH, syscall.EINVAL} {
		t.Run(signalErr.Error(), func(t *testing.T) {
			root, rootDone := startPOSIXStopHelper(t, 0)
			member, _ := startPOSIXStopHelper(t, root.Process.Pid)
			called := false
			err := killTreeWithGroupSignal(root, func(pid int, signal syscall.Signal) error {
				called = true
				if pid != -root.Process.Pid || signal != syscall.SIGKILL {
					t.Fatal("cleanup lost its exact group target", pid, signal)
				}
				return signalErr
			})
			if !called || !errors.Is(err, signalErr) || !errors.Is(err, errProcessTreeTerminationUnconfirmed) {
				t.Fatal("failed group signal was published as a successful tree receipt", err)
			}
			waitPOSIXStopHelper(t, rootDone)
			if err := member.Process.Signal(syscall.Signal(0)); err != nil {
				t.Fatal("fixture did not retain a live member after root-only cleanup", err)
			}
			var receipt processTreeTermination
			receipt.begin()
			receipt.finish(err)
			if receipt.confirmed() || !receipt.unconfirmed() {
				t.Fatal("root exit erased the failed group termination fence")
			}
		})
	}
}

func TestPOSIXKillTreeMissingGroupIsNotTreeProof(t *testing.T) {
	root, done := startPOSIXStopHelper(t, -1)
	if pgid, err := syscall.Getpgid(root.Process.Pid); err != nil || pgid == root.Process.Pid {
		t.Fatal("fixture unexpectedly owns a process group", pgid, err)
	}
	err := killTreeResult(root)
	if !errors.Is(err, syscall.ESRCH) || !errors.Is(err, errProcessTreeTerminationUnconfirmed) {
		t.Fatal("missing group was treated as proof of descendant exit", err)
	}
	waitPOSIXStopHelper(t, done)
}
