package agents

import (
	"context"

	"salcara/bridge/internal/protocol"
)

type contextLifecycleGuardKey struct{}

// WithContextLifecycleGuard supplies a live authorization check for an
// admitted native operation after its HTTP command context has ended. The
// dispatcher must detach only request cancellation, retaining source epochs,
// phone identity and current permission checks. Phone JSON cannot set this key.
func WithContextLifecycleGuard(ctx context.Context, validate func() error) context.Context {
	return context.WithValue(ctx, contextLifecycleGuardKey{}, validate)
}

func ContextLifecycleGuard(ctx context.Context) func() error {
	validate, _ := ctx.Value(contextLifecycleGuardKey{}).(func() error)
	return validate
}

// ContextProvider only observes an already owned runtime. Implementations may
// use a documented local summary control, never start/resume a worker or issue
// a model/token-count request to manufacture a reading. validate fences both
// the control write and the returned snapshot against source/permission changes.
type ContextProvider interface {
	SessionContext(ctx context.Context, id string, validate func() error) (protocol.SessionContext, error)
}

// ContextCompactor dispatches one native operation under a runtime revision
// CAS. Implementations retain the operation receipt and exclude other sends
// until a native terminal signal, rather than treating RPC acceptance as done.
type ContextCompactor interface {
	CompactContext(ctx context.Context, id, expectedRevision, operationID string, validate func() error) (protocol.SessionContext, error)
}
