// Package agentwatchdog detects agent processes that are alive but have stopped
// producing events — a "stalled" state where the CLI process hangs without
// exiting, so the normal process-exit detection never fires.
//
// The watchdog polls the agents.Manager every 60 seconds.  For each session
// whose status is "running" or "waiting_approval", it records the last time it
// saw a fresh event for that session.  If a session stays in that state with
// no new events for longer than StallTimeout (default 10 minutes), the watchdog
// requests an exact-turn interrupt after the silence timeout. The adapter
// publishes a terminal event only when that exact turn is confirmed stopped.
//
// The timeout is deliberately long: Codex and Claude Code can spend several
// minutes on a single tool call (long file writes, slow network fetches),
// and a false positive would kill a genuinely working agent.  10 minutes of
// absolute silence is a strong signal that the process is wedged.
package agentwatchdog

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

const (
	pollInterval    = 60 * time.Second
	stallTimeout    = 10 * time.Minute
	approvalGrace   = 30 * time.Minute // waiting_approval can sit longer; user may be away
	sessionsTimeout = 15 * time.Second
)

// Options configures the watchdog.
type Options struct {
	Manager agents.Manager
	Logger  *log.Logger
	// StallTimeout overrides the default 10-minute silence threshold.
	StallTimeout time.Duration
	// SessionsTimeout bounds one adapter's session enumeration so a stuck
	// runtime cannot permanently stop monitoring the other agents.
	SessionsTimeout time.Duration
	// EmitEvent publishes warnings for an unconfirmed stop. Confirmed terminal
	// events come from the agent adapter and are not repeated by the watchdog.
	// If nil, the watchdog only attempts the exact stop.
	EmitEvent func(protocol.Event)
}

// Watchdog polls the agent manager for stalled sessions.
type Watchdog struct {
	o       Options
	mu      sync.Mutex
	tickMu  sync.Mutex
	started atomic.Bool
	seen    map[string]sessionSeen
	logger  *log.Logger
}

type sessionSeen struct {
	observedAt time.Time
	updatedAt  int64
	activeTurn string
	status     string
	activity   uint64
	inFlight   bool
}

type watchdogSessionLister interface {
	WatchdogSessions(context.Context) ([]protocol.SessionInfo, error)
}

// New creates a Watchdog.  Call Run in a goroutine.
func New(o Options) *Watchdog {
	if o.Logger == nil {
		o.Logger = log.Default()
	}
	if o.StallTimeout <= 0 {
		o.StallTimeout = stallTimeout
	}
	if o.SessionsTimeout <= 0 {
		o.SessionsTimeout = sessionsTimeout
	}
	return &Watchdog{
		o:      o,
		seen:   make(map[string]sessionSeen),
		logger: o.Logger,
	}
}

// Run starts the polling loop.  Blocks until ctx is cancelled.
func (w *Watchdog) Run(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !w.started.CompareAndSwap(false, true) {
		return
	}
	w.run(ctx)
}

// ObserveEvent refreshes the silence timer only for an event that belongs to
// the exact active turn already being watched. Late events from an earlier
// turn must not keep a newer stalled turn alive.
func (w *Watchdog) ObserveEvent(ev protocol.Event) {
	key := ev.SessionKey
	if key == "" && ev.Session != nil {
		key = ev.Session.SessionKey
	}
	if key == "" || !agents.ValidTurnID(ev.TurnID) {
		return
	}
	w.mu.Lock()
	if seen, ok := w.seen[key]; ok && seen.activeTurn == ev.TurnID && (seen.status == "running" || seen.status == "waiting_approval") {
		seen.observedAt = time.Now()
		seen.activity++
		w.seen[key] = seen
	}
	w.mu.Unlock()
}

func (w *Watchdog) run(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	w.logger.Printf("agentwatchdog: started (stall timeout %s)", w.o.StallTimeout)
	w.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			w.logger.Printf("agentwatchdog: stopped")
			return
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}

func (w *Watchdog) tick(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	w.tickMu.Lock()
	defer w.tickMu.Unlock()
	if w.o.Manager == nil {
		return
	}
	now := time.Now()
	type agentSnapshot struct {
		agent    agents.Agent
		sessions []protocol.SessionInfo
		err      error
	}
	snapshots := make([]agentSnapshot, 0)
	// A failed enumeration is an incomplete view, not proof that sessions
	// disappeared. Keep the existing observation baseline until the adapter
	// provides a successful snapshot.
	enumerationFailed := false
	// Take one session snapshot per agent. Sessions() is the required Agent
	// enumeration method; the old optional List() assertion meant the built-in
	// Codex and Claude adapters were silently invisible to the watchdog.
	active := make(map[string]bool)
	for _, a := range w.o.Manager.Agents() {
		if ctx.Err() != nil {
			return
		}
		sessions, err := w.listSessions(ctx, a)
		snapshots = append(snapshots, agentSnapshot{agent: a, sessions: sessions, err: err})
		if err != nil {
			enumerationFailed = true
			continue
		}
		for _, session := range sessions {
			if agents.SessionBelongsTo(a, session.SessionKey, session) && (session.Status == "running" || session.Status == "waiting_approval") {
				active[session.SessionKey] = true
			}
		}
	}

	// Prune only after every adapter produced a complete snapshot. Keeping a
	// stale baseline for one cycle is safe; pruning on an error would make the
	// next successful snapshot look like a brand-new session.
	if !enumerationFailed {
		w.mu.Lock()
		for key := range w.seen {
			if !active[key] {
				delete(w.seen, key)
			}
		}
		w.mu.Unlock()
	}

	// Check each active session for staleness.
	for _, snapshot := range snapshots {
		if snapshot.err != nil {
			continue
		}
		for _, session := range snapshot.sessions {
			if !agents.SessionBelongsTo(snapshot.agent, session.SessionKey, session) || session.Status != "running" && session.Status != "waiting_approval" {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			key := session.SessionKey
			if key == "" {
				continue
			}

			w.mu.Lock()
			lastSeen, exists := w.seen[key]
			// A session without a revision cannot prove that it has been
			// silent. Refresh its observation on every poll until the adapter
			// supplies UpdatedAt, avoiding a false stall after ten minutes.
			if session.UpdatedAt <= 0 {
				w.seen[key] = sessionSeen{observedAt: now, updatedAt: session.UpdatedAt, activeTurn: session.ActiveTurnID, status: session.Status}
				w.mu.Unlock()
				continue
			}
			if !exists || session.UpdatedAt != lastSeen.updatedAt || session.ActiveTurnID != lastSeen.activeTurn || session.Status != lastSeen.status {
				// First time seeing this session; record it.
				w.seen[key] = sessionSeen{observedAt: now, updatedAt: session.UpdatedAt, activeTurn: session.ActiveTurnID, status: session.Status}
				w.mu.Unlock()
				continue
			}
			if lastSeen.inFlight {
				w.mu.Unlock()
				continue
			}
			silentFor := now.Sub(lastSeen.observedAt)

			// waiting_approval gets a longer grace period — the user may be away.
			timeout := w.o.StallTimeout
			if session.Status == "waiting_approval" {
				timeout = approvalGrace
			}

			if silentFor < timeout {
				w.mu.Unlock()
				continue
			}
			// Claim the stop attempt before dropping the lock. Run normally calls
			// tick serially, but tests, future triggers, or a slow adapter must not
			// issue duplicate stops for the same observed turn.
			claimed := lastSeen
			claimed.inFlight = true
			w.seen[key] = claimed
			w.mu.Unlock()

			// Activity may arrive between the timeout check and the stop attempt.
			// Recheck the claim before sending a control request.
			w.mu.Lock()
			current := w.seen[key]
			stillStalled := current.inFlight && current.updatedAt == claimed.updatedAt && current.activeTurn == claimed.activeTurn && current.status == claimed.status && current.activity == claimed.activity
			if !stillStalled && current.inFlight && current.updatedAt == claimed.updatedAt && current.activeTurn == claimed.activeTurn && current.status == claimed.status {
				current.inFlight = false
				w.seen[key] = current
			}
			w.mu.Unlock()
			if !stillStalled {
				continue
			}

			// A status alone is not enough to safely control an agent. Require
			// the exact active turn reported by this snapshot.
			if !agents.ValidTurnID(session.ActiveTurnID) {
				w.logger.Printf("agentwatchdog: session %s stalled without an active turn id", key)
				w.emitNotice(key, "任务长时间无响应，未取得可安全停止的任务轮次")
				w.finishAttempt(key, now, session, claimed.activity)
				continue
			}
			w.logger.Printf("agentwatchdog: session %s turn %s stalled for %s (status=%s), requesting precise interrupt", key, session.ActiveTurnID, silentFor, session.Status)
			result, err := w.interrupt(ctx, snapshot.agent, key, session.ActiveTurnID)
			if err != nil {
				w.logger.Printf("agentwatchdog: interrupt %s failed: %v", key, err)
				// A parent shutdown can cancel the exact-turn request. Do not publish
				// a user-facing failure for work that was abandoned as the watchdog
				// itself was stopping.
				if ctx.Err() == nil {
					w.emitNotice(key, "任务长时间无响应，停止失败，任务状态未确认")
				}
			} else if result.TurnID != session.ActiveTurnID {
				w.emitNotice(key, "任务长时间无响应，停止结果未确认")
			} else {
				switch result.Status {
				case "stopped":
					// A confirmed stop means the adapter observed and emitted the
					// exact turn's terminal event before returning. Do not synthesize
					// a second timeline event here.
				case "already_finished":
					// The exact target completed while the watchdog was acting. Its
					// terminal event is authoritative; never replace it with failure.
				case "requested", "unconfirmed":
					w.emitNotice(key, "任务长时间无响应，已请求停止，但尚未确认任务已停止")
				default:
					w.emitNotice(key, "任务长时间无响应，停止结果未确认")
				}
			}
			w.finishAttempt(key, now, session, claimed.activity)
		}
	}
}

// listSessions returns all known sessions for an agent. Enumeration is read
// only and uses the mandatory Agent.Sessions contract.
func (w *Watchdog) listSessions(ctx context.Context, a agents.Agent) ([]protocol.SessionInfo, error) {
	if a == nil {
		return nil, errors.New("agent unavailable")
	}
	callCtx, cancel := context.WithTimeout(ctx, w.o.SessionsTimeout)
	defer cancel()
	var sessions []protocol.SessionInfo
	var err error
	if live, ok := a.(watchdogSessionLister); ok {
		sessions, err = live.WatchdogSessions(callCtx)
	} else {
		sessions, err = a.Sessions(callCtx)
	}
	if err == nil && callCtx.Err() != nil {
		err = callCtx.Err()
	}
	return sessions, err
}

// interrupt kills the agent process for the given session key.
func (w *Watchdog) finishAttempt(key string, now time.Time, session protocol.SessionInfo, activity uint64) {
	w.mu.Lock()
	if current, ok := w.seen[key]; ok && current.updatedAt == session.UpdatedAt && current.activeTurn == session.ActiveTurnID && current.status == session.Status {
		if current.activity == activity {
			current.observedAt = now
		}
		current.inFlight = false
		w.seen[key] = current
	}
	w.mu.Unlock()
}

func (w *Watchdog) emitNotice(sessionKey, message string) {
	if w.o.EmitEvent != nil {
		w.o.EmitEvent(protocol.Event{SessionKey: sessionKey, Type: "notice", Level: "warn", Text: message})
	}
}

func (w *Watchdog) interrupt(parent context.Context, a agents.Agent, sessionKey, turnID string) (protocol.InterruptResult, error) {
	if a == nil || !agents.SessionBelongsTo(a, sessionKey, protocol.SessionInfo{SessionKey: sessionKey}) {
		return protocol.InterruptResult{}, agents.ErrTurnControlStale
	}
	id := strings.TrimPrefix(sessionKey, a.ID()+":")
	precise, ok := a.(agents.PreciseInterrupter)
	if !ok {
		return protocol.InterruptResult{}, agents.ErrExpectedTurnRequired
	}
	if parent.Err() != nil {
		return protocol.InterruptResult{}, parent.Err()
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	return precise.InterruptTurn(ctx, id, turnID, config.NewUUID())
}
