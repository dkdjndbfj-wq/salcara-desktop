package agents

import (
	"context"
	"salcara/bridge/internal/protocol"
)

// LatestTurnReverter is opt-in and never grants control over external workers.
type LatestTurnReverter interface {
	RevertLatestTurn(context.Context, string, string, string, string, func() error) (protocol.LatestTurnRevertResult, error)
}

// A durable replay may reconcile a stored intent but must never admit a write.
type LatestTurnRevertRecoverer interface {
	ReconcileLatestTurnRevert(context.Context, string, string, string, string, func() error) (protocol.LatestTurnRevertResult, error)
}
