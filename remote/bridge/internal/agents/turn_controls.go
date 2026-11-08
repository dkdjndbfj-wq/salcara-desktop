package agents

import (
	"context"
	"errors"
	"strings"
	"unicode"

	"salcara/bridge/internal/protocol"
)

var ErrExpectedTurnRequired = errors.New("turn_control_target_required: 请刷新并指定要操作的任务轮次")
var ErrTurnControlStale = errors.New("turn_control_stale: 任务轮次已经变化，消息未发送，请刷新后重试")

// Controls are opt-in contracts. The mandatory legacy Interrupt method never
// authorizes an ID-less stop; adapters and old clients must fail closed.
type PreciseInterrupter interface {
	InterruptTurn(context.Context, string, string, string) (protocol.InterruptResult, error)
}
type TerminalContinuer interface {
	ContinueWithOptions(context.Context, string, string, string, string, TurnOptions) (protocol.ContinueResult, error)
}

// Steer appends text to an existing, owned active turn. It cannot change its
// model, permissions or executor, and never falls back to starting a new turn.
type ActiveTurnSteerer interface {
	SteerTurn(context.Context, string, string, string, string, func() error) (protocol.SteerResult, error)
}

func ValidTurnID(id string) bool {
	if id == "" || len(id) > 256 || strings.TrimSpace(id) != id {
		return false
	}
	for _, r := range id {
		if unicode.IsControl(r) || unicode.IsSpace(r) || unicode.In(r, unicode.Cf) {
			return false
		}
	}
	return true
}

func continueOptions(expected, operation string, opts TurnOptions) (TurnOptions, error) {
	if !ValidTurnID(expected) {
		return opts, ErrExpectedTurnRequired
	}
	// A stable command identity is mandatory even without echo support. Receipt
	// replay is handled by the transport; terminal CAS also rejects a new ID.
	if !validControlOperationID(operation) {
		return opts, errors.New("turn_control_operation_required: 操作标识无效")
	}
	opts.ExpectedTerminalTurnID, opts.ControlOperationID = expected, operation
	return opts, nil
}

func validControlOperationID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, r := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return false
			}
			continue
		}
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

// ValidControlOperationID is the wire-level validation used by the dispatch
// layer as well as adapters.  Keeping this check in one place prevents an old
// client from turning an arbitrary string into a replay/idempotency key.
func ValidControlOperationID(id string) bool { return validControlOperationID(id) }
