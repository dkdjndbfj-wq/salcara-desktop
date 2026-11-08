package hubclient

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func TestMissingRecoveryKeyPreservesExistingStoreAndRejectsExecution(t *testing.T) {
	c, store := newTestClient(t, "https://hub.invalid", "key", nil)
	c.Push(protocol.Event{Type: "message", ID: "persisted", SessionKey: workspaceTestKey, Text: "retained result"})
	journal := filepath.Join(c.durable.dir, "events.journal")
	before, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(c.durable.dir, "payload.key")
	key, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	restarted := New(Options{Store: store})
	if restarted.Status().Recovery.State != "blocked" || restarted.durableErr == nil {
		t.Fatal("missing key was presented as healthy storage")
	}
	if _, err = os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("replacement key was created", err)
	}
	after, err := os.ReadFile(journal)
	if err != nil || string(before) != string(after) {
		t.Fatal("retained journal was modified", err)
	}
	_, _, _, err = restarted.beginDurableExecution(durableTestContext(restarted), store.Get(), map[string]any{"type": "session.send", "operationId": config.NewUUID()})
	if err == nil {
		t.Fatal("execution admitted with unavailable recovery storage")
	}
	for _, capability := range restarted.Device(context.Background()).Capabilities {
		if strings.HasPrefix(capability, "durable.") {
			t.Fatal("advertised unusable storage", capability)
		}
	}
	if err = os.WriteFile(keyPath, key, 0600); err != nil {
		t.Fatal(err)
	}
	restored := New(Options{Store: store})
	if restored.Status().Recovery.State != "ready" || len(restored.queue) != 1 {
		t.Fatal("original key did not restore queued messages", restored.Status())
	}
}

func TestRecoveryStatusShowsWriteFailureUntilCompleteSnapshotSucceeds(t *testing.T) {
	c, _ := newTestClient(t, "https://hub.invalid", "key", nil)
	if c.Status().Recovery.State != "ready" {
		t.Fatal("new store not ready")
	}
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	originalDir := c.durable.dir
	c.durable.dir = blocked
	c.Push(protocol.Event{Type: "message", ID: "failed-save", SessionKey: workspaceTestKey, Text: "memory result"})
	status := c.Status()
	if status.Recovery.State != "degraded" || status.Queued != 1 || status.Recovery.QueuedBytes == 0 || !status.Recovery.HistoryRequired {
		t.Fatal("storage failure or recovery marker hidden", status)
	}
	c.durable.dir = originalDir
	c.qmu.Lock()
	err := c.persistOutboxLocked()
	c.qmu.Unlock()
	if err != nil || c.Status().Recovery.State != "ready" {
		t.Fatal("snapshot did not recover health", err)
	}
	if !c.Status().Recovery.HistoryRequired {
		t.Fatal("storage repair hid the missing-history marker")
	}
}
