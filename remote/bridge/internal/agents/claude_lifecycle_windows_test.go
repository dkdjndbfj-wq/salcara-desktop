//go:build windows

package agents

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"salcara/bridge/internal/protocol"
)

func claudeLifecycleExecutable(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "owned-claude-lifecycle.exe")
	// The helper recursively starts its child; the GUI subsystem prevents
	// that child from allocating a console without changing fixture source.
	cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go.exe"), "build", "-ldflags=-H=windowsgui", "-o", exe, "./testdata/claude_lifecycle_helper")
	prepareCmd(cmd)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatal("cannot build synthetic lifecycle helper", err, string(output))
	}
	return exe
}

func newClaudeLifecycleAgent(t *testing.T, role string) (*claudeAgent, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SALCARA_CLAUDE_LIFECYCLE_ROLE", role)
	t.Setenv("SALCARA_CLAUDE_LIFECYCLE_DIR", dir)
	exe := claudeLifecycleExecutable(t)
	s := Settings{BridgeExe: exe, LocalToken: "synthetic-no-network-capability", Approval: "auto_all", ClaudeRoot: "http://127.0.0.1:1", ClaudeKey: "synthetic-api-key"}
	a := newClaudeAgent(func(protocol.Event) {}, func() Settings { return s }, exe, t.TempDir())
	return a, dir
}

func claudeOwnedChild(t *testing.T, dir string) windows.Handle {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(filepath.Join(dir, "child-pid")); err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil || pid <= 0 {
				t.Fatal("invalid synthetic child identifier")
			}
			handle := rpcOwnedHandle(t, pid)
			t.Cleanup(func() {
				_ = windows.TerminateProcess(handle, 0)
				_, _ = windows.WaitForSingleObject(handle, 2000)
			})
			return handle
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("owned synthetic child did not publish its identifier")
	return 0
}

func TestClaudeCloseRetiresExitedParentWithInheritedPipes(t *testing.T) {
	a, dir := newClaudeLifecycleAgent(t, "eof-parent")
	p, err := a.spawn(context.Background(), newUUID(), dir, "", "auto_all", false)
	if err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.procs[p.id] = p
	a.mu.Unlock()
	parent := rpcOwnedHandle(t, p.cmd.Process.Pid)
	closed := make(chan struct{})
	t.Cleanup(func() {
		a.Close()
		select {
		case <-closed:
		case <-time.After(3 * time.Second):
			t.Error("synthetic agent cleanup did not settle")
		}
	})
	child := claudeOwnedChild(t, dir)
	_ = p.stdin.Close()
	if status, err := windows.WaitForSingleObject(parent, 2000); err != nil || status != windows.WAIT_OBJECT_0 {
		t.Fatal("synthetic parent did not exit on EOF", status, err)
	}
	if status, err := windows.WaitForSingleObject(child, 0); err != nil || status != uint32(windows.WAIT_TIMEOUT) {
		t.Fatal("synthetic child did not retain inherited stdout", status, err)
	}
	go func() { a.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(6 * time.Second):
		// killTree has a 3s deadline plus a 250ms WaitDelay; process reaping
		// after transport retirement is bounded separately at 2s. A 3s total
		// assertion raced the terminator's legitimate deadline under load.
		t.Fatal("Claude Close remained blocked on inherited owned pipes")
	}
	select {
	case <-p.done:
	default:
		t.Fatal("owned Claude transport was not retired")
	}
	if !p.rootProcessExitConfirmed() {
		t.Fatal("exited parent was not reaped after inherited pipe retirement")
	}
	if p.processExitConfirmed() {
		t.Fatal("exited root falsely confirmed its live inherited-pipe descendant")
	}
	a.mu.Lock()
	fenced := a.hasUnconfirmedWorkerLocked()
	a.mu.Unlock()
	if !fenced {
		t.Fatal("inherited-pipe retirement discarded the unresolved process tree")
	}
}

func TestClaudeUnconfirmedExitRejectsEverySubsequentSpawn(t *testing.T) {
	a, dir := newClaudeLifecycleAgent(t, "closed-output-alive")
	s := a.settings()
	s.ClaudeRoot, s.ClaudeKey = "http://127.0.0.1:9/synthetic-no-network", "synthetic-provider-pin"
	a.settings = func() Settings { return s }
	p, err := a.spawn(context.Background(), newUUID(), dir, "", "auto_all", false)
	if err != nil {
		t.Fatal(err)
	}
	owned := rpcOwnedHandle(t, p.cmd.Process.Pid)
	a.mu.Lock()
	a.procs[p.id] = p
	a.mu.Unlock()
	t.Cleanup(func() {
		_ = windows.TerminateProcess(owned, 0)
		_, _ = windows.WaitForSingleObject(owned, 2000)
		a.Close()
	})
	// Model an OS kill failure on this owned fixture. The ordinary reader and
	// bounded wait perform retirement; no synthetic onExit call is needed.
	p.stopOnce.Do(func() {
		p.mu.Lock()
		p.stopped = true
		p.mu.Unlock()
		close(p.stopCh)
		p.closeOwnedPipes()
	})
	select {
	case <-p.done:
	case <-time.After(4 * time.Second):
		t.Fatal("forced Claude transport retirement remained unbounded")
	}
	if p.processExitConfirmed() {
		t.Fatal("fixture did not retain an unconfirmed OS process")
	}
	if p.runtimeSettingsFile == "" {
		t.Fatal("fixture missing runtime pin")
	}
	if _, err := os.Stat(p.runtimeSettingsFile); err != nil {
		t.Fatal("unconfirmed live process lost provider pin")
	}
	for range 3 {
		next, err := a.spawn(context.Background(), newUUID(), dir, "", "auto_all", false)
		if !errors.Is(err, errClaudeProcessExitUnconfirmed) || next != nil {
			if next != nil {
				a.stop(next)
			}
			t.Fatal("retired but unconfirmed owned process allowed another spawn")
		}
	}
	_ = windows.TerminateProcess(owned, 0)
	select {
	case <-p.processWaitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("eventual owned process exit was not reaped")
	}
	if !p.processExitConfirmed() {
		t.Fatal("eventual real exit did not release the process fence")
	}
	if _, err := os.Stat(filepath.Dir(p.runtimeSettingsFile)); !os.IsNotExist(err) {
		t.Fatal("confirmed process exit retained runtime pin")
	}
	t.Setenv("SALCARA_CLAUDE_LIFECYCLE_ROLE", "normal")
	next, err := a.spawn(context.Background(), newUUID(), dir, "", "auto_all", false)
	if err != nil || next == nil {
		t.Fatal("confirmed real exit did not permit a later safe startup", err)
	}
	a.stop(next)
}

func TestClaudeNormalLifecyclePreservesOutputOrderAndConcurrentStop(t *testing.T) {
	a, dir := newClaudeLifecycleAgent(t, "normal")
	rec := &recorder{}
	a.sink = rec.sink
	a.thr = newThrottle(rec.sink)
	t.Cleanup(a.Close)
	p, err := a.spawn(context.Background(), newUUID(), dir, "", "auto_all", false)
	if err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.procs[p.id] = p
	a.mu.Unlock()
	if err := a.writeTurn(p, "synthetic input", nil, nil); err != nil {
		t.Fatal(err)
	}
	rec.wait(t, "synthetic completed result", func(e protocol.Event) bool { return e.Type == "turn" && e.Status == "completed" })
	message, result := -1, -1
	for i, e := range rec.all() {
		if e.Type == "message" && e.Role == "assistant" && e.Text == "synthetic final answer" {
			message = i
		}
		if e.Type == "turn" && e.Status == "completed" {
			result = i
		}
	}
	if message < 0 || result <= message {
		t.Fatal("owned lifecycle reordered the final message after its result")
	}
	stopped := make(chan struct{}, 4)
	for range 4 {
		go func() { a.stop(p); stopped <- struct{}{} }()
	}
	for range 4 {
		select {
		case <-stopped:
		case <-time.After(4 * time.Second):
			t.Fatal("concurrent own-worker stop did not settle")
		}
	}
	select {
	case <-p.done:
	case <-time.After(4 * time.Second):
		t.Fatal("normal owned lifecycle failed to retire")
	}
	if !p.processExitConfirmed() || approvalSourceOpen(p.permissionLease.done) {
		t.Fatal("normal lifecycle did not confirm real exit or retire helper")
	}
}

func TestClaudePartialStartupDeliveryRetainsSessionHandle(t *testing.T) {
	a, dir := newClaudeLifecycleAgent(t, "exit-after-byte")
	t.Cleanup(a.Close)
	id, err := a.Start(context.Background(), dir, strings.Repeat("synthetic ", 512<<10), "", "auto_all")
	if id == "" || protocol.ExecutionErrorCode(err) != "command_delivery_uncertain" || !errors.Is(err, errClaudeDeliveryInterrupted) {
		t.Fatal("partial initial owned pipe write lost session handle or uncertainty")
	}
}
