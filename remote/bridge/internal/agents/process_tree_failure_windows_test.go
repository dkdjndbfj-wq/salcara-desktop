//go:build windows

package agents

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"salcara/bridge/internal/protocol"
)

func waitOwnedTreeTest(t *testing.T, done <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal(label + " did not settle")
	}
}

func assertOwnedTreeTestAlive(t *testing.T, child windows.Handle) {
	t.Helper()
	if status, err := windows.WaitForSingleObject(child, 0); err != nil || status != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("owned child must remain alive at failure barrier: %d %v", status, err)
	}
}

func cleanupOwnedTreeTest(t *testing.T, handle windows.Handle) {
	t.Helper()
	t.Cleanup(func() {
		if status, _ := windows.WaitForSingleObject(handle, 0); status != windows.WAIT_OBJECT_0 {
			_ = windows.TerminateProcess(handle, 0)
		}
		if status, err := windows.WaitForSingleObject(handle, 2000); err != nil || status != windows.WAIT_OBJECT_0 {
			t.Errorf("original owned process handle did not confirm cleanup: %d %v", status, err)
		}
	})
}

// Return a failure only after the root fallback has run, with a barrier that
// allows its concurrent Wait and transport retirement to finish before that
// failure reaches the stop caller. No global command hook is installed.
func ownedTreeFailureBarrier() (func(*exec.Cmd) error, <-chan struct{}, func()) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	terminate := func(cmd *exec.Cmd) error {
		err := killTreeWithTerminator(cmd, func(context.Context, int) (*exec.Cmd, error) {
			return nil, errors.New("synthetic owned tree terminator unavailable")
		})
		close(entered)
		<-release
		return err
	}
	return terminate, entered, unblock
}

func TestWindowsFailedTreeCleanupFencesClaude(t *testing.T) {
	exe, dir := claudeLifecycleExecutable(t), t.TempDir()
	t.Setenv("SALCARA_CLAUDE_LIFECYCLE_ROLE", "eof-parent")
	t.Setenv("SALCARA_CLAUDE_LIFECYCLE_DIR", dir)
	settings := Settings{BridgeExe: exe, ClaudeRoot: "http://127.0.0.1:1", ClaudeKey: "synthetic-only"}
	a := newClaudeAgent(func(protocol.Event) {}, func() Settings { return settings }, exe, t.TempDir())
	t.Cleanup(a.Close)
	p, err := a.spawn(context.Background(), newUUID(), dir, "", "ask", false)
	if err != nil {
		t.Fatal(err)
	}
	parent := rpcOwnedHandle(t, p.cmd.Process.Pid)
	cleanupOwnedTreeTest(t, parent)
	child := claudeOwnedChild(t, dir)
	cleanupOwnedTreeTest(t, child)
	a.mu.Lock()
	a.procs[p.id] = p
	a.mu.Unlock()
	terminate, entered, release := ownedTreeFailureBarrier()
	defer release()
	stopped := make(chan struct{})
	go func() { a.stopWithTreeKiller(p, terminate); close(stopped) }()
	waitOwnedTreeTest(t, entered, "root fallback")
	// Model concurrent EOF/transport retirement while termination is still in
	// flight. Only the actual reader/reaper calls finishExit.
	p.closeOwnedPipes()
	waitOwnedTreeTest(t, p.processWaitDone, "root Wait")
	waitOwnedTreeTest(t, p.done, "Claude transport retirement")
	assertOwnedTreeTestAlive(t, child)
	if p.processExitConfirmed() {
		t.Fatal("root Wait falsely confirmed its still-pending tree termination")
	}
	a.mu.Lock()
	fenced := a.hasUnconfirmedWorkerLocked()
	a.mu.Unlock()
	if !fenced {
		t.Fatal("pending termination lost the Claude startup fence")
	}
	release()
	waitOwnedTreeTest(t, stopped, "failed tree stop")
	if p.processExitConfirmed() {
		t.Fatal("root Wait cleared a failed Claude tree termination")
	}
	for range 3 {
		next, err := a.spawn(context.Background(), newUUID(), dir, "", "ask", false)
		if next != nil || !errors.Is(err, errClaudeProcessExitUnconfirmed) {
			t.Fatal("failed tree termination admitted a later Claude startup")
		}
	}
	a.Close()
	assertOwnedTreeTestAlive(t, child)
	if p.processExitConfirmed() {
		t.Fatal("Close cleared the failed Claude tree termination")
	}
}

func TestWindowsFailedTreeCleanupFencesRPC(t *testing.T) {
	exe, dir := claudeLifecycleExecutable(t), t.TempDir()
	cmd := exec.Command(exe)
	cmd.Env = childEnv(exe, map[string]string{
		"SALCARA_CLAUDE_LIFECYCLE_ROLE": "eof-parent",
		"SALCARA_CLAUDE_LIFECYCLE_DIR":  dir,
	}, nil)
	prepareCmd(cmd)
	c, err := startRPC(cmd, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Kill)
	parent := rpcOwnedHandle(t, cmd.Process.Pid)
	cleanupOwnedTreeTest(t, parent)
	child := claudeOwnedChild(t, dir)
	cleanupOwnedTreeTest(t, child)
	terminate, entered, release := ownedTreeFailureBarrier()
	defer release()
	stopped := make(chan struct{})
	go func() { c.killWithTreeKiller(terminate); close(stopped) }()
	waitOwnedTreeTest(t, entered, "root fallback")
	c.closeOwnedPipes()
	waitOwnedTreeTest(t, c.processWaitDone, "root Wait")
	waitOwnedTreeTest(t, c.done, "RPC transport retirement")
	assertOwnedTreeTestAlive(t, child)
	if c.processExitConfirmed() {
		t.Fatal("root Wait falsely confirmed its still-pending RPC tree termination")
	}
	if !errors.Is(c.err, errRPCProcessExitUnconfirmed) {
		t.Fatalf("RPC published a root-only error instead of the tree fence sentinel: %v", c.err)
	}
	settings := Settings{}
	a := newCodexAgent(func(protocol.Event) {}, func() Settings { return settings }, exe, filepath.Join(t.TempDir(), "synthetic-state.json"))
	t.Cleanup(a.Close)
	a.mu.Lock()
	a.conn = c
	a.mu.Unlock()
	a.onExit(c)
	for range 3 {
		next, err := a.ensure(context.Background())
		if next != nil || !errors.Is(err, errRPCProcessExitUnconfirmed) {
			t.Fatal("pending tree termination admitted a later Codex startup")
		}
	}
	release()
	waitOwnedTreeTest(t, stopped, "failed RPC tree stop")
	if c.processExitConfirmed() || !errors.Is(c.err, errRPCProcessExitUnconfirmed) {
		t.Fatal("root Wait cleared a failed RPC tree termination or its published sentinel")
	}
	if !errors.Is(a.fenceUnconfirmedProcess(c), errRPCProcessExitUnconfirmed) {
		t.Fatal("failed-start RPC path discarded the tree fence")
	}
	for range 3 {
		next, err := a.ensure(context.Background())
		if next != nil || !errors.Is(err, errRPCProcessExitUnconfirmed) {
			t.Fatal("failed tree termination admitted a later Codex startup")
		}
	}
	a.Close()
	assertOwnedTreeTestAlive(t, child)
	if c.processExitConfirmed() {
		t.Fatal("Close cleared the failed RPC tree termination")
	}
}

func TestWindowsTreeTerminatorRunErrorsStayUnconfirmed(t *testing.T) {
	exe := claudeLifecycleExecutable(t)
	for _, mode := range []string{"start-failure", "nonzero-exit"} {
		t.Run(mode, func(t *testing.T) {
			cmd, root := startWindowsOwnedLifecycle(t, exe, "closed-output-alive", t.TempDir())
			cleanupOwnedTreeTest(t, root)
			err := killTreeWithTerminator(cmd, func(ctx context.Context, _ int) (*exec.Cmd, error) {
				if mode == "start-failure" {
					return exec.CommandContext(ctx, filepath.Join(t.TempDir(), "does-not-exist.exe")), nil
				}
				kill := exec.CommandContext(ctx, exe)
				kill.Env = childEnv(exe, map[string]string{"SALCARA_CLAUDE_LIFECYCLE_ROLE": "intentional-nonzero"}, nil)
				return kill, nil
			})
			if !errors.Is(err, errProcessTreeTerminationUnconfirmed) {
				t.Fatal("terminator Run failure was lost after root fallback", err)
			}
			if status, err := windows.WaitForSingleObject(root, 2000); err != nil || status != windows.WAIT_OBJECT_0 {
				t.Fatal("Run failure abandoned its exact owned root", status, err)
			}
		})
	}
}

func TestWindowsNormallyReapedWorkersSkipLateTreeCleanup(t *testing.T) {
	exe := claudeLifecycleExecutable(t)
	cmd, root := startWindowsOwnedLifecycle(t, exe, "closed-output-alive", t.TempDir())
	cleanupOwnedTreeTest(t, root)
	if err := windows.TerminateProcess(root, 0); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	waitDone := make(chan struct{})
	close(waitDone)
	called := false
	terminate := func(*exec.Cmd) error {
		called = true
		return errors.New("must not reopen or dispatch a reaped root")
	}
	p := &claudeProc{cmd: cmd, processWaitDone: waitDone, stopCh: make(chan struct{})}
	a := &claudeAgent{}
	a.stopWithTreeKiller(p, terminate)
	c := &rpcConn{cmd: cmd, processWaitDone: waitDone, killCh: make(chan struct{})}
	c.killWithTreeKiller(terminate)
	if called || !p.processExitConfirmed() || !c.processExitConfirmed() {
		t.Fatal("late cleanup retried or permanently fenced a normally reaped worker")
	}
	if err := killTreeWithTerminator(nil, func(context.Context, int) (*exec.Cmd, error) {
		t.Fatal("nil process invoked terminator")
		return nil, nil
	}); err != nil {
		t.Fatal("nil process acquired an unresolved tree", err)
	}
}

func TestWindowsSuccessfulTreeCleanupReleasesRPCFence(t *testing.T) {
	exe, dir := claudeLifecycleExecutable(t), t.TempDir()
	cmd := exec.Command(exe)
	cmd.Env = childEnv(exe, map[string]string{
		"SALCARA_CLAUDE_LIFECYCLE_ROLE": "eof-parent",
		"SALCARA_CLAUDE_LIFECYCLE_DIR":  dir,
	}, nil)
	prepareCmd(cmd)
	c, err := startRPC(cmd, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Kill)
	root := rpcOwnedHandle(t, cmd.Process.Pid)
	cleanupOwnedTreeTest(t, root)
	child := claudeOwnedChild(t, dir)
	cleanupOwnedTreeTest(t, child)
	c.Kill()
	waitOwnedTreeTest(t, c.done, "successful RPC tree cleanup")
	if !c.processExitConfirmed() {
		t.Fatal("successful tree cleanup permanently fenced its reaped RPC worker")
	}
	for _, handle := range []windows.Handle{root, child} {
		if status, err := windows.WaitForSingleObject(handle, 1000); err != nil || status != windows.WAIT_OBJECT_0 {
			t.Fatal("successful system termination left an owned process alive", status, err)
		}
	}
	a := &codexAgent{}
	if err := a.fenceUnconfirmedProcess(c); err != nil || a.unconfirmedProcess != nil {
		t.Fatal("successful tree cleanup blocked a later RPC replacement", err)
	}
}

func TestWindowsSuccessfulTreeCleanupRestoresClaudeCompletion(t *testing.T) {
	a, dir := newClaudeLifecycleAgent(t, "normal")
	t.Cleanup(a.Close)
	p, err := a.spawn(context.Background(), newUUID(), dir, "", "ask", false)
	if err != nil {
		t.Fatal(err)
	}
	root := rpcOwnedHandle(t, p.cmd.Process.Pid)
	cleanupOwnedTreeTest(t, root)
	a.mu.Lock()
	a.procs[p.id] = p
	a.mu.Unlock()
	transcriptDir := filepath.Join(a.hist.dir, "-work")
	if err := os.MkdirAll(transcriptDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(transcriptDir, p.id+".jsonl"), []byte("{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":\"synthetic\"},\"cwd\":\"/work\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a.hist.noteOwnedTurn(p)
	a.hist.noteOwnedTerminal(p, "completed")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	stopped := make(chan struct{})
	go func() {
		a.stopWithTreeKiller(p, func(cmd *exec.Cmd) error {
			err := killTreeResult(cmd)
			close(entered)
			<-release
			return err
		})
		close(stopped)
	}()
	waitOwnedTreeTest(t, entered, "successful system tree termination")
	waitOwnedTreeTest(t, p.done, "Claude final session cleanup")
	if p.processExitConfirmed() || a.hist.ownedCompletion(p.id) {
		t.Fatal("unpublished tree result granted recovery before cleanup confirmation")
	}
	if next, err := a.spawn(context.Background(), p.id, dir, "", "ask", true); next != nil || !errors.Is(err, errClaudeProcessExitUnconfirmed) {
		t.Fatal("same-session startup bypassed pending final cleanup", err)
	}
	unblock()
	waitOwnedTreeTest(t, stopped, "successful tree result publication")
	if !p.processExitConfirmed() || !a.hist.ownedCompletion(p.id) {
		t.Fatal("successful late tree result lost the original session completion receipt")
	}
}
