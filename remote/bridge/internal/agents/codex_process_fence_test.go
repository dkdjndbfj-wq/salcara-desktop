package agents

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestCodexUnconfirmedRetiredProcessFencesEveryRestartAttempt(t *testing.T) {
	a, _, _ := newResumeSafetyAgent(t, cxThread{}, false)
	retired := a.conn
	retired.err = errRPCProcessExitUnconfirmed
	retired.closed = true
	retired.processWaitDone = make(chan struct{})
	close(retired.done)
	thread := a.threadLocked("fixture-running")
	thread.loaded = true
	thread.turnID = "actual-turn"
	a.onExit(retired)
	for range 3 {
		if _, err := a.ensure(context.Background()); !errors.Is(err, errRPCProcessExitUnconfirmed) {
			t.Fatalf("retry lost unconfirmed process fence: %v", err)
		}
	}
	if a.conn != nil || a.unconfirmedProcess != retired || thread.info.Controllable || !thread.statusUnconfirmed {
		t.Fatal("unconfirmed exit published a replacement or idle ownership")
	}
	close(retired.processWaitDone) // A wait error without an OS ProcessState is not confirmation.
	if _, err := a.ensure(context.Background()); !errors.Is(err, errRPCProcessExitUnconfirmed) {
		t.Fatal("wait completion without OS confirmation removed fence")
	}
}

func TestCodexUnpublishedStartupProcessRetirementKeepsFence(t *testing.T) {
	a, _, _ := newResumeSafetyAgent(t, cxThread{}, false)
	retired := a.conn
	a.conn = nil
	retired.err = errRPCProcessExitUnconfirmed
	retired.closed = true
	retired.processWaitDone = make(chan struct{})
	close(retired.done)
	if !errors.Is(a.fenceUnconfirmedProcess(retired), errRPCProcessExitUnconfirmed) {
		t.Fatal("startup failure discarded unconfirmed owned process")
	}
	if _, err := a.ensure(context.Background()); !errors.Is(err, errRPCProcessExitUnconfirmed) {
		t.Fatal("unpublished process did not block later startup")
	}
}

func TestCodexActualOSExitReleasesRestartFence(t *testing.T) {
	a, _, _ := newResumeSafetyAgent(t, cxThread{}, false)
	retired := a.conn
	retired.err, retired.closed = errRPCProcessExitUnconfirmed, true
	retired.processWaitDone = make(chan struct{})
	close(retired.done)
	a.onExit(retired)
	// Reap only this synthetic test binary; never launch the user's Codex.
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	retired.cmd = cmd
	close(retired.processWaitDone)
	a.exeOverride = filepath.Join(t.TempDir(), "intentionally-unavailable-fixture.exe")
	a.nextStart = time.Time{}
	if _, err := a.ensure(context.Background()); errors.Is(err, errRPCProcessExitUnconfirmed) || err == nil {
		t.Fatalf("actual OS exit did not release restart fence: %v", err)
	}
	if a.unconfirmedProcess != nil {
		t.Fatal("retired OS owner retained after confirmed exit")
	}
}
