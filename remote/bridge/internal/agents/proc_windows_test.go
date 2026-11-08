//go:build windows

package agents

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// Fault-injection helpers use a short deadline; production allows 10 seconds
// for the system utility to enumerate a real installed Agent's process tree.
func killTreeWithTerminator(cmd *exec.Cmd, newTerminator func(context.Context, int) (*exec.Cmd, error)) error {
	return killTreeWithTerminatorTimeout(cmd, newTerminator, 3*time.Second)
}

func TestWindowsTaskkillUsesSystemDirectory(t *testing.T) {
	// No global command replacement: even a PATH containing only an untrusted
	// directory must not decide which program receives an owned process PID.
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SystemRoot", t.TempDir())
	dir, err := windows.GetSystemDirectory()
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := windowsTaskkillCommand(context.Background(), 123)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(cmd.Path, filepath.Join(dir, "taskkill.exe")) {
		t.Fatal("terminator resolved through PATH or an environment override")
	}
	if strings.Join(cmd.Args[1:], " ") != "/T /F /PID 123" {
		t.Fatal("terminator lost its exact owned process-tree target")
	}
}

func startWindowsOwnedLifecycle(t *testing.T, exe, role, dir string) (*exec.Cmd, windows.Handle) {
	t.Helper()
	cmd := exec.Command(exe)
	cmd.Env = childEnv(exe, map[string]string{
		"SALCARA_CLAUDE_LIFECYCLE_ROLE": role,
		"SALCARA_CLAUDE_LIFECYCLE_DIR":  dir,
	}, nil)
	prepareCmd(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	owned := rpcOwnedHandle(t, cmd.Process.Pid)
	t.Cleanup(func() {
		_ = windows.TerminateProcess(owned, 0)
		_, _ = windows.WaitForSingleObject(owned, 2000)
		_ = stdin.Close()
		_ = cmd.Wait()
	})
	return cmd, owned
}

func TestWindowsKillTreeCancelsAndReapsOwnedTerminatorBeforeReturn(t *testing.T) {
	exe, dir := claudeLifecycleExecutable(t), t.TempDir()
	cmd, target := startWindowsOwnedLifecycle(t, exe, "closed-output-alive", dir)
	finished := make(chan struct{})
	started := time.Now()
	go func() {
		err := killTreeWithTerminator(cmd, func(ctx context.Context, pid int) (*exec.Cmd, error) {
			kill := exec.CommandContext(ctx, exe, "/T", "/F", "/PID", strconv.Itoa(pid))
			kill.Env = childEnv(exe, map[string]string{
				"SALCARA_CLAUDE_LIFECYCLE_SLOW_TASKKILL": "1",
				"SALCARA_CLAUDE_LIFECYCLE_DIR":           dir,
			}, nil)
			return kill, nil
		})
		if !errors.Is(err, errProcessTreeTerminationUnconfirmed) {
			t.Error("terminator timeout falsely confirmed tree exit")
		}
		close(finished)
	}()
	var terminator windows.Handle
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(filepath.Join(dir, "taskkill-started")); err == nil {
			pid, err := strconv.Atoi(string(data))
			if err != nil || pid <= 0 {
				t.Fatal("invalid owned terminator marker")
			}
			terminator = rpcOwnedHandle(t, pid)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if terminator == 0 {
		t.Fatal("synthetic terminator never reached its cancellation barrier")
	}
	t.Cleanup(func() {
		_ = windows.TerminateProcess(terminator, 0)
		_, _ = windows.WaitForSingleObject(terminator, 2000)
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Error("terminator cleanup did not return after its owned process exited")
		}
	})
	select {
	case <-finished:
	case <-time.After(4 * time.Second):
		t.Fatal("owned terminator cancellation failed to bound process cleanup")
	}
	if time.Since(started) < 3*time.Second {
		t.Fatal("synthetic terminator did not exercise the production deadline")
	}
	for _, handle := range []windows.Handle{terminator, target} {
		if status, err := windows.WaitForSingleObject(handle, 1000); err != nil || status != windows.WAIT_OBJECT_0 {
			t.Fatal("cleanup left the terminator or its exact owned target alive")
		}
	}
}

func TestWindowsKillTreeDiscoveryFailureStillKillsOwnedProcess(t *testing.T) {
	exe := claudeLifecycleExecutable(t)
	cmd, target := startWindowsOwnedLifecycle(t, exe, "closed-output-alive", t.TempDir())
	err := killTreeWithTerminator(cmd, func(context.Context, int) (*exec.Cmd, error) {
		return nil, errors.New("synthetic system-directory failure")
	})
	if !errors.Is(err, errProcessTreeTerminationUnconfirmed) {
		t.Fatal("terminator discovery failure falsely confirmed tree exit")
	}
	if status, err := windows.WaitForSingleObject(target, 1000); err != nil || status != windows.WAIT_OBJECT_0 {
		t.Fatal("terminator lookup failure abandoned its owned process")
	}
}

func TestWindowsKillTreeStopsOwnedDescendantsButNotUnrelatedProcess(t *testing.T) {
	exe, dir := claudeLifecycleExecutable(t), t.TempDir()
	cmd, target := startWindowsOwnedLifecycle(t, exe, "eof-parent", dir)
	child := claudeOwnedChild(t, dir)
	_, unrelated := startWindowsOwnedLifecycle(t, exe, "closed-output-alive", t.TempDir())
	if err := killTreeResult(cmd); err != nil {
		t.Fatal("successful system terminator did not confirm its tree operation", err)
	}
	for _, handle := range []windows.Handle{target, child} {
		if status, err := windows.WaitForSingleObject(handle, 1000); err != nil || status != windows.WAIT_OBJECT_0 {
			t.Fatal("system terminator did not stop the owned process tree")
		}
	}
	if status, err := windows.WaitForSingleObject(unrelated, 0); err != nil || status != uint32(windows.WAIT_TIMEOUT) {
		t.Fatal("process tree cleanup touched an unrelated synthetic worker")
	}
}

func TestWindowsKillTreeNeverDispatchesReapedPID(t *testing.T) {
	exe := claudeLifecycleExecutable(t)
	cmd, _ := startWindowsOwnedLifecycle(t, exe, "closed-output-alive", t.TempDir())
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	called := false
	err := killTreeWithTerminator(cmd, func(context.Context, int) (*exec.Cmd, error) {
		called = true
		return nil, errors.New("must never dispatch a reaped numeric PID")
	})
	if called {
		t.Fatal("cleanup attempted numeric PID dispatch after its handle was released")
	}
	if !errors.Is(err, errProcessTreeTerminationUnconfirmed) {
		t.Fatal("released root handle falsely proved descendant exit")
	}
}

func TestWindowsKillTreePinsOriginalHandleAcrossConcurrentWait(t *testing.T) {
	exe := claudeLifecycleExecutable(t)
	cmd := exec.Command(exe)
	cmd.Env = childEnv(exe, map[string]string{
		"SALCARA_CLAUDE_LIFECYCLE_ROLE": "closed-output-alive",
		"SALCARA_CLAUDE_LIFECYCLE_DIR":  t.TempDir(),
	}, nil)
	prepareCmd(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Do not open a second test-owned handle: that would itself pin the PID
	// and hide a missing production pin. The raw value is inspected only
	// inside the established production lifetime below, never used to kill.
	var raw windows.Handle
	if err := cmd.Process.WithHandle(func(handle uintptr) { raw = windows.Handle(handle) }); err != nil {
		t.Fatal(err)
	}
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce, waitOnce sync.Once
	releaseFactory := func() { releaseOnce.Do(func() { close(release) }) }
	waitOwned := func() { waitOnce.Do(func() { _ = cmd.Wait() }) }
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		releaseFactory()
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			t.Error("owned handle pin did not retire after the command factory returned")
		}
		waitOwned()
	})
	go func() {
		killTreeWithTerminator(cmd, func(context.Context, int) (*exec.Cmd, error) {
			close(entered)
			<-release
			return nil, errors.New("synthetic terminator not launched")
		})
		close(finished)
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("terminator did not reach its pinned command-factory barrier")
	}
	_ = cmd.Process.Kill()
	waited := make(chan struct{})
	go func() { waitOwned(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("holding the original process handle deadlocked its concurrent Wait")
	}
	pid, err := windows.GetProcessId(raw)
	if err != nil || pid != uint32(cmd.Process.Pid) {
		t.Fatal("concurrent Wait released the original process object before terminator retirement")
	}
	releaseFactory()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("retired terminator did not release its original handle pin")
	}
}
