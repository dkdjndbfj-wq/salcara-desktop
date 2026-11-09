package launcher

import "context"

type managedLaunchKey struct{}

// WithManagedLaunch selects the stop-before-config contract. Native launchers
// must preserve a durable ownership receipt, independent of the HTTP request.
func WithManagedLaunch(ctx context.Context) context.Context {
	return context.WithValue(ctx, managedLaunchKey{}, true)
}

func managedLaunch(ctx context.Context) bool { return ctx.Value(managedLaunchKey{}) == true }

// StopManagedCLI stops only the owned CLI trees sharing this configuration
// family. Missing or uncertain ownership is an error, never an idle proof.
func StopManagedCLI(ctx context.Context, plan Plan) error { return stopManagedCLI(ctx, plan) }
