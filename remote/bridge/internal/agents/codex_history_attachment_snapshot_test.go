package agents

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"salcara/bridge/internal/protocol"
)

func attachmentSnapshotThread() cxThread {
	return cxThread{ID: "attachment-history", Turns: []cxTurn{{ID: "fixture-turn", Status: "completed", Items: []json.RawMessage{
		json.RawMessage(`{"type":"userMessage","id":"prompt","content":[{"type":"text","text":"fixture prompt"}]}`),
	}}}}
}

func TestCodexHistoryAttachmentsAreIndependentSnapshots(t *testing.T) {
	a := newCodexAgent(func(protocol.Event) {}, func() Settings { return Settings{} }, "", "")
	defer a.thr.Close()
	want := protocol.AttachmentRef{ID: "att_00000000000000000000000000000001", Mime: "image/png", Size: 24}
	source := []protocol.AttachmentRef{want}
	a.mu.Lock()
	a.threadLocked("attachment-history").messageAttachments["prompt"] = source
	a.mu.Unlock()

	events := a.historyEvents("codex:attachment-history", attachmentSnapshotThread())
	if len(events) < 1 || len(events[0].Attachments) != 1 || events[0].Attachments[0] != want {
		t.Fatalf("history lost the exact opaque attachment: %+v", events)
	}
	// Updating the mutable source after conversion must not change an already
	// published history snapshot, and the receiver must not mutate its source.
	a.mu.Lock()
	source[0].Mime = "image/jpeg"
	a.mu.Unlock()
	if events[0].Attachments[0] != want {
		t.Fatal("history retained the mutable attachment source slice")
	}
	events[0].Attachments[0].Size = 999
	a.mu.Lock()
	got := a.threads["attachment-history"].messageAttachments["prompt"][0]
	a.mu.Unlock()
	if got.Mime != "image/jpeg" || got.Size != want.Size {
		t.Fatal("mutating returned history changed the agent attachment table")
	}
}

func TestCodexHistoryAttachmentsConcurrentEchoAndSnapshots(t *testing.T) {
	a := newCodexAgent(func(protocol.Event) {}, func() Settings { return Settings{} }, "", "")
	defer a.thr.Close()
	want := protocol.AttachmentRef{ID: "att_00000000000000000000000000000001", Mime: "image/png", Size: 24}
	a.mu.Lock()
	a.threadLocked("attachment-history").messageAttachments["prompt"] = []protocol.AttachmentRef{want}
	a.mu.Unlock()

	start := make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(3)
	go func() {
		defer workers.Done()
		<-start
		for i := 0; i < 500; i++ {
			// Use the real picture echo path: each new localImage records an
			// opaque reference in this thread while another goroutine reads it.
			raw := json.RawMessage(fmt.Sprintf(`{"threadId":"attachment-history","item":{"type":"userMessage","id":"echo-%d","content":[{"type":"localImage","path":"att_%032x.png"}]}}`, i, i+2))
			a.onNotify("item/completed", raw)
		}
	}()
	go func() {
		defer workers.Done()
		<-start
		for i := 0; i < 500; i++ {
			// Source slice replacement and mutation use the same lock as the
			// actual notification handler; history must use that lock too.
			a.mu.Lock()
			a.threads["attachment-history"].messageAttachments["prompt"] = []protocol.AttachmentRef{want}
			a.threads["attachment-history"].messageAttachments["prompt"][0].Width = i
			a.mu.Unlock()
		}
	}()
	go func() {
		defer workers.Done()
		<-start
		for i := 0; i < 500; i++ {
			events := a.historyEvents("codex:attachment-history", attachmentSnapshotThread())
			if len(events) < 1 || len(events[0].Attachments) != 1 || events[0].Attachments[0].ID != want.ID {
				t.Error("concurrent history lost its session's exact attachment")
				return
			}
			// Read after conversion too: under -race this catches a mutable
			// source slice escaping the mutex even if map lookup was protected.
			_ = events[0].Attachments[0].Width
		}
	}()
	close(start)
	workers.Wait()
	a.mu.Lock()
	count := len(a.threads["attachment-history"].messageAttachments)
	a.mu.Unlock()
	if count != 501 {
		t.Fatalf("real image echo path did not record all references: %d", count)
	}
}
