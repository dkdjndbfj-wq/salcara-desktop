package agents

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"

	"salcara/bridge/internal/protocol"
)

type codexSteerReceipt struct {
	digest [32]byte
	result protocol.SteerResult
	err    error
}

// SteerTurn deliberately never calls ensure/resume/start. A history snapshot
// does not authorize taking over a worker launched by another application.
func (a *codexAgent) SteerTurn(ctx context.Context, id, text, expected, operation string, validate func() error) (protocol.SteerResult, error) {
	var out protocol.SteerResult
	if !ValidTurnID(expected) {
		return out, ErrExpectedTurnRequired
	}
	if !ValidControlOperationID(operation) {
		return out, errors.New("turn_control_operation_required: 操作标识无效")
	}
	if strings.TrimSpace(text) == "" || len(text) > 1<<20 {
		return out, errors.New("请输入有效的引导内容")
	}
	a.mu.Lock()
	t := a.threads[id]
	a.mu.Unlock()
	if t == nil {
		return out, ErrTurnControlStale
	}
	t.controlMu.Lock()
	defer t.controlMu.Unlock()
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if validate != nil {
		if err := validate(); err != nil {
			return out, err
		}
	}
	digest := sha256.Sum256([]byte(expected + "\x00" + text))
	if old, ok := t.steerReceipts[operation]; ok {
		if old.digest != digest {
			return out, errors.New("turn_control_operation_conflict: 操作内容已改变")
		}
		return old.result, old.err
	}
	s, err := a.requireRelay(ctx)
	if err != nil {
		return out, err
	}
	a.mu.Lock()
	c, cwd := a.conn, t.info.Cwd
	a.mu.Unlock()
	guard := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if validate != nil {
			if err := validate(); err != nil {
				return err
			}
		}
		if a.aps.countFor("codex:"+id) != 0 {
			return ErrTurnControlStale
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.closed || c == nil || a.conn != c || a.connSig != codexSig(s) || a.threads[id] != t || !t.loaded ||
			t.statusUnconfirmed || t.permissionUnconfirmed || t.turnID != expected || t.info.Status != "running" {
			return ErrTurnControlStale
		}
		return nil
	}
	if err := guard(); err != nil {
		return out, err
	}
	if t.steerTurn != expected {
		t.steerTurn, t.steerReceipts = expected, make(map[string]codexSteerReceipt)
	}
	// Never evict an earlier operation from the same turn: that could turn a
	// retransmitted command into duplicate input. The next turn has a new CAS.
	if len(t.steerReceipts) >= 256 {
		return out, errors.New("当前轮次的引导次数已达上限")
	}
	if err := validateCodexEffectiveProviderAt(ctx, c, s, cwd); err != nil {
		return out, err
	}
	var response struct {
		TurnID string `json:"turnId"`
	}
	dispatched := false
	err = c.callGuarded(ctx, "turn/steer", map[string]any{
		"threadId": id, "expectedTurnId": expected,
		"input": []map[string]any{{"type": "text", "text": text}},
	}, &response, func() error {
		if err := guard(); err != nil {
			return err
		}
		dispatched = true
		return nil
	})
	if err == nil {
		if response.TurnID != expected {
			err = errRPCDeliveryInterrupted
		} else {
			out = protocol.SteerResult{Accepted: true, SessionKey: "codex:" + id, TurnID: expected, OperationID: operation}
		}
	}
	// Hub durable receipts own reconnect/restart recovery. This extra per-turn
	// fence also blocks duplicate delivery within the current worker lifetime,
	// including uncertain writes; never retry those by making a new operation.
	if dispatched {
		t.steerReceipts[operation] = codexSteerReceipt{digest: digest, result: out, err: err}
	}
	return out, err
}

var _ ActiveTurnSteerer = (*codexAgent)(nil)
