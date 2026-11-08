//go:build windows

package agents

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"salcara/bridge/internal/protocol"
)

// The acknowledgement below is deliberately synthetic: it models a successful
// tree-termination result whose root Wait has not completed. The harmless owned
// root is kept alive across the real two-second retirement timeout, then ended
// through its original handle. No claim is made about real taskkill timing.
func TestClaudeLateWaitCompletionReceipt(t *testing.T) {
	exe := claudeLifecycleExecutable(t)
	for _, name := range []string{"completed", "interrupted", "failed-tree", "replaced-lease", "failed-terminal", "busy-worker", "missing-transcript"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("SALCARA_CLAUDE_LIFECYCLE_ROLE", "closed-output-alive")
			t.Setenv("SALCARA_CLAUDE_LIFECYCLE_DIR", dir)
			settings := Settings{BridgeExe: exe, ClaudeRoot: "http://127.0.0.1:1", ClaudeKey: "synthetic-only"}
			a := newClaudeAgent(func(protocol.Event) {}, func() Settings { return settings }, exe, t.TempDir())
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
			transcriptDir := filepath.Join(a.hist.dir, "-synthetic")
			if err := os.MkdirAll(transcriptDir, 0700); err != nil {
				t.Fatal(err)
			}
			transcript := filepath.Join(transcriptDir, p.id+".jsonl")
			if err := os.WriteFile(transcript, []byte("{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":\"synthetic\"},\"cwd\":\"/synthetic\"}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			a.hist.noteOwnedTurn(p)
			a.hist.noteOwnedTerminal(p, "completed")
			if name == "interrupted" {
				p.mu.Lock()
				p.turns, p.turnIDs = 1, []string{"synthetic-interrupted-turn"}
				p.mu.Unlock()
			}
			a.stopWithTreeKiller(p, func(*exec.Cmd) error {
				if name == "failed-tree" {
					return errors.New("synthetic unresolved process tree")
				}
				return nil
			})
			waitOwnedTreeTest(t, p.done, "bounded transport retirement before late root Wait")
			if !p.exitCleanupReady.Load() || p.rootProcessExitConfirmed() || a.hist.ownedCompletion(p.id) {
				t.Fatal("fixture did not complete cleanup while root Wait remained outstanding")
			}
			if status, err := windows.WaitForSingleObject(root, 0); err != nil || status != uint32(windows.WAIT_TIMEOUT) {
				t.Fatal("owned root must still be alive at the late-Wait barrier", status, err)
			}
			switch name {
			case "replaced-lease":
				newer := &claudeProc{id: p.id}
				a.hist.noteOwnedTurn(newer)
				a.hist.noteOwnedTerminal(newer, "completed")
			case "failed-terminal":
				a.hist.noteOwnedTerminal(p, "failed")
			case "busy-worker":
				p.mu.Lock()
				p.turns = 1
				p.mu.Unlock()
			case "missing-transcript":
				if err := os.Remove(transcript); err != nil {
					t.Fatal(err)
				}
			}
			if err := windows.TerminateProcess(root, 0); err != nil {
				t.Fatal("cannot end owned synthetic root", err)
			}
			waitOwnedTreeTest(t, p.processWaitDone, "late root Wait")
			if !p.rootProcessExitConfirmed() {
				t.Fatal("original root was not reaped")
			}
			eligible := name == "completed" || name == "interrupted"
			if eligible {
				deadline := time.Now().Add(2 * time.Second)
				for !a.hist.ownedCompletion(p.id) && time.Now().Before(deadline) {
					time.Sleep(5 * time.Millisecond)
				}
				if !p.processExitConfirmed() || !a.hist.ownedCompletion(p.id) {
					t.Fatal("late root Wait lost the original worker completion receipt after successful tree acknowledgement and cleanup")
				}
				return
			}
			// Challenge the unchanged qualifier synchronously with its strongest
			// normalExit argument. These stable negative guards must reject
			// both this call and any still-scheduling late-Wait callback.
			a.hist.recordOwnedExit(p, true)
			if a.hist.ownedCompletion(p.id) {
				t.Fatal("late root Wait bypassed an existing recovery qualification")
			}
			if name == "failed-tree" && p.processExitConfirmed() {
				t.Fatal("late root Wait cleared the failed-tree fence")
			}
		})
	}
}
