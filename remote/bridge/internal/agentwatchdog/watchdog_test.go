package agentwatchdog

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/protocol"
)

type watchdogFixtureAgent struct {
	mu              sync.Mutex
	sessions        []protocol.SessionInfo
	sessionsErr     error
	sessionsWait    bool
	sessionCalls    int
	interrupts      []string
	precise         []string
	preciseState    string
	cancelOnPrecise context.CancelFunc
}

func (a *watchdogFixtureAgent) ID() string   { return "codex" }
func (a *watchdogFixtureAgent) Name() string { return "Codex" }
func (a *watchdogFixtureAgent) Detect(context.Context) protocol.Tool {
	return protocol.Tool{ID: "codex", Name: "Codex", Available: true}
}
func (a *watchdogFixtureAgent) Sessions(ctx context.Context) ([]protocol.SessionInfo, error) {
	a.mu.Lock()
	a.sessionCalls++
	sessions := append([]protocol.SessionInfo(nil), a.sessions...)
	err := a.sessionsErr
	wait := a.sessionsWait
	a.mu.Unlock()
	if wait {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return sessions, err
}
func (*watchdogFixtureAgent) Open(context.Context, string) (protocol.SessionInfo, []protocol.Event, error) {
	return protocol.SessionInfo{}, nil, nil
}
func (*watchdogFixtureAgent) Start(context.Context, string, string, string, string) (string, error) {
	return "", nil
}
func (*watchdogFixtureAgent) Send(context.Context, string, string) error { return nil }
func (a *watchdogFixtureAgent) Interrupt(_ context.Context, id string) error {
	a.mu.Lock()
	a.interrupts = append(a.interrupts, id)
	a.mu.Unlock()
	return nil
}
func (a *watchdogFixtureAgent) InterruptTurn(ctx context.Context, id, turnID, operation string) (protocol.InterruptResult, error) {
	a.mu.Lock()
	a.precise = append(a.precise, id+"/"+turnID+"/"+operation)
	state := a.preciseState
	cancel := a.cancelOnPrecise
	a.mu.Unlock()
	if cancel != nil {
		cancel()
		return protocol.InterruptResult{Status: "unconfirmed", TurnID: turnID}, ctx.Err()
	}
	if state == "" {
		state = "stopped"
	}
	return protocol.InterruptResult{Status: state, TurnID: turnID}, nil
}
func (*watchdogFixtureAgent) Respond(string, string, string) bool { return false }
func (*watchdogFixtureAgent) Models(context.Context) []string     { return nil }
func (*watchdogFixtureAgent) Close()                              {}

type watchdogFixtureManager struct{ list []agents.Agent }

func (m *watchdogFixtureManager) Agents() []agents.Agent {
	return append([]agents.Agent(nil), m.list...)
}
func (m *watchdogFixtureManager) Get(string) agents.Agent  { return nil }
func (*watchdogFixtureManager) Watch(context.Context)      {}
func (*watchdogFixtureManager) LocalHandler() http.Handler { return http.NotFoundHandler() }
func (*watchdogFixtureManager) Close()                     {}

func TestWatchdogUsesAgentSessionsAndDoesNotDuplicateConfirmedTerminalEvent(t *testing.T) {
	stamp := time.Now().UnixMilli()
	agent := &watchdogFixtureAgent{sessions: []protocol.SessionInfo{{
		SessionKey: "codex:run-1", Status: "running", ActiveTurnID: "turn-1", UpdatedAt: stamp,
	}}}
	var events []protocol.Event
	watchdog := New(Options{
		Manager:      &watchdogFixtureManager{list: []agents.Agent{agent}},
		Logger:       log.New(io.Discard, "", 0),
		StallTimeout: time.Minute,
		EmitEvent:    func(event protocol.Event) { events = append(events, event) },
	})

	watchdog.tick(context.Background()) // first observation establishes baseline
	watchdog.mu.Lock()
	watchdog.seen["codex:run-1"] = sessionSeen{observedAt: time.Now().Add(-2 * time.Minute), updatedAt: stamp, activeTurn: "turn-1", status: "running"}
	watchdog.mu.Unlock()
	watchdog.tick(context.Background())

	if got := agent.precise; len(got) != 1 || !strings.HasPrefix(got[0], "run-1/turn-1/") || !agents.ValidControlOperationID(strings.TrimPrefix(got[0], "run-1/turn-1/")) {
		t.Fatalf("precise interrupts = %v, want the raw session ID, exact turn ID and a UUID operation", got)
	}
	if len(events) != 0 {
		t.Fatalf("watchdog duplicated the adapter's confirmed terminal event: %+v", events)
	}
	if agent.sessionCalls != 2 {
		t.Fatalf("Sessions called %d times, want one snapshot per tick", agent.sessionCalls)
	}
}

func TestWatchdogCurrentTurnActivityResetsSilence(t *testing.T) {
	stamp := time.Now().UnixMilli()
	agent := &watchdogFixtureAgent{sessions: []protocol.SessionInfo{{
		SessionKey: "codex:run-active", Status: "running", ActiveTurnID: "turn-active", UpdatedAt: stamp,
	}}}
	watchdog := New(Options{Manager: &watchdogFixtureManager{list: []agents.Agent{agent}}, Logger: log.New(io.Discard, "", 0), StallTimeout: time.Minute})
	watchdog.tick(context.Background())
	watchdog.mu.Lock()
	seen := watchdog.seen["codex:run-active"]
	seen.observedAt = time.Now().Add(-2 * time.Minute)
	watchdog.seen["codex:run-active"] = seen
	watchdog.mu.Unlock()

	watchdog.ObserveEvent(protocol.Event{SessionKey: "codex:run-active", TurnID: "turn-active", Type: "message"})
	watchdog.tick(context.Background())

	if len(agent.precise) != 0 {
		t.Fatalf("fresh activity from the active turn was stopped: %v", agent.precise)
	}
}

func TestWatchdogIgnoresLateActivityFromPreviousTurn(t *testing.T) {
	stamp := time.Now().UnixMilli()
	agent := &watchdogFixtureAgent{sessions: []protocol.SessionInfo{{
		SessionKey: "codex:run-successor", Status: "running", ActiveTurnID: "turn-new", UpdatedAt: stamp,
	}}}
	watchdog := New(Options{Manager: &watchdogFixtureManager{list: []agents.Agent{agent}}, Logger: log.New(io.Discard, "", 0), StallTimeout: time.Minute})
	watchdog.tick(context.Background())
	watchdog.mu.Lock()
	seen := watchdog.seen["codex:run-successor"]
	seen.observedAt = time.Now().Add(-2 * time.Minute)
	watchdog.seen["codex:run-successor"] = seen
	watchdog.mu.Unlock()

	watchdog.ObserveEvent(protocol.Event{SessionKey: "codex:run-successor", TurnID: "turn-old", Type: "message"})
	watchdog.tick(context.Background())

	if len(agent.precise) != 1 || !strings.HasPrefix(agent.precise[0], "run-successor/turn-new/") {
		t.Fatalf("late event for the prior turn hid a stalled successor: %v", agent.precise)
	}
}

func TestWatchdogBoundsSessionEnumerationAndKeepsBaseline(t *testing.T) {
	stamp := time.Now().UnixMilli()
	agent := &watchdogFixtureAgent{sessions: []protocol.SessionInfo{{
		SessionKey: "codex:run-timeout", Status: "running", ActiveTurnID: "turn-timeout", UpdatedAt: stamp,
	}}, sessionsWait: true}
	watchdog := New(Options{
		Manager:         &watchdogFixtureManager{list: []agents.Agent{agent}},
		Logger:          log.New(io.Discard, "", 0),
		StallTimeout:    time.Minute,
		SessionsTimeout: 10 * time.Millisecond,
	})
	baseline := sessionSeen{observedAt: time.Now().Add(-2 * time.Minute), updatedAt: stamp, activeTurn: "turn-timeout", status: "running"}
	watchdog.seen["codex:run-timeout"] = baseline

	started := time.Now()
	watchdog.tick(context.Background())
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("session enumeration exceeded its deadline: %s", elapsed)
	}
	if len(agent.precise) != 0 {
		t.Fatalf("timed-out snapshot triggered an interrupt: %v", agent.precise)
	}
	watchdog.mu.Lock()
	seen, exists := watchdog.seen["codex:run-timeout"]
	watchdog.mu.Unlock()
	if !exists || !seen.observedAt.Equal(baseline.observedAt) {
		t.Fatalf("timed-out snapshot erased or reset the baseline: exists=%t seen=%+v", exists, seen)
	}
}

func TestWatchdogDoesNotUseLegacyInterruptOrClaimStoppedWithoutConfirmation(t *testing.T) {
	stamp := time.Now().UnixMilli()
	agent := &watchdogFixtureAgent{sessions: []protocol.SessionInfo{{SessionKey: "codex:run-3", Status: "running", ActiveTurnID: "turn-3", UpdatedAt: stamp}}, preciseState: "unconfirmed"}
	var events []protocol.Event
	watchdog := New(Options{Manager: &watchdogFixtureManager{list: []agents.Agent{agent}}, Logger: log.New(io.Discard, "", 0), StallTimeout: time.Minute, EmitEvent: func(event protocol.Event) { events = append(events, event) }})
	watchdog.tick(context.Background())
	watchdog.mu.Lock()
	watchdog.seen["codex:run-3"] = sessionSeen{observedAt: time.Now().Add(-2 * time.Minute), updatedAt: stamp, activeTurn: "turn-3", status: "running"}
	watchdog.mu.Unlock()
	watchdog.tick(context.Background())
	if len(agent.interrupts) != 0 || len(agent.precise) != 1 {
		t.Fatalf("watchdog used an unsafe interrupt path: legacy=%v precise=%v", agent.interrupts, agent.precise)
	}
	if len(events) != 1 || events[0].Type != "notice" || events[0].Level != "warn" || events[0].Status != "" {
		t.Fatalf("unconfirmed stop emitted an incorrect terminal event: %+v", events)
	}
}

func TestWatchdogRequiresActiveTurnID(t *testing.T) {
	stamp := time.Now().UnixMilli()
	agent := &watchdogFixtureAgent{sessions: []protocol.SessionInfo{{SessionKey: "codex:run-4", Status: "running", UpdatedAt: stamp}}}
	var events []protocol.Event
	watchdog := New(Options{Manager: &watchdogFixtureManager{list: []agents.Agent{agent}}, Logger: log.New(io.Discard, "", 0), StallTimeout: time.Minute, EmitEvent: func(event protocol.Event) { events = append(events, event) }})
	watchdog.tick(context.Background())
	watchdog.mu.Lock()
	watchdog.seen["codex:run-4"] = sessionSeen{observedAt: time.Now().Add(-2 * time.Minute), updatedAt: stamp, status: "running"}
	watchdog.mu.Unlock()
	watchdog.tick(context.Background())
	if len(agent.precise) != 0 || len(agent.interrupts) != 0 || len(events) != 1 || events[0].Type != "notice" {
		t.Fatalf("missing active turn was not handled safely: precise=%v legacy=%v events=%+v", agent.precise, agent.interrupts, events)
	}
}

func TestWatchdogSuppressesStopFailureNoticeDuringShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stamp := time.Now().UnixMilli()
	agent := &watchdogFixtureAgent{
		sessions:        []protocol.SessionInfo{{SessionKey: "codex:run-shutdown", Status: "running", ActiveTurnID: "turn-shutdown", UpdatedAt: stamp}},
		cancelOnPrecise: cancel,
	}
	var events []protocol.Event
	watchdog := New(Options{Manager: &watchdogFixtureManager{list: []agents.Agent{agent}}, Logger: log.New(io.Discard, "", 0), StallTimeout: time.Minute, EmitEvent: func(event protocol.Event) { events = append(events, event) }})
	watchdog.tick(ctx)
	watchdog.mu.Lock()
	watchdog.seen["codex:run-shutdown"] = sessionSeen{observedAt: time.Now().Add(-2 * time.Minute), updatedAt: stamp, activeTurn: "turn-shutdown", status: "running"}
	watchdog.mu.Unlock()
	watchdog.tick(ctx)
	if len(events) != 0 {
		t.Fatalf("shutdown cancellation emitted a misleading user-facing result: %+v", events)
	}
}

func TestWatchdogDoesNotTreatMissingRevisionAsStalled(t *testing.T) {
	agent := &watchdogFixtureAgent{sessions: []protocol.SessionInfo{{
		SessionKey: "codex:run-2", Status: "running", UpdatedAt: 0,
	}}}
	watchdog := New(Options{
		Manager:      &watchdogFixtureManager{list: []agents.Agent{agent}},
		Logger:       log.New(io.Discard, "", 0),
		StallTimeout: time.Second,
	})

	watchdog.tick(context.Background())
	watchdog.mu.Lock()
	watchdog.seen["codex:run-2"] = sessionSeen{observedAt: time.Now().Add(-time.Hour)}
	watchdog.mu.Unlock()
	watchdog.tick(context.Background())

	if len(agent.interrupts) != 0 {
		t.Fatalf("missing UpdatedAt triggered an interrupt: %v", agent.interrupts)
	}
	watchdog.mu.Lock()
	seen := watchdog.seen["codex:run-2"]
	watchdog.mu.Unlock()
	if age := time.Since(seen.observedAt); age > time.Second {
		t.Fatalf("missing revision did not refresh observation, age=%s", age)
	}
}

func TestWatchdogRetainsObservationOnSessionSnapshotError(t *testing.T) {
	stamp := time.Now().UnixMilli()
	agent := &watchdogFixtureAgent{
		sessions:    []protocol.SessionInfo{{SessionKey: "codex:still-running", Status: "running", ActiveTurnID: "turn-keep", UpdatedAt: stamp}},
		sessionsErr: errors.New("fixture unavailable"),
	}
	watchdog := New(Options{Manager: &watchdogFixtureManager{list: []agents.Agent{agent}}, Logger: log.New(io.Discard, "", 0)})
	baseline := sessionSeen{observedAt: time.Now().Add(-time.Hour), updatedAt: stamp, activeTurn: "turn-keep", status: "running"}
	watchdog.seen["codex:still-running"] = baseline

	watchdog.tick(context.Background())

	if len(agent.interrupts) != 0 {
		t.Fatalf("session enumeration error triggered interrupt: %v", agent.interrupts)
	}
	watchdog.mu.Lock()
	seen, exists := watchdog.seen["codex:still-running"]
	watchdog.mu.Unlock()
	if !exists || !seen.observedAt.Equal(baseline.observedAt) || seen.activeTurn != baseline.activeTurn {
		t.Fatalf("failed snapshot erased or rewrote the live session baseline: exists=%t seen=%+v", exists, seen)
	}

	// Once enumeration recovers, the retained stale baseline should still make
	// the original stalled turn eligible for intervention instead of restarting
	// its entire timeout window.
	agent.mu.Lock()
	agent.sessionsErr = nil
	agent.mu.Unlock()
	watchdog.tick(context.Background())
	if len(agent.precise) != 1 || !strings.HasPrefix(agent.precise[0], "still-running/turn-keep/") {
		t.Fatalf("recovered snapshot lost the original stalled turn: precise=%v", agent.precise)
	}
}
