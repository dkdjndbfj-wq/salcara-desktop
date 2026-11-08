package hubclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func durableTestContext(c *Client) context.Context {
	cfg := c.o.Store.Get()
	return withCommandGuard(context.Background(), commandGuard{identity: config.RemoteIdentity(cfg), deviceID: cfg.DeviceID, bindingID: cfg.PhoneBindingID, phoneHash: cfg.PhoneHash})
}

func TestDurableDispatchRestartReplaysReceiptWithoutExecuting(t *testing.T) {
	c, store := newTestClient(t, "https://hub.invalid", "key", nil)
	a := &receiptReconnectAgent{fakeAgent: &fakeAgent{id: "codex"}, entered: make(chan struct{}), release: make(chan struct{})}
	close(a.release)
	c.SetManager(&fakeManager{list: []agents.Agent{a}})
	cmd := map[string]any{"type": "session.send", "sessionKey": workspaceTestKey, "text": "private durable prompt", "operationId": config.NewUUID(), "controlSurface": "cli"}
	result, err := c.Dispatch(durableTestContext(c), cmd)
	if err != nil {
		t.Fatal(err)
	}
	restarted := New(Options{Store: store})
	restarted.SetManager(c.manager())
	second, err := restarted.Dispatch(durableTestContext(restarted), cmd)
	if err != nil {
		t.Fatal(err)
	}
	one, _ := json.Marshal(result)
	two, _ := json.Marshal(second)
	if !bytes.Equal(one, two) || a.calls.Load() != 1 {
		t.Fatalf("duplicate executed: %s / %s calls %d", one, two, a.calls.Load())
	}
	cmd["text"] = "different payload"
	if _, err = restarted.Dispatch(durableTestContext(restarted), cmd); err == nil || a.calls.Load() != 1 {
		t.Fatal("conflicting operation ran again")
	}
	bytesOnDisk, err := os.ReadFile(filepath.Join(c.durable.dir, "operations.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bytesOnDisk, []byte("private durable prompt")) {
		t.Fatal("private command persisted unencrypted")
	}
}

func TestDurableClaimCrashSourceChangeAndQuotaFailure(t *testing.T) {
	d, err := openDurableStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if replay, _, err := d.claim("binding", "op", "digest", "source"); err != nil || replay {
		t.Fatal(replay, err)
	}
	d2, err := openDurableStore(d.dir)
	if err != nil {
		t.Fatal(err)
	}
	if replay, _, err := d2.claim("binding", "op", "digest", "source"); !replay || err == nil {
		t.Fatal("crashed execution was re-admitted")
	}
	if err = d.finish("binding", "op", map[string]any{"secret": "private receipt"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err = d2.claim("binding", "op", "digest", "changed source"); err == nil {
		t.Fatal("source change exposed old receipt")
	}
	if replay, _, err := d2.claim("new-binding", "op", "digest", "source"); err != nil || replay {
		t.Fatal("new binding collided with old private scope")
	}
	bad := filepath.Join(t.TempDir(), "not-a-directory")
	if err = os.WriteFile(bad, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	d2.dir = bad
	if _, _, err = d2.claim("binding", "another", "d", "s"); err == nil {
		t.Fatal("disk error admitted execution")
	}
}

func TestDurableOutboxRestartsWithExactFrozenBatchAndJournal(t *testing.T) {
	c, store := newTestClient(t, "https://hub.invalid", "key", nil)
	c.Push(protocol.Event{Type: "message", SessionKey: workspaceTestKey, ID: "one", Text: "private queue"})
	c.qmu.Lock()
	c.inFlight = 1
	c.inFlightBatchID = config.NewUUID()
	c.inFlightPayload = json.RawMessage(`{"batchId":"frozen","events":[{"text":"immutable"}]}`)
	if err := c.persistOutboxLocked(); err != nil {
		t.Fatal(err)
	}
	batch := c.inFlightBatchID
	payload := string(c.inFlightPayload)
	c.qmu.Unlock()
	c.Push(protocol.Event{Type: "message", SessionKey: workspaceTestKey, ID: "two", Text: "journal tail"})
	restarted := New(Options{Store: store})
	if restarted.durableErr != nil {
		t.Fatal(restarted.durableErr)
	}
	if len(restarted.queue) != 2 || restarted.inFlight != 1 || restarted.inFlightBatchID != batch || string(restarted.inFlightPayload) != payload {
		t.Fatalf("lost frozen batch or tail: %+v", restarted.outboxSnapshotLocked())
	}
	for _, name := range []string{"outbox.enc", "events.journal"} {
		b, err := os.ReadFile(filepath.Join(c.durable.dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte("private queue")) || bytes.Contains(b, []byte("journal tail")) {
			t.Fatal("plaintext event on disk")
		}
	}
	if err := store.Update(func(cfg *config.Config) error {
		cfg.PhoneBindingID = "replacement"
		cfg.PhoneHash = strings.Repeat("e", 64)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	other := New(Options{Store: store})
	if len(other.queue) != 0 || other.inFlightBatchID != "" {
		t.Fatal("old binding outbox leaked")
	}
}

func TestDurableAttachmentChunksRestartConflictAndLease(t *testing.T) {
	s := testAttachmentStore(t)
	id := budgetAttachmentID(77)
	raw := budgetImage("durable-payload")
	encoded := base64.StdEncoding.EncodeToString(raw)
	if done, err := s.put(id, "image/png", 0, 2, encoded[:8]); err != nil || done {
		t.Fatal(done, err)
	}
	if len(s.pending[id].chunks) != 0 {
		t.Fatal("image chunks retained in memory")
	}
	restarted := testAttachmentStore(t)
	restarted.dir = s.dir
	restarted.bindingsFile = s.bindingsFile
	if _, err := restarted.put(id, "image/png", 0, 2, "AAAAAAAA"); err == nil {
		t.Fatal("conflicting duplicate accepted after restart")
	}
	if done, err := restarted.put(id, "image/png", 1, 2, encoded[8:]); err != nil || !done {
		t.Fatal(done, err)
	}
	if err := restarted.bind([]string{id}, "codex:one"); err != nil {
		t.Fatal(err)
	}
	if err := restarted.bind([]string{id}, "codex:two"); err != nil {
		t.Fatal(err)
	}
	release, err := restarted.lease([]string{id})
	if err != nil {
		t.Fatal(err)
	}
	path, _ := restarted.find(id)
	past := time.Now().Add(-2 * attachmentTTL)
	if err = os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}
	restarted.lastSweep = time.Time{}
	restarted.sweepLocked()
	if _, err := restarted.read(id, "codex:two", 0, 90); err != nil {
		t.Fatal("active reference removed", err)
	}
	release()
	restarted.lastSweep = time.Time{}
	restarted.sweepLocked()
	if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("expired image not reclaimed after final lease")
	}
}

func TestDurableHubBlobTransferUsesRawBytesAndBoundIdentity(t *testing.T) {
	image := budgetImage("blob fixture")
	id := budgetAttachmentID(99)
	var uploaded atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Salcara-Binding-Id") != "binding" || r.Header.Get("X-Salcara-Phone-Hash") != strings.Repeat("a", 64) {
			t.Error("blob identity missing")
		}
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "image/png")
			w.Write(image)
			return
		}
		b, _ := io.ReadAll(io.LimitReader(r.Body, 100))
		if !bytes.Equal(b, image) || r.URL.Query().Get("attachmentId") != id {
			t.Error("blob body is not original image")
		}
		uploaded.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"id": id})
	}))
	defer server.Close()
	c, store := newTestClient(t, server.URL, "key", nil)
	if err := store.Update(func(cfg *config.Config) error {
		cfg.PhoneBindingID = "binding"
		cfg.PhoneHash = strings.Repeat("a", 64)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s := c.attachmentsFor(context.Background())
	cmd := map[string]any{"attachments": []any{id}}
	if err := c.ensureCommandAttachments(context.Background(), store.Get(), s, cmd); err != nil {
		t.Fatal(err)
	}
	refs := s.refs([]string{id})
	if len(refs) != 1 {
		t.Fatal("download not published")
	}
	if err := c.uploadBatchAttachments(context.Background(), store.Get(), []protocol.Event{{Attachments: refs}}); err != nil {
		t.Fatal(err)
	}
	if uploaded.Load() != 1 {
		t.Fatal("event image not uploaded")
	}
}

func TestDurableOutboxLostAckRestartsWithIdenticalHTTPBatch(t *testing.T) {
	requests := make(chan []byte, 4)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 2<<20))
		requests <- b
		if calls.Add(1) == 1 {
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	c, store := newTestClient(t, server.URL, "key", nil)
	c.mu.Lock()
	c.status.State = StateConnected
	c.mu.Unlock()
	c.Push(protocol.Event{Type: "message", SessionKey: workspaceTestKey, ID: "restart-http", Text: "exact payload"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.flushLoop(ctx); close(done) }()
	var first []byte
	select {
	case first = <-requests:
	case <-time.After(3 * time.Second):
		t.Fatal("first batch missing")
	}
	cancel()
	<-done
	restarted := New(Options{Store: store, FlushInterval: 5 * time.Millisecond})
	restarted.mu.Lock()
	restarted.status.State = StateConnected
	restarted.mu.Unlock()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	done2 := make(chan struct{})
	go func() { restarted.flushLoop(ctx2); close(done2) }()
	var second []byte
	select {
	case second = <-requests:
	case <-time.After(3 * time.Second):
		t.Fatal("restored batch missing")
	}
	cancel2()
	<-done2
	if !bytes.Equal(first, second) {
		t.Fatalf("restart changed frozen batch:\n%s\n%s", first, second)
	}
}

func TestDurableDispatchStorageFailureNeverReachesAgent(t *testing.T) {
	c, _ := newTestClient(t, "https://hub.invalid", "key", nil)
	a := &receiptReconnectAgent{fakeAgent: &fakeAgent{id: "codex"}, entered: make(chan struct{}), release: make(chan struct{})}
	close(a.release)
	c.SetManager(&fakeManager{list: []agents.Agent{a}})
	bad := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(bad, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	c.durable.dir = bad
	_, err := c.Dispatch(durableTestContext(c), map[string]any{"type": "session.send", "sessionKey": workspaceTestKey, "text": "must not execute", "operationId": config.NewUUID(), "controlSurface": "cli"})
	if err == nil || a.calls.Load() != 0 {
		t.Fatal("storage failure dispatched a command")
	}
}

func TestDurableAttachmentQuotaSpansPhoneGenerations(t *testing.T) {
	root := t.TempDir()
	one := testAttachmentStore(t)
	two := testAttachmentStore(t)
	one.dir = filepath.Join(root, "one")
	two.dir = filepath.Join(root, "two")
	one.budgetDir = root
	two.budgetDir = root
	one.limits.cacheFiles = 1
	two.limits.cacheFiles = 1
	if _, ok := one.importBytes("codex:one", "image/png", budgetImage("one")); !ok {
		t.Fatal("first image rejected")
	}
	if _, ok := two.importBytes("codex:two", "image/png", budgetImage("two")); ok {
		t.Fatal("phone generation multiplied disk quota")
	}
}
