package agents

import (
	"context"
	"errors"

	"salcara/bridge/internal/protocol"
)

func (a *codexAgent) reserveCodexContinuation(c *rpcConn, id, expected string) error {
	// Approval registry checks are intentionally outside a.mu (its callbacks
	// may inspect agent state). Native turn/start remains the final host CAS.
	if a.aps.countFor("codex:"+id) != 0 {
		return ErrTurnControlStale
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	t := a.threads[id]
	if a.closed || a.conn != c || t == nil || t.statusUnconfirmed || t.turnID != "" ||
		t.info.ActiveTurnID != "" || t.info.LastTerminalTurnID != expected || t.continuedTerminal == expected || t.childOverflow {
		return ErrTurnControlStale
	}
	for _, status := range t.childStates {
		if status == "running" || status == "pendingInit" {
			return ErrTurnControlStale
		}
	}
	t.continuedTerminal = expected
	return nil
}

func (a *codexAgent) ContinueWithOptions(ctx context.Context, id, text, expected, operation string, opts TurnOptions) (protocol.ContinueResult, error) {
	opts, err := continueOptions(expected, operation, opts)
	if err != nil {
		return protocol.ContinueResult{}, err
	}
	if err = a.SendWithOptions(ctx, id, text, opts); err != nil {
		return protocol.ContinueResult{}, err
	}
	return protocol.ContinueResult{Accepted: true, SessionKey: "codex:" + id, PreviousTurnID: expected, OperationID: operation}, nil
}

func (a *codexAgent) InterruptTurn(ctx context.Context, id, expected, operation string) (protocol.InterruptResult, error) {
	out := protocol.InterruptResult{TurnID: expected}
	if !ValidTurnID(expected) {
		return out, ErrExpectedTurnRequired
	}
	if !ValidControlOperationID(operation) {
		return out, errors.New("turn_control_operation_required: 操作标识无效")
	}
	a.mu.Lock()
	t := a.threads[id]
	a.mu.Unlock()
	if t == nil {
		return out, ErrTurnControlStale
	}
	t.controlMu.Lock()
	defer t.controlMu.Unlock()
	a.mu.Lock()
	c := a.conn
	if t.info.LastTerminalTurnID == expected && t.turnID != expected {
		a.mu.Unlock()
		out.Status = "already_finished"
		return out, nil
	}
	if a.closed || !t.loaded || c == nil || t.turnID != expected {
		a.mu.Unlock()
		return out, ErrTurnControlStale
	}
	a.mu.Unlock()
	// Do not cancel every approval by session before this RPC: a completion
	// racing the command could otherwise cancel a newer turn's question.
	err := c.Call(ctx, "turn/interrupt", map[string]any{"threadId": id, "turnId": expected, "operationId": operation}, nil)
	if err != nil {
		out.Status = "unconfirmed"
		if protocol.ExecutionErrorCode(err) == "command_delivery_uncertain" {
			return out, nil
		}
		return out, err
	}
	a.mu.Lock()
	stopped := a.conn == c && t.info.LastTerminalTurnID == expected && t.turnID != expected
	a.mu.Unlock()
	if stopped {
		out.Status = "stopped"
	} else {
		out.Status = "requested"
	}
	return out, nil
}

var _ PreciseInterrupter = (*codexAgent)(nil)
var _ TerminalContinuer = (*codexAgent)(nil)
