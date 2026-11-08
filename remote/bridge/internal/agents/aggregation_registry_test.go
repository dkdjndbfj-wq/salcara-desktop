package agents

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/protocol"
)

type registryDeclaredAgent struct {
	registryFixtureAgent
	id         string
	descriptor protocol.AgentDescriptor
}

func (a *registryDeclaredAgent) ID() string                           { return a.id }
func (a *registryDeclaredAgent) Descriptor() protocol.AgentDescriptor { return a.descriptor }

func registryOptions(t *testing.T, extras ...Agent) Options {
	t.Helper()
	return Options{StateDir: t.TempDir(), ClaudeHome: filepath.Join(t.TempDir(), ".claude"), ExtraAgents: extras}
}
func declaredRegistryAgent() *registryDeclaredAgent {
	d := registryFixtureAgent{}.Descriptor()
	return &registryDeclaredAgent{id: "hermes", descriptor: d}
}
func TestAggregationRegistryRejectsAliasedNamespacesAndWrongSurfaces(t *testing.T) {
	for _, mutation := range []string{"colon", "path", "descriptor-id", "descriptor-tool", "desktop", "read-only", "reserved"} {
		t.Run(mutation, func(t *testing.T) {
			a := declaredRegistryAgent()
			switch mutation {
			case "colon":
				a.id = "hermes:codex"
			case "path":
				a.id = "hermes/agent"
			case "descriptor-id":
				a.descriptor.ID = "codex"
			case "descriptor-tool":
				a.descriptor.Tool = "codex"
			case "reserved":
				a.id = "codex-desktop"
				a.descriptor.ID, a.descriptor.Tool = a.id, a.id
			default:
				a.descriptor.ControlSurface = mutation
			}
			m := NewManagerWithOptions(nil, func() Settings { return Settings{} }, registryOptions(t, a))
			defer m.Close()
			if len(m.Agents()) != 2 || m.Get(a.ID()) != nil {
				t.Fatal("invalid extension registered")
			}
		})
	}
	a := declaredRegistryAgent()
	a.descriptor.APIFamily = "codex"
	m := NewManagerWithOptions(nil, func() Settings { return Settings{} }, registryOptions(t, a))
	defer m.Close()
	if m.Get("hermes") != a || len(m.Agents()) != 3 {
		t.Fatal("explicit shared API family rejected")
	}
}

type registryBoundAgent struct {
	*registryDeclaredAgent
	mu           sync.Mutex
	sink         Sink
	watchStarted chan struct{}
	closed       atomic.Bool
}

func (a *registryBoundAgent) BindEventSink(sink Sink)   { a.mu.Lock(); a.sink = sink; a.mu.Unlock() }
func (a *registryBoundAgent) Watch(ctx context.Context) { close(a.watchStarted); <-ctx.Done() }
func (a *registryBoundAgent) Close()                    { a.closed.Store(true) }
func (a *registryBoundAgent) currentSink() Sink         { a.mu.Lock(); defer a.mu.Unlock(); return a.sink }

func TestAggregationBoundLiveEventsPreserveProvenanceAndDetachOnClose(t *testing.T) {
	a := &registryBoundAgent{registryDeclaredAgent: declaredRegistryAgent(), watchStarted: make(chan struct{})}
	a.descriptor.Capabilities.ImageOutput = true
	var mu sync.Mutex
	var received []protocol.Event
	m := NewManagerWithOptions(func(e protocol.Event) { mu.Lock(); received = append(received, e); mu.Unlock() }, func() Settings { return Settings{} }, registryOptions(t, a))
	sink := a.currentSink()
	if sink == nil {
		t.Fatal("live sink not bound")
	}
	wrong := []protocol.Event{
		{Type: "message", SessionKey: "codex:s"},
		{Type: "message", SessionKey: "hermes:s", Tool: "codex"},
		{Type: "session.updated", SessionKey: "hermes:s", Session: &protocol.SessionInfo{SessionKey: "hermes:s", Tool: "codex"}},
		{Type: "session.updated", SessionKey: "hermes:s", Session: &protocol.SessionInfo{SessionKey: "hermes:other"}},
	}
	for _, e := range wrong {
		sink(e)
	}
	info := protocol.SessionInfo{SessionKey: "hermes:s", Client: "Hermes", ControlSurface: "desktop", ControlExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	min := float64(1)
	e := protocol.Event{Type: "session.updated", Session: &info, ImageData: []string{"synthetic-image"}, Questions: []protocol.Question{{ID: "q", Options: []protocol.QuestionOption{{Label: "original"}}, Min: &min}}}
	sink(e)
	info.Title = "changed"
	e.ImageData[0] = "changed"
	e.Questions[0].Options[0].Label = "changed"
	min = 2
	mu.Lock()
	if len(received) != 1 {
		t.Fatalf("received foreign events: %d", len(received))
	}
	got := received[0]
	mu.Unlock()
	if got.Tool != "hermes" || got.SessionKey != "hermes:s" || got.Session.ControlSurface != "cli" || got.Session.ControlExpiresAt != 0 || got.Session.Title != "" || got.ImageData[0] != "synthetic-image" || got.Questions[0].Options[0].Label != "original" || *got.Questions[0].Min != 1 {
		t.Fatalf("event provenance or cloning failed: %+v", got)
	}
	m.Watch(context.Background())
	select {
	case <-a.watchStarted:
	case <-time.After(time.Second):
		t.Fatal("bound watcher not started")
	}
	m.Close()
	if a.currentSink() != nil || !a.closed.Load() {
		t.Fatal("live adapter not detached/closed")
	}
	sink(protocol.Event{Type: "message", SessionKey: "hermes:s", Text: "late"})
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatal("old sink survived close")
	}
}

func TestAggregationBrowserAndComputerMetadataDoNotAliasSource(t *testing.T) {
	a := &registryBoundAgent{registryDeclaredAgent: declaredRegistryAgent(), watchStarted: make(chan struct{})}
	var received []protocol.Event
	m := NewManagerWithOptions(func(e protocol.Event) { received = append(received, e) }, func() Settings { return Settings{} }, registryOptions(t, a))
	defer m.Close()
	browser := protocol.BrowserNavigation{Action: "navigate", URL: "http://localhost:3000/original", PageID: "original-page"}
	computer := protocol.ComputerActivity{Version: 1, Surface: "browser", Action: "navigate", Provider: "playwright"}
	a.currentSink()(protocol.Event{Type: "tool", SessionKey: "hermes:s", Browser: &browser, Computer: &computer})
	browser.Action, browser.URL, browser.PageID = "open", "https://changed.invalid", "changed-page"
	computer.Version, computer.Surface, computer.Action, computer.Provider = 2, "computer", "click", "changed-provider"
	if len(received) != 1 || received[0].Browser == &browser || received[0].Computer == &computer {
		t.Fatal("metadata was not copied before entering the sink")
	}
	if got := received[0].Browser; got == nil || *got != (protocol.BrowserNavigation{Action: "navigate", URL: "http://localhost:3000/original", PageID: "original-page"}) {
		t.Fatalf("source mutation changed browser navigation: %+v", got)
	}
	if got := received[0].Computer; got == nil || *got != (protocol.ComputerActivity{Version: 1, Surface: "browser", Action: "navigate", Provider: "playwright"}) {
		t.Fatalf("source mutation changed computer activity: %+v", got)
	}
}

type registryUnboundWatcher struct {
	registryFixtureAgent
	calls atomic.Int32
}

func (a *registryUnboundWatcher) Watch(context.Context) { a.calls.Add(1) }
func TestAggregationUnboundWatcherDoesNotInventLiveTransport(t *testing.T) {
	a := &registryUnboundWatcher{}
	m := NewManagerWithOptions(nil, func() Settings { return Settings{} }, registryOptions(t, a))
	defer m.Close()
	m.Watch(context.Background())
	if a.calls.Load() != 0 {
		t.Fatal("raw extension watcher was started")
	}
}

func TestAggregationSessionIdentityRejectsForeignToolAndControlsWithoutVendorGuessing(t *testing.T) {
	a := declaredRegistryAgent()
	for _, key := range []string{"codex:s", "hermes:", "hermes:\x00", "hermes:\n", "hermes:\u202e", "hermes:" + strings.Repeat("x", 513)} {
		if SessionBelongsTo(a, key, protocol.SessionInfo{SessionKey: key}) {
			t.Fatalf("unsafe identity accepted: %q", key)
		}
	}
	if !SessionBelongsTo(a, "hermes:vendor:opaque/identifier", protocol.SessionInfo{SessionKey: "hermes:vendor:opaque/identifier"}) {
		t.Fatal("invented a vendor raw ID grammar")
	}
	if SessionBelongsTo(a, "hermes:s", protocol.SessionInfo{SessionKey: "hermes:s", Tool: "codex"}) {
		t.Fatal("foreign explicit Tool accepted")
	}
}

func TestAggregationCloseWaitsForAdmittedFanoutBeforeDetaching(t *testing.T) {
	a := &registryBoundAgent{registryDeclaredAgent: declaredRegistryAgent(), watchStarted: make(chan struct{})}
	entered, release, publishDone, closeDone := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	m := NewManagerWithOptions(func(protocol.Event) { calls.Add(1); close(entered); <-release }, func() Settings { return Settings{} }, registryOptions(t, a))
	oldSink := a.currentSink()
	go func() { oldSink(protocol.Event{Type: "message", SessionKey: "hermes:s"}); close(publishDone) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("event did not enter sink")
	}
	go func() { m.Close(); close(closeDone) }()
	select {
	case <-closeDone:
		t.Fatal("Close retired an admitted fanout before it finished")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-publishDone:
	case <-time.After(time.Second):
		t.Fatal("fanout failed to finish")
	}
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("Close failed to drain fanout")
	}
	oldSink(protocol.Event{Type: "message", SessionKey: "hermes:s"})
	if calls.Load() != 1 || a.currentSink() != nil {
		t.Fatal("retired sink remained attached")
	}
}
