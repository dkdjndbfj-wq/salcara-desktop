package hubclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func TestEventScopeRestartPreservesFrozenBatchAndRequiresHandshake(t *testing.T) {
	c, store := newTestClient(t, "https://hub.invalid", "key", nil)
	cfg := store.Get()
	if err := c.acceptEventScope(cfg, "generation-one", true); err != nil {
		t.Fatal(err)
	}
	imageID := budgetAttachmentID(202)
	c.Push(protocol.Event{Type: "message", SessionKey: workspaceTestKey, ID: "one", Text: "private payload", Attachments: []protocol.AttachmentRef{{ID: imageID, Mime: "image/png"}}})
	c.qmu.Lock()
	c.inFlight, c.inFlightBatchID = 1, config.NewUUID()
	c.inFlightPayload = json.RawMessage(`{"batchId":"frozen","eventScope":"generation-one","events":[{"text":"private payload"}]}`)
	if err := c.persistOutboxLocked(); err != nil {
		t.Fatal(err)
	}
	batch, payload := c.inFlightBatchID, string(c.inFlightPayload)
	c.qmu.Unlock()
	restarted := New(Options{Store: store})
	if restarted.durableErr != nil {
		t.Fatal(restarted.durableErr)
	}
	if !restarted.eventScopeRequired(cfg) || restarted.eventScopeFor(cfg) != "" {
		t.Fatal("disk state was treated as authenticated scope")
	}
	frozen := context.WithValue(context.Background(), eventScopeContextKey{}, "generation-one")
	if !restarted.attachmentsForConfig(frozen, cfg).outboxPins[imageID] {
		t.Fatal("restart did not pin the persisted generation image before handshake")
	}
	if _, err := restarted.requestFor(context.Background(), cfg, http.MethodPost, "/bridge/events", nil, true); err == nil {
		t.Fatal("uploaded before authenticated handshake")
	}
	if err := restarted.acceptEventScope(cfg, "generation-one", true); err != nil {
		t.Fatal(err)
	}
	if len(restarted.queue) != 1 || restarted.inFlight != 1 || restarted.inFlightBatchID != batch || string(restarted.inFlightPayload) != payload {
		t.Fatal("same-generation restart changed frozen batch")
	}
	// Identical heartbeat must not require another disk write.
	restarted.durable.dir = ""
	if err := restarted.acceptEventScope(cfg, "generation-one", true); err != nil {
		t.Fatal("unchanged heartbeat rewrote outbox", err)
	}
}

func TestEventScopeStaleRegistrationAndLedgerCannotUseNewGeneration(t *testing.T) {
	c, store := newTestClient(t, "https://hub.invalid", "key", nil)
	oldCfg := store.Get()
	if err := c.acceptEventScope(oldCfg, "old", true); err != nil {
		t.Fatal(err)
	}
	oldContext := withCommandGuard(context.Background(), commandGuard{identity: config.RemoteIdentity(oldCfg), deviceID: oldCfg.DeviceID, eventScope: "old"})
	if err := c.acceptEventScope(oldCfg, "new", true); err != nil {
		t.Fatal(err)
	}
	cmd := map[string]any{"type": "session.send", "sessionKey": workspaceTestKey, "text": "old", "operationId": config.NewUUID()}
	if _, _, _, err := c.beginDurableExecution(oldContext, oldCfg, cmd); err == nil {
		t.Fatal("old command claimed a new-generation receipt")
	}
	if err := store.Update(func(cfg *config.Config) error { cfg.HubURL = "https://replacement.invalid/salcara-hub"; return nil }); err != nil {
		t.Fatal(err)
	}
	newCfg := store.Get()
	if err := c.acceptEventScope(newCfg, "replacement", true); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"", "old"} {
		if err := c.acceptEventScope(oldCfg, scope, true); err == nil {
			t.Fatal("stale registration accepted")
		}
		if c.eventScopeFor(newCfg) != "replacement" || c.queueEventScope != "replacement" {
			t.Fatal("stale registration mutated new authority or queue")
		}
	}
}

func TestEventScopeRebindDropsPrivateQueueRecoveryAndImagePins(t *testing.T) {
	c, store := newTestClient(t, "https://hub.invalid", "key", nil)
	cfg := store.Get()
	if err := c.acceptEventScope(cfg, "old", true); err != nil {
		t.Fatal(err)
	}
	id := budgetAttachmentID(201)
	c.Push(protocol.Event{Type: "message", SessionKey: workspaceTestKey, ID: "old-message", Text: "old secret", Attachments: []protocol.AttachmentRef{{ID: id, Mime: "image/png"}}})
	old := c.attachmentsFor(context.Background())
	if !old.outboxPins[id] {
		t.Fatal("queued image not protected")
	}
	c.qmu.Lock()
	c.inFlight, c.inFlightBatchID = 1, "old-batch"
	c.inFlightPayload = json.RawMessage(`{"text":"old secret"}`)
	c.noteDroppedLocked([]protocol.Event{{SessionKey: "private-session", Tool: "codex"}})
	c.qmu.Unlock()
	if err := c.acceptEventScope(cfg, "new", true); err != nil {
		t.Fatal(err)
	}
	if len(c.queue) != 0 || c.inFlightBatchID != "" || len(c.recovery) != 0 || len(c.recoverySeen) != 0 || !c.recoveryAll {
		t.Fatal("old private recovery state survived rebind")
	}
	if old.outboxPins[id] || c.attachmentsFor(context.Background()) == old {
		t.Fatal("old image pin or namespace survived rebind")
	}
	state, _ := json.Marshal(c.outboxSnapshotLocked())
	if strings.Contains(string(state), "old secret") || strings.Contains(string(state), "private-session") {
		t.Fatal("old private data survived snapshot")
	}
	restarted := New(Options{Store: store})
	if err := restarted.acceptEventScope(cfg, "new", true); err != nil {
		t.Fatal(err)
	}
	if len(restarted.queue) != 0 || !restarted.recoveryAll {
		t.Fatal("rebind isolation did not survive restart")
	}
}

func TestEventScopeFreezesBlobHeaderAndFileNamespace(t *testing.T) {
	c, store := newTestClient(t, "https://hub.invalid", "key", nil)
	cfg := store.Get()
	if err := c.acceptEventScope(cfg, "old", true); err != nil {
		t.Fatal(err)
	}
	frozen := context.WithValue(context.Background(), eventScopeContextKey{}, "old")
	old := c.attachmentsForConfig(frozen, cfg)
	if err := c.acceptEventScope(cfg, "new", true); err != nil {
		t.Fatal(err)
	}
	req, err := c.requestFor(frozen, cfg, http.MethodGet, "/bridge/blobs?id=att_example", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("X-Salcara-Event-Scope") != "old" || c.attachmentsForConfig(frozen, cfg) != old || c.attachmentsFor(context.Background()) == old {
		t.Fatal("old upload was relabelled with new scope")
	}
}

func TestEventScopeRejectsCommandWaitingOnTaskMutex(t *testing.T) {
	c, store := newTestClient(t, "https://hub.invalid", "key", nil)
	cfg := store.Get()
	if err := c.acceptEventScope(cfg, "old", true); err != nil {
		t.Fatal(err)
	}
	a := &receiptReconnectAgent{fakeAgent: &fakeAgent{id: "codex"}, entered: make(chan struct{}), release: make(chan struct{})}
	close(a.release)
	c.SetManager(&fakeManager{list: []agents.Agent{a}})
	ctx := withCommandGuard(context.Background(), commandGuard{identity: config.RemoteIdentity(cfg), deviceID: cfg.DeviceID, eventScope: "old"})
	c.taskConfigMu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := c.Dispatch(ctx, map[string]any{"type": "session.send", "sessionKey": workspaceTestKey, "text": "stale task", "operationId": config.NewUUID(), "controlSurface": "cli"})
		done <- err
	}()
	if err := c.acceptEventScope(cfg, "new", true); err != nil {
		t.Fatal(err)
	}
	c.taskConfigMu.Unlock()
	select {
	case err := <-done:
		if err == nil || a.calls.Load() != 0 {
			t.Fatal("stale command reached Agent", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("command did not finish")
	}
}

func TestEventScopeRegistrationHonorsRequiredAndLegacyQR(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"eventScope":"server-generation","eventScopeRequired":true}`))
	}))
	defer server.Close()
	c, store := newTestClient(t, server.URL, "key", nil)
	if err := c.registerWithEventScope(context.Background(), store.Get(), "/bridge/register", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if !c.eventScopeRequired(store.Get()) || c.eventScopeFor(store.Get()) != "server-generation" {
		t.Fatal("server requirement was ignored")
	}
	if err := c.acceptEventScope(store.Get(), ""); err == nil {
		t.Fatal("required scope downgraded")
	}
	if _, err := c.requestFor(context.Background(), store.Get(), http.MethodPost, "/bridge/events", nil, true); err == nil {
		t.Fatal("missing scope still authorized")
	}
	qr, qrStore := newTestClient(t, "https://hub.invalid", "key", nil)
	qr.Push(protocol.Event{Type: "message", SessionKey: workspaceTestKey, Text: "QR queue"})
	if err := qr.acceptEventScope(qrStore.Get(), ""); err != nil {
		t.Fatal("legacy QR failed", err)
	}
	if err := qr.acceptEventScope(qrStore.Get(), "qr-generation", false); err != nil {
		t.Fatal(err)
	}
	if len(qr.queue) != 1 {
		t.Fatal("same-binding QR upgrade discarded queue")
	}
}
