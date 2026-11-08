//go:build !windows

package agents

import (
	"os/exec"
	"testing"
	"time"
)

func TestRPCSignalExitRequiresWaitPublicationAndTreeConfirmation(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	c := &rpcConn{cmd: cmd, processWaitDone: make(chan struct{})}
	if c.processExitConfirmed() {
		t.Fatal("running process was confirmed terminated")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if cmd.ProcessState == nil || cmd.ProcessState.Exited() {
		t.Fatal("fixture was not signal terminated")
	}
	if c.processExitConfirmed() {
		t.Fatal("Wait completion was used before publication")
	}
	close(c.processWaitDone)
	if !c.processExitConfirmed() {
		t.Fatal("signal termination permanently fenced the next worker")
	}
	c.treeTermination.begin()
	if c.processExitConfirmed() {
		t.Fatal("root exit bypassed pending process-tree termination")
	}
	c.treeTermination.finish(errProcessTreeTerminationUnconfirmed)
	if c.processExitConfirmed() {
		t.Fatal("root exit bypassed failed process-tree termination")
	}
}

func TestRPCSignalTerminatedWorkerCompletesShutdown(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	prepareCmd(cmd)
	c, err := startRPC(cmd, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Kill)
	c.Kill()
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
		t.Fatal("signal-terminated RPC worker did not finish shutdown")
	}
	if !c.processExitConfirmed() {
		t.Fatal("terminated RPC process remained fenced after Wait and tree cleanup")
	}
}
