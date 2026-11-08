package agents

import (
	"context"
	"os"
	"reflect"
	"testing"
)

func TestCodexIdleAPIRestartResumesUnchangedRecentHistory(t *testing.T) {
	for _, change := range []string{"unchanged", "resume-writes-history", "changed-file", "active-native", "unconfirmed-native"} {
		t.Run(change, func(t *testing.T) {
			path := resumeSafetyRollout(t, 0)
			thread := cxThread{ID: "original", Path: &path, Status: cxThreadStatus{Type: "notLoaded"}, Turns: []cxTurn{{ID: "old-turn", Status: "completed"}}}
			a, transport, _ := newResumeSafetyAgent(t, thread, false)
			cached := a.threadLocked(thread.ID)
			cached.loaded, cached.path, cached.info.Status = true, path, "idle"
			captured := a.captureIdleCodexRolloutsLocked()
			a.markUnloadedLocked()
			if len(captured) != 1 {
				t.Fatal("owned idle history was not captured")
			}
			// Only the production confirmed-exit path installs this ephemeral
			// proof. The transport here is synthetic and owns no OS process.
			cached.idleRestartRollout = captured[cached]
			opts := TurnOptions{}
			switch change {
			case "resume-writes-history":
				opts.PermissionMode = "ask"
				transport.confirmAsk = true
				transport.onResume = func() {
					transport.thread.Status.Type = "idle"
					if err := os.WriteFile(path, []byte("native resume appended its own session metadata\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "changed-file":
				if err := os.WriteFile(path, []byte("another writer appended a turn\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "active-native":
				transport.thread.Turns = []cxTurn{{ID: "other-turn", Status: "inProgress"}}
			case "unconfirmed-native":
				transport.thread.Status.Type = "future-unknown-status"
			}
			err := a.SendWithOptions(context.Background(), thread.ID, "continue on API B", opts)
			if change == "unchanged" || change == "resume-writes-history" {
				if err != nil || !cached.loaded || cached.idleRestartRollout != nil {
					t.Fatalf("clean API switch did not resume the original: %v", err)
				}
				if !reflect.DeepEqual(transport.calls, []string{"thread/read", "config/read", "thread/resume", "config/read", "turn/start"}) {
					t.Fatal("original thread was not read and resumed", transport.calls)
				}
			} else if err == nil || !reflect.DeepEqual(transport.calls, []string{"thread/read"}) {
				t.Fatalf("changed/unconfirmed history permitted a mutation: %v %v", err, transport.calls)
			}
		})
	}
}

func TestCodexIdleAPIRestartRequiresConfirmedExit(t *testing.T) {
	a, _, _ := newResumeSafetyAgent(t, cxThread{}, false)
	thread := a.threadLocked("original")
	thread.loaded, thread.path, thread.info.Status = true, resumeSafetyRollout(t, 0), "idle"
	captured := a.captureIdleCodexRolloutsLocked()
	a.markUnloadedLocked()
	a.rememberIdleCodexRolloutsLocked(&rpcConn{processWaitDone: make(chan struct{})}, captured)
	if thread.idleRestartRollout != nil {
		t.Fatal("unconfirmed process exit granted a recent-history exception")
	}
}
