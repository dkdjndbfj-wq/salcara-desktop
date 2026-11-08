//go:build windows

package agents

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"salcara/bridge/internal/protocol"
)

func codexCloseFixture(t *testing.T) (*codexAgent, string) {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(t.TempDir(), "owned-codex-close.exe")
	build := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go.exe"), "build", "-o", exe, "./testdata/codex_close_helper")
	prepareCmd(build)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatal("cannot build owned startup fixture", err, string(output))
	}
	t.Setenv("SALCARA_CODEX_CLOSE_DIR", dir)
	t.Setenv("SALCARA_CODEX_CLOSE_ROLE", "init-barrier")
	a := newCodexAgent(func(protocol.Event) {}, func() Settings { return Settings{} }, exe, "")
	t.Cleanup(a.Close)
	return a, dir
}

func codexCloseFixtureProcess(t *testing.T, dir string) windows.Handle {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(filepath.Join(dir, "init-owned-pid")); err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil || pid <= 0 {
				t.Fatal("invalid owned startup process identifier")
			}
			return rpcOwnedHandle(t, pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("owned startup fixture did not reach initialization barrier")
	return 0
}

func TestCodexCloseKillsUnpublishedStartupBeforeReturning(t *testing.T) {
	a, dir := codexCloseFixture(t)
	started := make(chan error, 1)
	go func() { _, err := a.ensure(context.Background()); started <- err }()
	owned := codexCloseFixtureProcess(t, dir)
	t.Cleanup(func() {
		_ = windows.TerminateProcess(owned, 0)
		_, _ = windows.WaitForSingleObject(owned, 2000)
		a.Close()
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Error("owned startup did not settle during cleanup")
		}
	})
	closed := make(chan struct{})
	go func() { a.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(6 * time.Second):
		t.Fatal("Close was blocked by unfinished owned initialization")
	}
	if status, err := windows.WaitForSingleObject(owned, 1000); err != nil || status != windows.WAIT_OBJECT_0 {
		t.Fatal("Close returned while unpublished owned startup was still alive")
	}
	select {
	case err := <-started:
		started <- err // retain for cleanup's bounded drain
		if err == nil {
			t.Fatal("closed agent published a late initialized connection")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled startup did not return promptly")
	}
	a.mu.Lock()
	current := a.conn
	a.mu.Unlock()
	if current != nil {
		t.Fatal("closed agent retained a newly published connection")
	}
	for range 3 {
		if c, err := a.ensure(context.Background()); c != nil || err == nil {
			t.Fatal("closed agent admitted another startup")
		}
	}
}

func TestCodexCancelledStartupRetiresOwnedWorker(t *testing.T) {
	a, dir := codexCloseFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan error, 1)
	go func() { _, err := a.ensure(ctx); started <- err }()
	owned := codexCloseFixtureProcess(t, dir)
	t.Cleanup(func() {
		_ = windows.TerminateProcess(owned, 0)
		_, _ = windows.WaitForSingleObject(owned, 2000)
		a.Close()
	})
	cancel()
	select {
	case err := <-started:
		if err == nil {
			t.Fatal("cancelled initialization published a worker")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled initialization left an unbounded startup")
	}
	if status, err := windows.WaitForSingleObject(owned, 1000); err != nil || status != windows.WAIT_OBJECT_0 {
		t.Fatal("cancelled initialization did not stop its owned process")
	}
	a.mu.Lock()
	current, startup := a.conn, a.starting
	a.mu.Unlock()
	if current != nil || startup != nil {
		t.Fatal("cancelled startup left published state or an active startup lease")
	}
}

func TestCodexCloseBoundedWithOwnedNotifySinkBarrier(t *testing.T) {
	a, dir := codexCloseFixture(t)
	t.Setenv("SALCARA_CODEX_CLOSE_ROLE", "notify-barrier")
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	a.sink = func(e protocol.Event) {
		if e.Type == "turn" && e.Status == "started" {
			close(entered)
			<-release
		}
	}
	started := make(chan error, 1)
	go func() { _, err := a.ensure(context.Background()); started <- err }()
	owned := codexCloseFixtureProcess(t, dir)
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		_ = windows.TerminateProcess(owned, 0)
		_, _ = windows.WaitForSingleObject(owned, 2000)
		a.Close()
	})
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("owned fixture did not enter notify sink barrier")
	}
	closed := make(chan struct{})
	go func() { a.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(12 * time.Second):
		t.Fatal("non-cooperative notify sink caused unbounded explicit Close")
	}
	if status, err := windows.WaitForSingleObject(owned, 1000); err != nil || status != windows.WAIT_OBJECT_0 {
		t.Fatal("bounded Close abandoned its live owned OS process")
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-started:
		if err == nil {
			t.Fatal("closed agent published a late worker after notify sink resumed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("late sink drain did not retire cancelled startup")
	}
	a.mu.Lock()
	current := a.conn
	a.mu.Unlock()
	if current != nil {
		t.Fatal("late sink drain republished a closed agent connection")
	}
}
