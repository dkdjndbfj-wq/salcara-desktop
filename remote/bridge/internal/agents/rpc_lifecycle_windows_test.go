//go:build windows

package agents

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"salcara/bridge/internal/protocol"
)

// This test binary is the only executable used here. Children contain no
// provider/credential configuration and are identified by owned process handles.
func rpcLifecycleCommand(role, dir string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestRPCOwnedLifecycleHelper$")
	cmd.Env = childEnv(os.Args[0], map[string]string{"SALCARA_FAKE": "", "SALCARA_RPC_LIFECYCLE": role, "SALCARA_RPC_LIFECYCLE_DIR": dir}, nil)
	prepareCmd(cmd)
	return cmd
}

func TestRPCOwnedLifecycleHelper(t *testing.T) {
	role := os.Getenv("SALCARA_RPC_LIFECYCLE")
	if role == "" {
		return
	}
	dir := os.Getenv("SALCARA_RPC_LIFECYCLE_DIR")
	switch role {
	case "stdout-child":
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Join(dir, "release-child")); err == nil {
				os.Exit(0)
			}
			time.Sleep(10 * time.Millisecond)
		}
		os.Exit(0)
	case "closed-output-alive":
		_ = os.Stdout.Close()
		_ = os.Stderr.Close()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Join(dir, "release-parent")); err == nil {
				os.Exit(0)
			}
			time.Sleep(10 * time.Millisecond)
		}
		os.Exit(0)
	case "eof-parent":
		child := rpcLifecycleCommand("stdout-child", dir)
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"method": "fixture/child", "params": map[string]any{"pid": child.Process.Pid}})
		_, _ = io.Copy(io.Discard, os.Stdin)
		// Deliberately do not wait for child: reproduce the exited-parent /
		// inherited pipe shape, not a real Codex or user's other application.
		os.Exit(0)
	case "normal-exit":
		scanner := bufio.NewScanner(os.Stdin)
		if scanner.Scan() {
			var request struct {
				ID int64 `json:"id"`
			}
			_ = json.Unmarshal(scanner.Bytes(), &request)
			encoder := json.NewEncoder(os.Stdout)
			_ = encoder.Encode(map[string]any{"method": "fixture/first"})
			_ = encoder.Encode(map[string]any{"id": request.ID, "result": map[string]any{"ok": true}})
			_ = encoder.Encode(map[string]any{"method": "fixture/final"})
		}
		os.Exit(0)
	}
	os.Exit(3)
}

func rpcOwnedHandle(t *testing.T, pid int) windows.Handle {
	t.Helper()
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		t.Fatal("cannot obtain synthetic owned process handle", err)
	}
	t.Cleanup(func() { _ = windows.CloseHandle(handle) })
	return handle
}

func TestRPCKillRetiresExitedParentWithInheritedPipes(t *testing.T) {
	dir := t.TempDir()
	childPID := make(chan int, 1)
	cmd := rpcLifecycleCommand("eof-parent", dir)
	conn, err := startRPC(cmd, func(method string, raw json.RawMessage) {
		if method == "fixture/child" {
			var child struct {
				PID int `json:"pid"`
			}
			if json.Unmarshal(raw, &child) == nil {
				childPID <- child.PID
			}
		}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	parent := rpcOwnedHandle(t, cmd.Process.Pid)
	t.Cleanup(func() {
		conn.Kill()
		select {
		case <-conn.done:
		case <-time.After(2 * time.Second):
			t.Error("synthetic connection cleanup did not settle")
		}
	})
	var pid int
	select {
	case pid = <-childPID:
	case <-time.After(3 * time.Second):
		t.Fatal("synthetic child did not start")
	}
	child := rpcOwnedHandle(t, pid)
	t.Cleanup(func() {
		// A handle captured while this owned child was live avoids PID reuse.
		_ = windows.TerminateProcess(child, 0)
		_, _ = windows.WaitForSingleObject(child, 2000)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- conn.Call(ctx, "turn/start", map[string]any{"input": "synthetic only"}, nil) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn.mu.Lock()
		pending := len(conn.pending)
		conn.mu.Unlock()
		if pending == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("synthetic mutation was not waiting")
		}
		time.Sleep(time.Millisecond)
	}
	_ = conn.stdin.Close()
	if status, err := windows.WaitForSingleObject(parent, 2000); err != nil || status != windows.WAIT_OBJECT_0 {
		t.Fatal("synthetic parent did not exit on stdin EOF", status, err)
	}
	if status, err := windows.WaitForSingleObject(child, 0); err != nil || status != uint32(windows.WAIT_TIMEOUT) {
		t.Fatal("fixture did not retain inherited pipes after parent exit", status, err)
	}
	started := time.Now()
	conn.Kill()
	select {
	case <-conn.done:
	case <-time.After(2 * time.Second):
		t.Fatal("Kill left RPC waiting for stdout from an orphaned owned child")
	}
	if time.Since(started) > 6*time.Second {
		t.Fatal("owned process cleanup exceeded its bound")
	}
	select {
	case err := <-result:
		if err == nil || protocol.ExecutionErrorCode(err) != "command_delivery_uncertain" {
			t.Fatal("possibly delivered mutation was not marked uncertain", err)
		}
	case <-time.After(time.Second):
		t.Fatal("closed transport left a mutating RPC pending")
	}
	if conn.alive() {
		t.Fatal("retired source still reported alive")
	}
	if !conn.rootProcessExitConfirmed() {
		t.Fatal("real parent exit was not confirmed after wait")
	}
	if conn.processExitConfirmed() || !errors.Is(conn.err, errRPCProcessExitUnconfirmed) {
		t.Fatal("exited root falsely confirmed its live inherited-pipe descendant")
	}
	var calls sync.WaitGroup
	for range 8 {
		calls.Add(1)
		go func() { defer calls.Done(); conn.Kill() }()
	}
	calls.Wait()
}

func TestRPCNormalExitKeepsFinalNotificationsInOrder(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	conn, err := startRPC(rpcLifecycleCommand("normal-exit", t.TempDir()), func(method string, _ json.RawMessage) {
		mu.Lock()
		methods = append(methods, method)
		mu.Unlock()
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Kill)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var response struct {
		OK bool `json:"ok"`
	}
	if err := conn.Call(ctx, "thread/read", nil, &response); err != nil || !response.OK {
		t.Fatal("normal final response was lost", err)
	}
	select {
	case <-conn.done:
	case <-ctx.Done():
		t.Fatal("normal exit did not settle")
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(methods, ",") != "fixture/first,fixture/final" {
		t.Fatal("normal final notification ordering changed", strconv.Quote(strings.Join(methods, ",")))
	}
}

func TestRPCForcedRetirementBoundsUnconfirmedProcessExit(t *testing.T) {
	conn, err := startRPC(rpcLifecycleCommand("closed-output-alive", t.TempDir()), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	owned := rpcOwnedHandle(t, conn.cmd.Process.Pid)
	t.Cleanup(func() {
		_ = windows.TerminateProcess(owned, 0)
		_, _ = windows.WaitForSingleObject(owned, 2000)
		conn.closeOwnedPipes()
	})
	// Fault injection models an OS kill failure without replacing any global
	// process hook or asking the test to kill an unrelated application.
	started := time.Now()
	conn.killOnce.Do(func() {
		close(conn.killCh)
		conn.closeOwnedPipes()
	})
	select {
	case <-conn.done:
	case <-time.After(4 * time.Second):
		t.Fatal("unconfirmed process exit blocked transport retirement forever")
	}
	if time.Since(started) > 4*time.Second || !errors.Is(conn.err, errRPCProcessExitUnconfirmed) {
		t.Fatal("OS failure was not bounded and reported as unconfirmed", conn.err)
	}
	if status, err := windows.WaitForSingleObject(owned, 0); err != nil || status != uint32(windows.WAIT_TIMEOUT) {
		t.Fatal("fault injection did not leave the owned fixture alive", status, err)
	}
	if conn.alive() {
		t.Fatal("unconfirmed dead transport remained usable")
	}
	if conn.processExitConfirmed() {
		t.Fatal("transport retirement falsely confirmed an OS process exit")
	}
	_ = windows.TerminateProcess(owned, 0)
	select {
	case <-conn.processWaitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("owned process reaper did not confirm the eventual real exit")
	}
	if !conn.processExitConfirmed() {
		t.Fatal("eventual real exit cannot clear the failed-kill fence")
	}
}

func TestRPCSetupFailureClosesOnlyNewOwnedPipes(t *testing.T) {
	for _, stage := range []string{"stdout", "stderr", "start"} {
		t.Run(stage, func(t *testing.T) {
			cmd := exec.Command(filepath.Join(t.TempDir(), "not-a-real-executable.exe"))
			if stage == "stdout" {
				cmd.Stdout = io.Discard
			}
			if stage == "stderr" {
				cmd.Stderr = io.Discard
			}
			if conn, err := startRPC(cmd, nil, nil); err == nil || conn != nil {
				t.Fatal("invalid synthetic startup unexpectedly succeeded")
			}
			if input, ok := cmd.Stdin.(*os.File); ok {
				if _, err := input.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
					t.Fatal("child-facing stdin leaked after startup failure", err)
				}
			}
			if output, ok := cmd.Stdout.(*os.File); ok {
				if _, err := output.Write([]byte("synthetic")); !errors.Is(err, os.ErrClosed) {
					t.Fatal("child-facing stdout leaked after startup failure", err)
				}
			}
			if output, ok := cmd.Stderr.(*os.File); ok {
				if _, err := output.Write([]byte("synthetic")); !errors.Is(err, os.ErrClosed) {
					t.Fatal("child-facing stderr leaked after startup failure", err)
				}
			}
			if stage == "stdout" && cmd.Stdout != io.Discard || stage == "stderr" && cmd.Stderr != io.Discard {
				t.Fatal("caller-owned output configuration was replaced")
			}
		})
	}
}

type rpcLifecycleInterruptedWriter struct {
	writing chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (w *rpcLifecycleInterruptedWriter) Write(data []byte) (int, error) {
	close(w.writing)
	<-w.closed
	return len(data), io.ErrClosedPipe
}

func (w *rpcLifecycleInterruptedWriter) Close() error {
	w.once.Do(func() { close(w.closed) })
	return nil
}

func TestRPCKillDuringWritePreservesMutationUncertainty(t *testing.T) {
	for _, method := range []string{"turn/start", "thread/read", "thread/turns/list"} {
		t.Run(method, func(t *testing.T) {
			writer := &rpcLifecycleInterruptedWriter{writing: make(chan struct{}), closed: make(chan struct{})}
			conn := &rpcConn{stdin: writer, pending: map[int64]chan rpcResult{}, done: make(chan struct{}), killCh: make(chan struct{})}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- conn.Call(ctx, method, nil, nil) }()
			select {
			case <-writer.writing:
			case <-ctx.Done():
				t.Fatal("synthetic write did not start")
			}
			conn.Kill()
			select {
			case err := <-result:
				if method == "turn/start" && !errors.Is(err, errRPCDeliveryInterrupted) || method != "turn/start" && !errors.Is(err, io.ErrClosedPipe) {
					t.Fatal("shutdown write uncertainty did not match the RPC kind", err)
				}
			case <-ctx.Done():
				t.Fatal("shutdown did not release the interrupted write")
			}
			conn.mu.Lock()
			pending := len(conn.pending)
			conn.mu.Unlock()
			if pending != 0 {
				t.Fatal("interrupted write retained a pending RPC")
			}
		})
	}
}

func TestRPCForcedShutdownPreservesReadOnlyWaitRetryability(t *testing.T) {
	for _, method := range []string{"turn/start", "thread/read", "thread/turns/list"} {
		t.Run(method, func(t *testing.T) {
			writer := &rpcFixtureWriter{calls: make(chan []byte, 1)}
			conn := &rpcConn{stdin: writer, pending: map[int64]chan rpcResult{}, done: make(chan struct{}), killCh: make(chan struct{})}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- conn.Call(ctx, method, nil, nil) }()
			select {
			case <-writer.calls:
			case <-ctx.Done():
				t.Fatal("synthetic RPC was not sent")
			}
			conn.Kill()
			closed := errors.New("synthetic transport closed")
			conn.shutdown(closed)
			select {
			case err := <-result:
				if method == "turn/start" && !errors.Is(err, errRPCDeliveryInterrupted) || method != "turn/start" && !errors.Is(err, closed) {
					t.Fatal("pending RPC failure did not preserve read/mutation distinction", err)
				}
			case <-ctx.Done():
				t.Fatal("shutdown did not settle pending synthetic RPC")
			}
		})
	}
}
