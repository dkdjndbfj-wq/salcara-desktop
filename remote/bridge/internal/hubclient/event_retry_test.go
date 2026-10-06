package hubclient

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func TestEventRetryKeepsExactBatchWhileNewProgressAndRecoveryArrive(t *testing.T) {
	type upload struct {
		BatchID string          `json:"batchId"`
		Events  json.RawMessage `json:"events"`
	}
	requests := make(chan upload, 8)
	var count atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body upload
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests <- body
		// Simulate a Hub that accepted the first request but lost its reply.
		if count.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	c := economyClient(t)
	c.o.FlushInterval = 10 * time.Millisecond
	if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.HubURL = server.URL; return nil }); err != nil {
		t.Fatal(err)
	}
	c.setState(StateConnected, "")
	c.Push(snapshot("first", false))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); c.flushLoop(ctx) }()
	next := func() upload {
		t.Helper()
		select {
		case body := <-requests:
			return body
		case <-time.After(3 * time.Second):
			t.Fatal("event upload did not arrive")
			return upload{}
		}
	}
	first := next()
	// Both ordinary progress and gap notices must stay out of the attempted
	// batch. Otherwise a retry conflicts with the Hub's accepted digest.
	c.Push(snapshot("latest", true))
	c.Push(protocol.Event{Type: "notice", SessionKey: "codex:lost", Text: strings.Repeat("x", maxEventWeight)})
	second := next()
	if first.BatchID == "" || second.BatchID != first.BatchID || !bytes.Equal(first.Events, second.Events) {
		t.Fatalf("retry changed batch identity/content: first=%s %s second=%s %s", first.BatchID, first.Events, second.BatchID, second.Events)
	}
	third := next()
	if third.BatchID == first.BatchID || !bytes.Contains(third.Events, []byte("latest")) || !bytes.Contains(third.Events, []byte(historyGapPrefix)) {
		t.Fatalf("new progress/recovery not isolated in next batch: %s", third.Events)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("flush loop did not stop")
	}
}

func TestOrdinaryReconnectRetainsQueueUntilFreshPairStatus(t *testing.T) {
	var uploads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploads.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	c := economyClient(t)
	c.o.FlushInterval = 10 * time.Millisecond
	if err := c.o.Store.Update(func(cfg *config.Config) error {
		cfg.HubURL = server.URL
		cfg.RemoteDeviceOnly = true
		cfg.PhoneBindingID, cfg.PhoneHash = strings.Repeat("a", 64), strings.Repeat("b", 64)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cfg := c.o.Store.Get()
	c.setState(StateConnected, "")
	c.publishPair(PairStatus{DeviceID: cfg.DeviceID, Paired: true}, eventIdentity(cfg))
	c.Push(protocol.Event{Type: "notice", ID: "before", SessionKey: "codex:original", Text: "before reconnect"})
	c.Kick()
	c.Push(protocol.Event{Type: "notice", ID: "during", SessionKey: "codex:original", Text: "during reconnect"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); c.flushLoop(ctx) }()
	time.Sleep(80 * time.Millisecond)
	c.qmu.Lock()
	retained := len(c.queue)
	c.qmu.Unlock()
	if retained != 2 || uploads.Load() != 0 {
		t.Fatalf("unknown authorization discarded/sent progress: retained=%d uploads=%d", retained, uploads.Load())
	}
	c.publishPair(PairStatus{DeviceID: cfg.DeviceID, Paired: true}, eventIdentity(cfg))
	waitFor(t, "retained reconnect progress", func() bool { return uploads.Load() == 1 && c.Status().Queued == 0 })
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("flush loop did not stop")
	}
}

func TestLateUploadAcknowledgementCannotClearNewIdentityBatch(t *testing.T) {
	type upload struct {
		BatchID string          `json:"batchId"`
		Events  json.RawMessage `json:"events"`
	}
	requests := make(chan upload, 4)
	release := make(chan struct{})
	var count atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body upload
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests <- body
		if count.Add(1) == 1 {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	c := economyClient(t)
	c.o.FlushInterval = 10 * time.Millisecond
	if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.HubURL = server.URL; return nil }); err != nil {
		t.Fatal(err)
	}
	c.setState(StateConnected, "")
	c.Push(protocol.Event{Type: "notice", ID: "old", SessionKey: "codex:original", Text: "old identity"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); c.flushLoop(ctx) }()
	next := func() upload {
		t.Helper()
		select {
		case body := <-requests:
			return body
		case <-time.After(3 * time.Second):
			t.Fatal("event upload did not arrive")
			return upload{}
		}
	}
	first := next()
	if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.DeviceSecret = "replacement-fixture-secret"; return nil }); err != nil {
		t.Fatal(err)
	}
	c.Push(protocol.Event{Type: "notice", ID: "new", SessionKey: "codex:original", Text: "new identity"})
	close(release)
	second := next()
	if first.BatchID == second.BatchID || !bytes.Contains(second.Events, []byte("new identity")) || bytes.Contains(second.Events, []byte("old identity")) {
		t.Fatalf("late acknowledgement cleared or mixed replacement batch: %s", second.Events)
	}
	waitFor(t, "replacement queue acknowledged", func() bool { return c.Status().Queued == 0 })
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("flush loop did not stop")
	}
}
