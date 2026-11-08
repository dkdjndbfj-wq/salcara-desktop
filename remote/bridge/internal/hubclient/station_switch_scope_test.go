package hubclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func TestScopedStationTransferPreservesFrozenQueueAndImagesAcrossRestart(t *testing.T) {
	c, source, target := stationSwitchFixture(t, nil, nil)
	if err := c.acceptEventScope(source, "source-generation", true); err != nil {
		t.Fatal(err)
	}
	images := c.attachmentsFor(context.Background())
	ref, ok := images.importBase64(workspaceTestKey, base64.StdEncoding.EncodeToString(budgetImage("pending scoped image")))
	if !ok {
		t.Fatal("fixture image was not stored")
	}
	c.Push(protocol.Event{Type: "message", SessionKey: workspaceTestKey, ID: "pending", Text: "retained scoped progress", Attachments: []protocol.AttachmentRef{ref}})
	c.qmu.Lock()
	c.inFlight, c.inFlightBatchID = 1, "source-frozen-batch"
	c.inFlightPayload = json.RawMessage(`{"eventScope":"source-generation","batchId":"source-frozen-batch","events":[]}`)
	if err := c.persistOutboxLocked(); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(c.outboxSnapshotLocked())
	c.qmu.Unlock()
	result, err := c.switchStationMode(context.Background(), stationSwitchInput(source, target), true)
	if err == nil || !strings.Contains(err.Error(), "恢复原站连接") || result != nil {
		t.Fatalf("scoped queue silently transferred: result=%v err=%v", result, err)
	}
	after, _ := json.Marshal(c.outboxSnapshotLocked())
	if string(before) != string(after) || !reflect.DeepEqual(source, c.o.Store.Get()) || c.stationSwitching.Load() {
		t.Fatal("rejected transfer changed source config or frozen outbox")
	}
	path, found := images.find(ref.ID)
	if !found || !images.outboxPins[ref.ID] {
		t.Fatal("rejected transfer released its source image")
	}
	past := time.Now().Add(-2 * attachmentTTL)
	if err = os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}
	restarted := New(Options{Store: c.o.Store})
	if restarted.durableErr != nil || len(restarted.queue) != 1 || restarted.inFlightBatchID != "source-frozen-batch" || restarted.queueEventScope != "source-generation" {
		t.Fatal("rejected handover was not recoverable on restart", restarted.durableErr)
	}
	frozen := context.WithValue(context.Background(), eventScopeContextKey{}, restarted.queueEventScope)
	restoredImages := restarted.attachmentsForConfig(frozen, source)
	restoredImages.mu.Lock()
	restoredImages.sweepLocked()
	protected := restoredImages.outboxPins[ref.ID]
	restoredImages.mu.Unlock()
	if _, err = os.Stat(path); err != nil || !protected {
		t.Fatal("pending scoped image expired during restart", err)
	}
}

func TestScopedStationTransferRechecksLateProgressBeforeConfigCommit(t *testing.T) {
	c, source, _ := stationSwitchFixture(t, nil, nil)
	if err := c.acceptEventScope(source, "source-generation", true); err != nil {
		t.Fatal(err)
	}
	if err := c.beginStationSwitchMode(true); err != nil {
		t.Fatal(err)
	}
	defer c.endStationSwitch()
	// Models/target verification can take time while an existing Agent emits.
	c.Push(protocol.Event{Type: "message", SessionKey: workspaceTestKey, ID: "late", Text: "late progress"})
	committed := false
	err := c.commitStationSwitchMode(func(*config.Config) error { committed = true; return nil }, true)
	if err == nil || committed || !reflect.DeepEqual(source, c.o.Store.Get()) || len(c.queue) != 1 || c.queue[0].ID != "late" {
		t.Fatal("late scoped progress was lost during commit", err)
	}
}

func TestScopedStationTransferPreservesRecoveryWithoutQueuedText(t *testing.T) {
	for _, state := range []string{"session-gap", "frozen-batch", "frozen-recovery"} {
		t.Run(state, func(t *testing.T) {
			c, source, _ := stationSwitchFixture(t, nil, nil)
			if err := c.acceptEventScope(source, "source-generation", true); err != nil {
				t.Fatal(err)
			}
			switch state {
			case "session-gap":
				c.noteDroppedLocked([]protocol.Event{{SessionKey: workspaceTestKey, Tool: "codex"}})
			case "frozen-batch":
				c.inFlightBatchID = "recovery-only-batch"
			case "frozen-recovery":
				c.inFlightRecovery = map[string]recoveryMark{workspaceTestKey: {generation: 1}}
			}
			before, _ := json.Marshal(c.outboxSnapshotLocked())
			if err := c.beginStationSwitchMode(true); err == nil || c.stationSwitching.Load() {
				t.Fatal("recovery-only scoped transfer admitted", err)
			}
			committed := false
			if err := c.commitStationSwitchMode(func(*config.Config) error { committed = true; return nil }, true); err == nil || committed {
				t.Fatal("recovery-only scoped transfer committed", err)
			}
			after, _ := json.Marshal(c.outboxSnapshotLocked())
			if string(before) != string(after) {
				t.Fatal("rejected transfer changed recovery metadata")
			}
		})
	}
}

func TestScopedStationTransferAllowsGenericGapWithoutPrivateQueue(t *testing.T) {
	c, source, target := stationSwitchFixture(t, nil, nil)
	if err := c.acceptEventScope(source, "source-generation", true); err != nil {
		t.Fatal(err)
	}
	c.recoveryAll, c.recoveryOverflow = true, 1
	result, err := c.switchStationMode(context.Background(), stationSwitchInput(source, target), true)
	if err != nil || result["switched"] != true || c.o.Store.Get().DeviceID != target.DeviceID {
		t.Fatal("generic history-gap epoch permanently blocked handover", result, err)
	}
	if err := c.acceptEventScope(c.o.Store.Get(), "target-generation", true); err != nil {
		t.Fatal(err)
	}
	if len(c.queue) != 0 || len(c.recovery) != 0 || !c.recoveryAll {
		t.Fatal("target handshake failed to retain only an identity-free gap")
	}
}

func TestScopedStationSwitchWithEmptyQueueStillSucceeds(t *testing.T) {
	for _, transfer := range []bool{false, true} {
		name := "normal"
		if transfer {
			name = "standby"
		}
		t.Run(name, func(t *testing.T) {
			c, source, target := stationSwitchFixture(t, nil, nil)
			if err := c.acceptEventScope(source, "source-generation", true); err != nil {
				t.Fatal(err)
			}
			result, err := c.switchStationMode(context.Background(), stationSwitchInput(source, target), transfer)
			if err != nil || result["switched"] != true || c.o.Store.Get().DeviceID != target.DeviceID {
				t.Fatal("empty scoped queue prevented station switch", result, err)
			}
		})
	}
}
