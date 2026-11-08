package agents

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"
	"time"

	"salcara/bridge/internal/protocol"
)

func TestCodexFeaturesChangedAcceptsOnlyOwnedRuntime(t *testing.T) {
	for _, state := range []string{"current", "starting", "cancelled-start", "replaced", "closed"} {
		t.Run(state, func(t *testing.T) {
			a, f, rec := newCodexFeatureFixture(t)
			c := a.conn
			a.installCodexNotificationHandler(c)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			t.Cleanup(func() { a.mu.Lock(); a.starting = nil; a.closed = false; a.mu.Unlock() })
			switch state {
			case "starting", "cancelled-start":
				a.conn = nil
				a.starting = &codexStartupLease{ctx: ctx, conn: c}
				if state == "cancelled-start" {
					cancel()
				}
			case "replaced":
				a.conn = &rpcConn{}
			case "closed":
				a.closed = true
			}
			completed := make(chan struct{})
			go func() {
				// Extra fields cannot invent a session, path or message payload.
				c.handleLine([]byte(`{"method":"skills/changed","params":{"threadId":"foreign","path":"untrusted","text":"hidden"}}`))
				close(completed)
			}()
			select {
			case <-completed:
			case <-time.After(time.Second):
				t.Fatal("notification blocked the RPC reader")
			}
			want := 0
			if state == "current" || state == "starting" {
				want = 1
			}
			events := rec.all()
			if len(events) != want || len(f.requests) != 0 {
				t.Fatalf("events=%+v requests=%v", events, f.requests)
			}
			if want == 1 && !reflect.DeepEqual(events[0], protocol.Event{Type: "features.changed", Tool: "codex", TS: events[0].TS}) {
				t.Fatalf("notification carried a non-control payload: %+v", events[0])
			}
		})
	}
}

func TestCodexFeaturesChangedLeavesRPCResponseAndOtherNotificationsUsable(t *testing.T) {
	a, f, rec := newCodexFeatureFixture(t)
	c := a.conn
	a.installCodexNotificationHandler(c)
	f.skillReadHook = func(int) {
		c.handleLine([]byte(`{"method":"skills/changed","params":{}}`))
	}
	if _, _, err := readCodexFeatures(context.Background(), c, f.cwd); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(map[string]any{"method": "thread/started", "params": map[string]any{"thread": f.base.thread}})
	c.handleLine(encoded)
	events := rec.all()
	if len(events) != 2 || events[0].Type != "features.changed" || events[1].Type != "session.updated" {
		t.Fatalf("lost notifications or response: %+v", events)
	}
}

func TestRPCNotificationHandlerReplacementIsSynchronized(t *testing.T) {
	c := &rpcConn{}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 1000 {
			c.setNotificationHandler(func(string, json.RawMessage) {
				// The callback is outside c.mu and may update a handler.
				c.setRequestHandler(nil)
			})
		}
	}()
	go func() {
		defer wg.Done()
		for range 1000 {
			c.handleLine([]byte(`{"method":"skills/changed","params":{}}`))
		}
	}()
	wg.Wait()
}

func TestManagerFeaturesChangedProjectsOnlyRegisteredControl(t *testing.T) {
	a := declaredRegistryAgent()
	a.descriptor.Capabilities.Features = true
	var events []protocol.Event
	m := NewManagerWithOptions(func(ev protocol.Event) { events = append(events, ev) }, func() Settings { return Settings{} }, registryOptions(t, a)).(*manager)
	defer m.Close()
	m.publishAgentEvent(a, a.ID(), protocol.Event{Type: "features.changed", Text: "private", Cwd: "private", ApprovalID: "approval", ImageData: []string{"private"}, TS: 42})
	if !reflect.DeepEqual(events, []protocol.Event{{Type: "features.changed", Tool: a.ID(), TS: 42}}) {
		t.Fatalf("control contains unrelated fields: %+v", events)
	}
	for _, ev := range []protocol.Event{
		{Type: "features.changed", Tool: "codex"},
		{Type: "features.changed", SessionKey: "hermes:s"},
		{Type: "features.changed", Session: &protocol.SessionInfo{}},
		{Type: "message", Text: "no session"},
	} {
		m.publishAgentEvent(a, a.ID(), ev)
	}
	a.descriptor.Capabilities.Features = false
	m.publishAgentEvent(a, a.ID(), protocol.Event{Type: "features.changed"})
	a.descriptor.Capabilities.Features = true
	a.descriptor.ControlSurface = "desktop"
	m.publishAgentEvent(a, a.ID(), protocol.Event{Type: "features.changed"})
	if len(events) != 1 {
		t.Fatalf("invalid control was admitted: %+v", events)
	}
}
