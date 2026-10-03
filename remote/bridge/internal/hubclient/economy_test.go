package hubclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
	"sync"
	"testing"
	"time"
)

func TestEconomyInFlightAckKeepsNewFinalSnapshot(t *testing.T) {
	batches := make(chan []protocol.Event, 3)
	firstAck := make(chan struct{})
	var acknowledge sync.Once
	defer acknowledge.Do(func() { close(firstAck) })
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Events []protocol.Event `json:"events"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		requests++
		batches <- body.Events
		if requests == 1 {
			<-firstAck
		}
		w.WriteHeader(200)
	}))
	defer server.Close()
	c := economyClient(t)
	c.o.FlushInterval = 20 * time.Millisecond
	if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.HubURL = server.URL; return nil }); err != nil {
		t.Fatal(err)
	}
	c.setState(StateConnected, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.flushLoop(ctx)
	c.Push(snapshot("a", false))
	var first []protocol.Event
	select {
	case first = <-batches:
	case <-time.After(time.Second):
		t.Fatal("first upload missing")
	}
	c.Push(snapshot("ab", false))
	c.Push(snapshot("abc", true))
	acknowledge.Do(func() { close(firstAck) })
	var second []protocol.Event
	select {
	case second = <-batches:
	case <-time.After(time.Second):
		t.Fatal("final upload missing")
	}
	if len(first) != 1 || first[0].Text != "a" || len(second) != 1 || second[0].Text != "abc" || !second[0].Final {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
}

func economyClient(t *testing.T) *Client {
	t.Helper()
	store, err := config.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	return New(Options{Store: store, FlushBatch: 1000})
}
func snapshot(text string, final bool) protocol.Event {
	return protocol.Event{Type: "message", SessionKey: "codex:test", Tool: "codex", ID: "answer", Role: "assistant", Text: text, Final: final}
}
func TestEconomyDefaultFlushInterval(t *testing.T) {
	c := economyClient(t)
	if c.o.FlushInterval != time.Second {
		t.Fatal(c.o.FlushInterval)
	}
}
func TestEconomyCoalescesCumulativeSnapshots(t *testing.T) {
	c := economyClient(t)
	c.Push(snapshot("a", false))
	c.Push(snapshot("ab", false))
	c.Push(snapshot("abc", true))
	if len(c.queue) != 1 || c.queue[0].Text != "abc" || !c.queue[0].Final {
		t.Fatalf("queue: %+v", c.queue)
	}
	c.Push(snapshot("later", false))
	if len(c.queue) != 2 || !c.queue[0].Final {
		t.Fatal("final snapshot overwritten")
	}
	reasoning := protocol.Event{Type: "reasoning", SessionKey: "codex:test", Tool: "codex", ID: "summary", Text: "one"}
	c.Push(reasoning)
	reasoning.Text = "two"
	c.Push(reasoning)
	if len(c.queue) != 3 || c.queue[2].Text != "two" {
		t.Fatal("reasoning not coalesced")
	}
}
func TestEconomyPreservesBoundaryAndScope(t *testing.T) {
	c := economyClient(t)
	c.Push(snapshot("before", false))
	c.Push(protocol.Event{Type: "approval.request", SessionKey: "codex:test", ApprovalID: "approve"})
	c.Push(snapshot("after", false))
	c.Push(protocol.Event{Type: "turn", SessionKey: "codex:test", Status: "completed"})
	c.Push(snapshot("next turn", false))
	another := snapshot("other session", false)
	another.SessionKey = "codex:other"
	c.Push(another)
	if len(c.queue) != 6 || c.queue[0].Text != "before" || c.queue[1].Type != "approval.request" || c.queue[3].Type != "turn" {
		t.Fatalf("boundary/scope changed: %+v", c.queue)
	}
}
func TestEconomyNeverMutatesInFlightPrefix(t *testing.T) {
	c := economyClient(t)
	c.Push(snapshot("a", false))
	c.inFlight = 1
	c.Push(snapshot("ab", false))
	c.Push(snapshot("abc", false))
	if len(c.queue) != 2 || c.queue[0].Text != "a" || c.queue[1].Text != "abc" {
		t.Fatalf("in-flight changed: %+v", c.queue)
	}
	c.inFlight = 0
	c.Push(snapshot("abcd", true))
	if len(c.queue) != 2 || c.queue[0].Text != "a" || !c.queue[1].Final {
		t.Fatal("latest unsent final lost")
	}
}
func TestEconomyStationChangeClearsOldSnapshots(t *testing.T) {
	c := economyClient(t)
	if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.HubURL = "https://one.example/salcara-hub"; return nil }); err != nil {
		t.Fatal(err)
	}
	c.Push(snapshot("old", false))
	c.inFlight = 1
	if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.HubURL = "https://two.example/salcara-hub"; return nil }); err != nil {
		t.Fatal(err)
	}
	c.Push(snapshot("new", false))
	if len(c.queue) != 1 || c.inFlight != 0 || c.queue[0].Text != "new" {
		t.Fatal("station data crossed")
	}
}
