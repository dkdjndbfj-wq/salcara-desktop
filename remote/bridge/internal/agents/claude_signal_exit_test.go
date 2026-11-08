//go:build !windows

package agents

import (
	"os/exec"
	"testing"
)

// Only a synthetic sleeping process; no real Agent or credentials involved.
func TestClaudeSignalTerminatedWorkerIsConfirmed(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skip("sleep fixture unavailable")
	}
	defer func() { _ = cmd.Process.Kill() }()
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if cmd.ProcessState == nil || cmd.ProcessState.Exited() {
		t.Fatal("fixture not signal terminated")
	}
	p := &claudeProc{cmd: cmd, processWaitDone: make(chan struct{})}
	if p.processExitConfirmed() {
		t.Fatal("published process fence before Wait completion")
	}
	close(p.processWaitDone)
	if !p.processExitConfirmed() {
		t.Fatal("signal exit permanently fenced later Claude workers")
	}
}
