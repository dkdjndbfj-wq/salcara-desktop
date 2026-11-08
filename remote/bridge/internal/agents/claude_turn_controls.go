package agents

import (
	"context"
	"errors"

	"salcara/bridge/internal/protocol"
)

// ContinueWithOptions is the Claude Code equivalent of Codex's same-thread
// continuation. It never adopts an external transcript: the terminal ID must
// be the one published by this Bridge, and an external session needs the
// process-local completion lease before it can be resumed.
func (a *claudeAgent) ContinueWithOptions(ctx context.Context, id, text, expected, operation string, opts TurnOptions) (protocol.ContinueResult, error) {
	if !ValidTurnID(expected) {
		return protocol.ContinueResult{}, ErrExpectedTurnRequired
	}
	if !ValidControlOperationID(operation) {
		return protocol.ContinueResult{}, errors.New("turn_control_operation_required: 操作标识无效")
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return protocol.ContinueResult{}, errors.New("桥接程序正在退出")
	}
	p := a.procs[id]
	info, hasLive := a.live[id]
	if !hasLive {
		a.mu.Unlock()
		return protocol.ContinueResult{}, ErrTurnControlStale
	}
	if info.ActiveTurnID != "" || info.LastTerminalTurnID != expected {
		a.mu.Unlock()
		return protocol.ContinueResult{}, ErrTurnControlStale
	}
	if a.continuedTerminal == nil {
		a.continuedTerminal = map[string]string{}
	}
	if a.continuedTerminal[id] == expected {
		a.mu.Unlock()
		return protocol.ContinueResult{}, ErrTurnControlStale
	}
	if p != nil {
		p.mu.Lock()
		busy := p.turns > 0 || len(p.turnIDs) > 0 || p.activeSubtasks.Load() || p.contextCompacting || p.stopped
		p.mu.Unlock()
		if busy {
			a.mu.Unlock()
			return protocol.ContinueResult{}, ErrTurnControlStale
		}
	} else if !a.hist.ownedCompletion(id) {
		a.mu.Unlock()
		return protocol.ContinueResult{}, errClaudeExternalStateUnknown
	}
	a.continuedTerminal[id] = expected
	a.mu.Unlock()

	// SendWithOptions either writes to the still-owned idle worker or creates a
	// new Bridge worker with --resume after the exact completion lease check.
	// Keep the caller's observed terminal through image reads, worker startup,
	// and writer contention. The early reservation is not the final CAS.
	opts.ExpectedTerminalTurnID = expected
	if err := a.SendWithOptions(ctx, id, text, opts); err != nil {
		a.mu.Lock()
		if a.continuedTerminal[id] == expected {
			delete(a.continuedTerminal, id)
		}
		a.mu.Unlock()
		return protocol.ContinueResult{}, err
	}
	return protocol.ContinueResult{Accepted: true, SessionKey: "claude:" + id, PreviousTurnID: expected, OperationID: operation}, nil
}

var _ TerminalContinuer = (*claudeAgent)(nil)
