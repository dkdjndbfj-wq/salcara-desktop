package hubclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func TestLongOfflineGapIsScopedAndContainsNoDroppedContent(t *testing.T) {
	c := economyClient(t)
	c.o.QueueCap = 1
	c.Push(protocol.Event{SessionKey: "codex:old", Tool: "codex", Type: "approval.request", ApprovalID: "stale", Text: "PRIVATE DROPPED BODY"})
	c.Push(snapshot("retained", true))
	upload, marks := c.recoveryBatchLocked(c.queue)
	if len(upload) != 2 || upload[0].SessionKey != "codex:old" || upload[0].Type != "notice" || !strings.HasPrefix(upload[0].Text, historyGapPrefix) {
		t.Fatalf("recovery notification missing: %+v", upload)
	}
	if len(marks) != 1 || upload[0].ApprovalID != "" || strings.Contains(upload[0].Text, "PRIVATE") {
		t.Fatal("recovery metadata retained old content/approval")
	}
	if len(c.queue) != 1 || c.queue[0].Text != "retained" {
		t.Fatal("gap notice changed bounded event queue")
	}
}

func TestLongOfflineRecoveryMetadataIsBounded(t *testing.T) {
	c := economyClient(t)
	c.o.QueueCap = 2
	for i := 0; i < 10000; i++ {
		c.Push(protocol.Event{SessionKey: fmt.Sprintf("codex:%d", i), Tool: "codex", Type: "message", ID: "m", Text: "data"})
	}
	if len(c.queue) != 2 || len(c.recovery) != maxRecoverySessions || !c.recoveryAll {
		t.Fatalf("unbounded state: queue=%d recovery=%d", len(c.queue), len(c.recovery))
	}
	upload, _ := c.recoveryBatchLocked(c.queue)
	if len(upload) > 104 {
		t.Fatalf("unbounded upload: %d", len(upload))
	}
	for _, event := range c.queue {
		found := false
		for _, item := range upload {
			found = found || item.Type == "notice" && item.SessionKey == event.SessionKey
		}
		if !found {
			t.Fatal("metadata overflow did not mark a retained session")
		}
	}
}

func TestLongOfflineMetadataOverflowSurvivesDrainAndAvoidsRepeatedReads(t *testing.T) {
	c := economyClient(t)
	for i := 0; i <= maxRecoverySessions; i++ {
		c.noteDroppedLocked([]protocol.Event{{SessionKey: fmt.Sprintf("codex:%d", i), Tool: "codex"}})
	}
	for len(c.recovery) > 0 {
		_, marks := c.recoveryBatchLocked(nil)
		c.acknowledgeRecoveryLocked(marks)
	}
	if !c.recoveryAll {
		t.Fatal("empty upload queue forgot untracked lost sessions")
	}
	event := protocol.Event{SessionKey: fmt.Sprintf("codex:%d", maxRecoverySessions), Tool: "codex", Type: "message"}
	upload, marks := c.recoveryBatchLocked([]protocol.Event{event})
	if len(upload) != 2 || upload[0].Type != "notice" || upload[0].SessionKey != event.SessionKey {
		t.Fatal("untracked session did not receive recovery after drain")
	}
	c.acknowledgeRecoveryLocked(marks)
	upload, _ = c.recoveryBatchLocked([]protocol.Event{event})
	if len(upload) != 1 || upload[0].Type != "message" {
		t.Fatal("recently recovered session incurred an unnecessary repeated read")
	}
}

func TestLongOfflineOldOverflowAcknowledgementCannotCoverNewOverflow(t *testing.T) {
	c := economyClient(t)
	for i := 0; i <= maxRecoverySessions; i++ {
		c.noteDroppedLocked([]protocol.Event{{SessionKey: fmt.Sprintf("codex:%d", i), Tool: "codex"}})
	}
	event := protocol.Event{SessionKey: "codex:untracked", Tool: "codex", Type: "message"}
	_, oldMarks := c.recoveryBatchLocked([]protocol.Event{event})
	oldEpoch := c.recoveryOverflow
	c.noteDroppedLocked([]protocol.Event{{SessionKey: "codex:new-overflow", Tool: "codex"}})
	c.acknowledgeRecoveryLocked(oldMarks)
	if c.recoveryOverflow == oldEpoch || c.recoverySeen[event.SessionKey] == c.recoveryOverflow {
		t.Fatal("old acknowledgement covered a newer untracked gap")
	}
	upload, _ := c.recoveryBatchLocked([]protocol.Event{event})
	found := false
	for _, item := range upload {
		found = found || item.Type == "notice" && item.SessionKey == event.SessionKey
	}
	if !found {
		t.Fatal("new overflow was silently cleared by an old acknowledgement")
	}
}

func TestLongOfflineOverflowAcknowledgementsAndToolsRemainBounded(t *testing.T) {
	c := economyClient(t)
	c.recoveryAll = true
	c.recoveryOverflow = 1
	c.recoveryGeneration = 1
	for i := 0; i < 10000; i++ {
		event := protocol.Event{SessionKey: fmt.Sprintf("codex:%d", i), Tool: strings.Repeat("PRIVATE", 10000)}
		upload, marks := c.recoveryBatchLocked([]protocol.Event{event})
		if upload[0].Tool != "" {
			t.Fatal("unbounded tool identity retained in recovery metadata")
		}
		c.acknowledgeRecoveryLocked(marks)
	}
	if len(c.recoverySeen) != maxRecoverySessions || len(c.recoverySeenOrder) != maxRecoverySessions {
		t.Fatal("overflow acknowledgement metadata is unbounded")
	}
	c.clearRecoveryLocked()
	if c.recoveryOverflow != 0 || len(c.recoverySeen) != 0 || len(c.recoverySeenOrder) != 0 {
		t.Fatal("station reset retained overflow acknowledgement identities")
	}
}

func TestLongOfflineByteBudgetAndOversizedEventDoNotBlockProgress(t *testing.T) {
	c := economyClient(t)
	c.o.QueueBytes = 20 << 10
	for i := 0; i < 100; i++ {
		c.Push(protocol.Event{SessionKey: "codex:large", Type: "message", ID: fmt.Sprint(i), Text: strings.Repeat("x", 1000)})
	}
	if c.queueBytes > c.o.QueueBytes || len(c.recovery) != 1 || len(c.queue) >= 100 {
		t.Fatal("byte budget failed to bound retained offline progress")
	}
	c.Push(protocol.Event{SessionKey: "codex:oversized", Type: "tool", Output: strings.Repeat("private", 300000)})
	if _, marked := c.recovery["codex:oversized"]; !marked {
		t.Fatal("oversized event was silently lost")
	}
	c.Push(snapshot("later-small-event", true))
	if c.queue[len(c.queue)-1].Text != "later-small-event" {
		t.Fatal("oversized event blocked later progress")
	}
	counted := 0
	for _, event := range c.queue {
		counted += eventWeight(event)
	}
	if c.queueBytes != counted {
		t.Fatal("queue accounting diverged")
	}
}

func TestLongOfflineSnapshotCoalescingUpdatesByteAccounting(t *testing.T) {
	c := economyClient(t)
	c.Push(snapshot("a", false))
	c.Push(snapshot(strings.Repeat("b", 2000), false))
	if len(c.queue) != 1 || c.queueBytes != eventWeight(c.queue[0]) {
		t.Fatal("coalescing byte count is stale")
	}
	c.Push(snapshot("c", true))
	if c.queueBytes != eventWeight(c.queue[0]) {
		t.Fatal("shrinking snapshot byte count is stale")
	}
}

func TestLongOfflineUploadsRespectByteBudget(t *testing.T) {
	queue := make([]protocol.Event, 200)
	for i := range queue {
		queue[i] = snapshot(strings.Repeat("x", 30000), true)
	}
	n := uploadCount(queue)
	if n < 1 || n >= 200 {
		t.Fatal("large upload was not split")
	}
	size := 0
	for _, event := range queue[:n] {
		size += eventWeight(event)
	}
	if size > maxUploadWeight {
		t.Fatal("upload byte budget exceeded")
	}
}

func TestLongOfflineOldAcknowledgementCannotClearNewGap(t *testing.T) {
	c := economyClient(t)
	c.o.QueueCap = 1
	c.Push(snapshot("old", true))
	c.Push(snapshot("new", true))
	_, marks := c.recoveryBatchLocked(c.queue)
	c.Push(snapshot("newest", true))
	c.acknowledgeRecoveryLocked(marks)
	if len(c.recovery) != 1 {
		t.Fatal("old acknowledgement erased a newer gap")
	}
}

func TestLongOfflineOverflowKeepsImmutableInFlightPrefix(t *testing.T) {
	c := economyClient(t)
	c.o.QueueCap = 3
	for _, text := range []string{"in-flight-1", "in-flight-2", "unsent"} {
		c.Push(protocol.Event{Type: "message", SessionKey: "codex:test", Tool: "codex", ID: text, Text: text, Final: true})
	}
	c.inFlight = 2
	c.Push(protocol.Event{Type: "message", SessionKey: "codex:test", Tool: "codex", ID: "latest", Text: "latest", Final: true})
	if c.inFlight != 2 || len(c.queue) != 3 || c.queue[0].Text != "in-flight-1" || c.queue[1].Text != "in-flight-2" || c.queue[2].Text != "latest" {
		t.Fatalf("in-flight prefix dropped: %+v", c.queue)
	}
}

func TestLongOfflineStationChangeClearsGapMetadata(t *testing.T) {
	c := economyClient(t)
	c.o.QueueCap = 1
	c.Push(snapshot("one", true))
	c.Push(snapshot("two", true))
	if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.HubURL = "https://new.example/salcara-hub"; return nil }); err != nil {
		t.Fatal(err)
	}
	c.Push(snapshot("new station", true))
	if len(c.recovery) != 0 || c.recoveryAll {
		t.Fatal("old station recovery metadata leaked")
	}
}

func TestLongOfflineGapRetriedUntilAcknowledgedWithoutCommandExecution(t *testing.T) {
	batches := make(chan []protocol.Event, 4)
	var mu sync.Mutex
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/bridge/events" {
			t.Error("recovery attempted a non-event endpoint")
			w.WriteHeader(500)
			return
		}
		var body struct {
			Events []protocol.Event `json:"events"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		requests++
		n := requests
		mu.Unlock()
		batches <- body.Events
		if n == 1 {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	}))
	defer server.Close()
	c := economyClient(t)
	c.o.QueueCap = 1
	c.o.FlushInterval = 10 * time.Millisecond
	if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.HubURL = server.URL; return nil }); err != nil {
		t.Fatal(err)
	}
	c.setState(StateConnected, "")
	c.Push(protocol.Event{Type: "message", SessionKey: "codex:lost", Tool: "codex", ID: "old", Text: "old"})
	c.Push(snapshot("new", true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.flushLoop(ctx)
	for i := 0; i < 2; i++ {
		select {
		case batch := <-batches:
			if len(batch) != 2 || batch[0].Type != "notice" || batch[0].SessionKey != "codex:lost" {
				t.Fatalf("gap not retried: %+v", batch)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("recovery upload missing")
		}
	}
	waitFor(t, "recovery acknowledged", func() bool {
		c.qmu.Lock()
		defer c.qmu.Unlock()
		return len(c.queue) == 0 && len(c.recovery) == 0
	})
}
